package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
)

// Sourcer 是按优先级串行寻源的编排器，实现流程图第 2~4 步：
//
//	两项请求 → 先遍历【综合源】列表，两者皆得即停
//	         → 综合源用尽仍缺 → 按维度分别补齐（过滤本次已请求过的源）
//	单项请求 → 直接遍历该维度的优先级列表，命中即停
//
// 它对 orchestrator 仍是一个 port.UpstreamPort。与被它取代的并发聚合器相比有三个
// 本质差别：
//
//  1. 串行 + 短路。命中即停，能省下后面所有源的钱；代价是时延从 max(各源) 变成
//     sum(命中前各源)，故有总时延预算 budget 兜底（预算耗尽不再尝试下一个源，
//     并在轨迹里记原因，绝不把下游拖到超时）。
//  2. 判定下沉到维度。一次调用可能「发票查得、税务查无」，因此完整度是 DimSet
//     的累积，而不是单一业务码。
//  3. 逐源留痕。每个源（含未调用的）都产出一条 model.SourceCall，带自己的上游
//     订单号/请求号与成本——不再像聚合器那样把 N 组标识归并成一对丢掉其余。
type Sourcer struct {
	sources  []Source
	combined []Source // 能一次同时提供发票与税务的源（当前配置下可能为空）
	invoice  []Source
	tax      []Source
	budget   time.Duration
	calls    int // 全部逻辑源的上游调用总数（决定 Requery 能否直通）
}

// Source 是寻源优先级列表里的一个候选：一个**逻辑源**，内含 1..N 次上游调用。
// 逻辑源是必要的：证通的发票聚合 part1/part2 是互补字段（各出一半列表），必须同属
// 一个逻辑源一起调用——否则「命中即停」会在 part1 命中后跳过 part2，下游拿到的
// 字段比改造前更少。
type Source struct {
	Name     string       // 逻辑源名：优先级列表的单位，也是「已请求过」去重的键
	Provider string       // 上游 kind: entcredit/salesdata
	Provides model.DimSet // 该源能提供的维度；两维皆有 = 综合源
	Priority int          // 越小越先调用
	Optional bool         // scope=basic 时跳过（如源5 销项数据）
	Calls    []Call       // 内含的上游调用（互补，需全部调用）
}

// Call 是逻辑源内的一次上游调用，1:1 对应配置里的一个 upstreams 条目。
type Call struct {
	Label   string       // 契约段名 invoice1/invoice2/tax1/tax2/sales
	Dims    model.DimSet // 该次调用覆盖的维度
	CostFen int64        // 该次调用的上游单价（分）
	CostOn  string       // hit=仅查得计成本 / call=调用即计成本（缺省 hit）
	Port    port.UpstreamPort
}

// 成本口径：不同上游的商务条款不同（有的按调用收，有的只按命中收），故按源可配。
const (
	CostOnHit  = "hit"
	CostOnCall = "call"
)

// defaultBudget 是一次请求内全部上游调用的总时延预算。串行寻源最坏情况是
// 逐个源串起来，必须有个总闸门，否则下游先超时。
const defaultBudget = 9 * time.Second

// NewSourcer 校验并构建寻源器：按 (Priority, 成本, 配置顺序) 稳定排序，再按能力
// 切成三个优先级列表。未显式给 priority 时全为 0，排序自然退化为「价格由低到高」。
func NewSourcer(sources []Source, budget time.Duration) (*Sourcer, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("upstream sourcer: 至少需要一个源")
	}
	for i := range sources {
		if sources[i].Name == "" {
			sources[i].Name = fmt.Sprintf("source%d", i+1)
		}
		if len(sources[i].Calls) == 0 {
			return nil, fmt.Errorf("upstream sourcer: 源 %q 没有任何上游调用", sources[i].Name)
		}
		if sources[i].Provides.Empty() {
			return nil, fmt.Errorf("upstream sourcer: 源 %q 未声明 provides（发票/税务）", sources[i].Name)
		}
		for j := range sources[i].Calls {
			if sources[i].Calls[j].Port == nil {
				return nil, fmt.Errorf("upstream sourcer: 源 %q 的调用 %d 未初始化", sources[i].Name, j)
			}
			if sources[i].Calls[j].Label == "" {
				sources[i].Calls[j].Label = fmt.Sprintf("%s_%d", sources[i].Name, j+1)
			}
		}
	}
	if budget <= 0 {
		budget = defaultBudget
	}

	ordered := make([]Source, len(sources))
	copy(ordered, sources)
	idx := make(map[string]int, len(sources))
	for i, s := range sources {
		idx[s.Name] = i
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].Priority != ordered[b].Priority {
			return ordered[a].Priority < ordered[b].Priority
		}
		ca, cb := ordered[a].cost(), ordered[b].cost()
		if ca != cb {
			return ca < cb
		}
		return idx[ordered[a].Name] < idx[ordered[b].Name]
	})

	s := &Sourcer{sources: ordered, budget: budget}
	for _, src := range ordered {
		s.calls += len(src.Calls)
		if src.Provides.Both() {
			s.combined = append(s.combined, src)
		}
		if src.Provides.Invoice {
			s.invoice = append(s.invoice, src)
		}
		if src.Provides.Tax {
			s.tax = append(s.tax, src)
		}
	}
	return s, nil
}

// cost 是逻辑源本次寻源的总成本（内含各上游调用单价之和）。
func (s Source) cost() int64 {
	var total int64
	for _, c := range s.Calls {
		total += c.CostFen
	}
	return total
}

// Active 返回源数量描述，供健康检查/日志使用。
func (s *Sourcer) Active() string {
	return fmt.Sprintf("sourcer(%d源/%d调用, 综合源%d)", len(s.sources), s.calls, len(s.combined))
}

func (s *Sourcer) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	want := req.Want
	if want.Empty() {
		want = model.AllDims() // 老下游不带 dataType：按两项都要处理
	}
	tr := newTrace(s.budget)

	// 2b. 综合寻源：两项都要时先试能一次拿全的源。
	if want.Both() {
		s.traverse(ctx, req, tr, s.combined, want)
	}
	// 2a/3. 单项寻源 = 缺项补齐：同一段代码，候选集是「该维度的源 − 本次已请求过的源」。
	if want.Invoice && !tr.got.Invoice {
		s.traverse(ctx, req, tr, s.invoice, model.DimSet{Invoice: true})
	}
	if want.Tax && !tr.got.Tax {
		s.traverse(ctx, req, tr, s.tax, model.DimSet{Tax: true})
	}

	tr.fillSkipped(s.sources, want)
	slog.Debug("upstream sourcing done", "reqid", req.Reqid,
		"want", want.String(), "got", tr.got.String(), "costFen", tr.costFen, "calls", len(tr.rows))
	return tr.result(req, want)
}

// traverse 按序遍历一个优先级列表，直到 need 被满足或列表走完。跳过的源只记原因，
// 轨迹行留到最后由 fillSkipped 统一补，避免同一个源出现在多个列表里被记两次。
func (s *Sourcer) traverse(ctx context.Context, req *model.UpstreamRequest, tr *trace, list []Source, need model.DimSet) {
	for _, src := range list {
		if tr.got.Covers(need) {
			return
		}
		if tr.done[src.Name] {
			continue // 本次已请求过该源（综合源阶段调过）——绝不重复付费
		}
		if _, skipped := tr.reason[src.Name]; skipped {
			continue
		}
		if req.Scope == model.ScopeBasic && src.Optional {
			tr.reason[src.Name] = "scope=basic 跳过可选源"
			continue
		}
		if left := tr.left(); left <= 0 {
			tr.reason[src.Name] = "已超出本次总时延预算，未再尝试"
			continue
		}
		s.invoke(ctx, req, tr, src)
	}
}

// invoke 调用一个逻辑源：内含的互补调用并发发出（它们同属一个源、一起计价，没有
// 短路可省），再按维度归一该源的结果。
func (s *Sourcer) invoke(ctx context.Context, req *model.UpstreamRequest, tr *trace, src Source) {
	callCtx, cancel := context.WithTimeout(ctx, tr.left())
	defer cancel()

	rows := make([]model.SourceCall, len(src.Calls))
	var wg sync.WaitGroup
	for i := range src.Calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := src.Calls[i]
			start := time.Now()
			res, err := c.Port.Query(callCtx, req)
			sec := classify(res, err)
			row := model.SourceCall{
				Source:    src.Name,
				Label:     c.Label,
				Alias:     SourceAlias(c.Label),
				Provider:  src.Provider,
				Dims:      c.Dims,
				Status:    sec.Status,
				Msg:       sec.Error,
				LatencyMs: time.Since(start).Milliseconds(),
			}
			// 失败也要可追查（铁律）：子源"已应答但业务失败"时的上游订单号/请求号在
			// *model.UpstreamError 里，res 为 nil 时只能从错误里捞，不能丢。
			var ue *model.UpstreamError
			switch {
			case err != nil && errors.As(err, &ue):
				row.Code, row.UID, row.LogID = ue.Code, ue.UID, ue.LogID
				if ue.Msg != "" {
					row.Msg = ue.Msg
				}
			case res != nil:
				row.Code, row.UID, row.LogID = res.Code, res.UID, res.LogID
			}
			if sec.Status == model.CallOK || c.CostOn == CostOnCall {
				row.CostFen = c.CostFen // 查无/失败是否计成本由该源的商务条款决定
			}
			rows[i] = row
			tr.sections.put(c.Label, sec)
		}(i)
	}
	wg.Wait()

	tr.done[src.Name] = true
	tr.commit(rows)
}

// trace 是一次请求的寻源上下文：已调用源、逐源轨迹、累计实得维度与成本。
type trace struct {
	started time.Time
	budget  time.Duration
	got     model.DimSet
	costFen int64
	seq     int
	done    map[string]bool
	reason  map[string]string
	rows    []model.SourceCall
	sections
}

// sections 收集各源的 range 段（并发写入，需加锁）。
type sections struct {
	mu   sync.Mutex
	data map[string]aggSection
}

func (s *sections) put(label string, sec aggSection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[label] = sec
}

func newTrace(budget time.Duration) *trace {
	return &trace{
		started:  time.Now(),
		budget:   budget,
		done:     map[string]bool{},
		reason:   map[string]string{},
		sections: sections{data: map[string]aggSection{}},
	}
}

// left 是剩余时延预算。
func (t *trace) left() time.Duration { return t.budget - time.Since(t.started) }

// commit 把一个逻辑源的调用结果并入轨迹：编号、累计成本、累计实得维度。
func (t *trace) commit(rows []model.SourceCall) {
	for _, r := range rows {
		t.seq++
		r.Seq = t.seq
		t.costFen += r.CostFen
		if r.Status == model.CallOK {
			t.got = t.got.Union(r.Dims)
		}
		t.rows = append(t.rows, r)
	}
}

// fillSkipped 为未调用的源补 skipped 轨迹行——「没调哪些源、为什么没调」和
// 「调了哪些源」同样是对账证据（尤其是被更便宜的源短路掉的那些）。
func (t *trace) fillSkipped(all []Source, want model.DimSet) {
	for _, src := range all {
		if t.done[src.Name] {
			continue
		}
		reason := t.reason[src.Name]
		if reason == "" {
			if src.Provides.Intersect(want).Empty() {
				reason = "本次请求不需要该维度"
			} else {
				reason = "更高优先级源已满足，未调用"
			}
		}
		for _, c := range src.Calls {
			t.rows = append(t.rows, model.SourceCall{
				Source:   src.Name,
				Label:    c.Label,
				Alias:    SourceAlias(c.Label),
				Provider: src.Provider,
				Dims:     c.Dims,
				Status:   model.CallSkipped,
				Reason:   reason,
			})
			t.sections.put(c.Label, aggSection{Status: model.CallSkipped, Error: reason})
		}
	}
}

// result 按新判定表产出下游结论：
//
//	实得 ≥1 维度                → 001（计费；按实得维度定档，见 model.StandardOf）
//	无实得 + 有源失败 + 有源查无 → 002（确定结论、不计费）
//	无实得 + 全部查无           → 999
//	无实得 + 全部失败           → *model.UpstreamError（对外 505062，走复查/对账）
func (t *trace) result(req *model.UpstreamRequest, want model.DimSet) (*model.UpstreamResult, error) {
	sum := model.SummarizeSources(t.rows)
	base := &model.UpstreamResult{
		UID:     sum.UID,
		Reqid:   req.Reqid,
		LogID:   sum.LogID,
		Got:     t.got,
		Sources: t.rows,
		CostFen: t.costFen,
	}

	if sum.Called == 0 {
		// 没有任何源可用于本次维度（配置缺口），当作我方原因失败，不计费。
		return nil, fmt.Errorf("寻源无可用数据源 (reqid=%s, 请求维度=%s)", req.Reqid, want.String())
	}
	if sum.OK == 0 && sum.Err == sum.Called {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("寻源全部数据源失败 (reqid=%s)", req.Reqid))
		code := ""
		for _, r := range t.rows {
			if r.Status != model.CallError {
				continue
			}
			if code == "" && r.Code != "" {
				code = r.Code
			}
			b.WriteString(fmt.Sprintf(" | %s: code=%s %s", r.Label, r.Code, r.Msg))
		}
		if code == "" {
			code = "sourcing_all_failed"
		}
		return nil, &model.UpstreamError{
			Code: code, Msg: b.String(), UID: sum.UID, LogID: sum.LogID,
			Sources: t.rows, CostFen: t.costFen,
		}
	}

	switch {
	case !t.got.Empty():
		merged, err := json.Marshal(t.sections.data)
		if err != nil {
			return nil, fmt.Errorf("寻源结果序列化失败: %w", err)
		}
		base.Code, base.Msg, base.Range = "001", "成功", string(merged)
	case sum.Err > 0:
		merged, err := json.Marshal(t.sections.data)
		if err != nil {
			return nil, fmt.Errorf("寻源结果序列化失败: %w", err)
		}
		base.Code, base.Msg, base.Range = "002", "未取得数据且部分数据源异常", string(merged)
	default:
		// 全部查无：与单上游 999 一致，range 恒空 (DESIGN §7.4)。
		base.Code, base.Msg = "999", "查无结果"
	}
	return base, nil
}

// Requery：只有单调用配置才能直通上游复查；多源时逐源复查另案处理，返回
// Reachable=false 让台账保持 PENDING 由对账兜底（与聚合器时期行为一致）。
// 有了逐源明细 (upstream_call) 之后已具备逐源复查的数据基础（每源各自的 uid）。
func (s *Sourcer) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	if s.calls == 1 {
		return s.sources[0].Calls[0].Port.Requery(ctx, reqid)
	}
	return &model.RequeryResult{Reachable: false}, nil
}
