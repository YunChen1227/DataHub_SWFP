package application

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/quota"
)

// bookTask 是一次请求响应后的记账工作单：台账结算（可选）+ 逐源明细 + 审计落库。
// 三者对构造下游响应毫无贡献，从关键路径剥离（DESIGN 异步记账）。
type bookTask struct {
	token    *quota.ReserveToken    // 与 decision 成对；nil = 无需结算（鉴权/参数失败、幂等重放、PENDING）
	decision *model.BillingDecision // 上游确定结论；PENDING 场景为 nil（台账留待复查/对账结算）
	sources  []model.SourceCall     // 逐源寻源轨迹（含 skipped）；无上游调用的路径为空
	rec      *model.AuditRecord     // 恒非 nil：每次请求都写审计
	// 主体年度计费的输入：查的哪家企业 + 该客户的三档合同价（免费期扣减后要按剩下的
	// 类目重算金额）。参数校验失败的路径两者皆空。
	creditCode string
	rates      model.FeeRates
}

// settleInput 组装结算输入。busiCode 取审计记录里的最终下游业务码——它在响应
// 映射完成后才确定，正好由本任务（响应写回后执行）带上。
func (t bookTask) settleInput() quota.SettleInput {
	in := quota.SettleInput{Decision: t.decision, Sources: t.sources, CreditCode: t.creditCode}
	if t.rec != nil {
		in.BusiCode = t.rec.BusiCode
	}
	return in
}

// applyChargeToAudit 把主体计费结论回填进审计记录。必须在 AppendAudit 之前调用。
//
// Billed 的语义在此改变：它不再等于「是否查得数据」(FoundData，那是 busiCode 10)，
// 而是「本次是否真的收了钱」。于是 FoundData=true 且 Billed=false 就是一次免费期
// 命中——这正是「计费与查得统计彻底分开」的落地形态。
func (t bookTask) applyChargeToAudit() {
	if t.rec == nil || t.decision == nil {
		return
	}
	d := t.decision
	t.rec.FeeStandard = string(d.Standard)
	t.rec.AmountFen = d.AmountFen
	t.rec.ChargedScope = d.Charged.String()
	t.rec.CoveredScope = d.Covered.String()
	t.rec.ChargeState = d.ChargeState
	t.rec.Billed = d.ChargeState == model.ChargeCharged
}

// callRecords 把轨迹转成逐源明细行。关联键与台账同构 (appKey, version, reqid)，
// 另带 requestId 供后台按请求下钻。
func (t bookTask) callRecords() []*model.UpstreamCallRecord {
	if len(t.sources) == 0 || t.rec == nil {
		return nil
	}
	out := make([]*model.UpstreamCallRecord, 0, len(t.sources))
	now := time.Now()
	for _, c := range t.sources {
		out = append(out, &model.UpstreamCallRecord{
			SourceCall: c,
			AppKey:     t.rec.AppKey,
			Version:    t.rec.Version,
			Reqid:      t.rec.Reqid,
			RequestID:  t.rec.RequestID,
			Billable:   c.Status == model.CallOK,
			CreatedAt:  now,
		})
	}
	return out
}

// Bookkeeper 把结算 + 审计移出请求关键路径：Handle 构造完响应即入队返回，
// 常驻 worker 用独立 context（请求 ctx 响应后即取消，不能复用）落库。
//
// 可靠性口径：
//   - 背压：队列满或已关闭时降级为「同步执行」——宁可让该请求慢几毫秒，绝不
//     静默丢弃计费台账/审计记录。
//   - 停机：Close() 停止接收并 drain 全部余量后返回（主流程在 HTTP Shutdown
//     后调用，复用现有 10s 优雅停机窗口）。
//   - 进程崩溃窗口：队列中未落库的审计/计数丢失，但 PENDING 台账已在响应前
//     同步写入（崩溃安全锚点），由复查/对账兜底终态化——与 DESIGN §7.3 一致。
//   - 与 RequeryWorker 的边界：本 worker 是同步路径结算的唯一执行者；
//     RequeryWorker 只处理「复查可达」的 PENDING 台账（当前各上游 Requery 均为
//     stub 不可达）。若未来实现真实 Requery，需为其加台账年龄下限，避免与
//     在队列中的毫秒级新 PENDING 抢结算。
type Bookkeeper struct {
	quota *quota.Service
	audit port.AuditRepository
	calls port.UpstreamCallRepository // 逐源明细；nil 时跳过（未装配/测试）
	log   *slog.Logger

	// 主体年度计费；subjects 或 billing 为 nil 时整个判定跳过，decision 保持
	// Decide 给出的按次计费毛口径（未装配时的退化行为，见 WithSubjectBilling）。
	subjects   port.SubjectBillingRepository
	billing    *billing.Service
	freeWindow string // Postgres interval 字面量；空则用 model.DefaultFreeWindow

	mu     sync.RWMutex // 保护 closed 与 tasks 的发送/关闭竞态
	closed bool
	tasks  chan bookTask
	wg     sync.WaitGroup
}

// NewBookkeeper 启动 workers 个常驻记账协程（queueSize/workers ≤0 时取缺省
// 1024/2）。quota 或 audit 可为 nil（对应操作跳过）。
func NewBookkeeper(q *quota.Service, audit port.AuditRepository, queueSize, workers int, log *slog.Logger) *Bookkeeper {
	if queueSize <= 0 {
		queueSize = 1024
	}
	if workers <= 0 {
		workers = 2
	}
	if log == nil {
		log = slog.Default()
	}
	b := &Bookkeeper{quota: q, audit: audit, log: log, tasks: make(chan bookTask, queueSize)}
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for t := range b.tasks {
				b.process(t)
			}
		}()
	}
	return b
}

// WithUpstreamCalls 挂接逐源明细仓储（upstream_call 表）。未挂接时不落明细，
// 台账里的汇总列仍然可用。
func (b *Bookkeeper) WithUpstreamCalls(r port.UpstreamCallRepository) *Bookkeeper {
	b.calls = r
	return b
}

// WithSubjectBilling 挂接主体年度计费：免费期仓储 + 计费服务 + 免费期长度
// （Postgres interval 字面量，如 "1 year"）。
//
// **不挂接即退化为按次计费**——每次查得都按实得维度足额收费，免费期形同不存在。
// 生产装配必须调用本方法，否则会重复向客户收费。
func (b *Bookkeeper) WithSubjectBilling(r port.SubjectBillingRepository, bill *billing.Service, window string) *Bookkeeper {
	b.subjects, b.billing, b.freeWindow = r, bill, window
	return b
}

// Submit 入队一个记账任务；队列满或已关闭时同步执行（背压降级，不丢任务）。
func (b *Bookkeeper) Submit(t bookTask) {
	b.mu.RLock()
	if !b.closed {
		select {
		case b.tasks <- t:
			b.mu.RUnlock()
			return
		default:
			// 队列满：降级同步。
		}
	}
	b.mu.RUnlock()
	b.process(t)
}

// Close 停止接收新任务并 drain 队列（阻塞至全部落库）。幂等。
func (b *Bookkeeper) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.tasks)
	b.mu.Unlock()
	b.wg.Wait()
}

// process 执行一个任务。必须用独立 context——请求 ctx 在响应写回后即被取消，
// 复用它会导致所有异步落库统一报 context canceled（异步化的经典坑）。
func (b *Bookkeeper) process(t bookTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 主体年度计费必须最先执行：它会重算 decision 的收费标准与应收金额，而下面的
	// Settle（写台账 + 计费计数器）与 AppendAudit 都要落这两个值。
	b.chargeSubject(ctx, t)

	if b.quota != nil && t.token != nil && t.decision != nil {
		if err := b.quota.Settle(ctx, t.token, t.settleInput()); err != nil {
			b.log.Error("async settle failed", "reqid", t.token.Reqid, "err", err)
		}
	}
	// 逐源明细与结算解耦：PENDING/失败路径没有结算，但上游成本照样已经发生，
	// 明细必须落库，否则对账时看不到这笔支出。
	if b.calls != nil {
		if rows := t.callRecords(); len(rows) > 0 {
			if err := b.calls.AppendUpstreamCalls(ctx, rows); err != nil {
				b.log.Error("async upstream calls append failed", "requestId", t.rec.RequestID, "err", err)
			}
		}
	}
	if b.audit != nil && t.rec != nil {
		if err := b.audit.AppendAudit(ctx, t.rec); err != nil {
			b.log.Error("async audit append failed", "requestId", t.rec.RequestID, "err", err)
		}
	}
}

// chargeSubject 执行主体年度计费判定，并把结论回填进 decision 与审计记录。
//
// 判定只看**实得类目**：查无(999)、部分源异常无实得(002)、全源失败一律不动窗口，
// 也不产生应收。复查路径（Requery）不携带维度信息，同样落在这条分支上。
func (b *Bookkeeper) chargeSubject(ctx context.Context, t bookTask) {
	if b.subjects == nil || b.billing == nil || t.decision == nil || t.token == nil {
		return // 未装配主体计费：保持 Decide 的按次计费毛口径
	}
	d := t.decision
	if t.creditCode == "" || d.Result == nil || d.Result.Got.Empty() {
		b.billing.ApplyCoverage(d, nil, t.rates) // 无实得类目 → NOCHARGE，不改金额
		t.applyChargeToAudit()
		return
	}

	res, err := b.subjects.ApplyCoverage(ctx, model.CoverageRequest{
		LicenseID:  t.token.LicenseID,
		Route:      t.token.Route,
		CreditCode: t.creditCode,
		Got:        d.Result.Got,
		Reqid:      t.token.Reqid,
		RequestID:  t.rec.RequestID,
		Window:     b.freeWindow,
	})
	if err != nil {
		// fail-closed：宁可暂时少收，绝不因一次 DB 抖动给客户重复收费。台账带着
		// credit_code + reqid 落 DEFERRED，由对账任务重放那条幂等的原子 SQL 补记。
		billing.MarkCoverageDeferred(d)
		t.applyChargeToAudit()
		b.log.Error("主体计费判定失败，本次按 0 收并标 DEFERRED 待对账补记",
			"reqid", t.token.Reqid, "requestId", t.rec.RequestID, "err", err)
		return
	}

	b.billing.ApplyCoverage(d, res, t.rates)
	t.applyChargeToAudit()
	if d.Charged.Empty() {
		return // 全部类目命中免费期：不写计费流水
	}

	// 计费流水与免费期状态解耦：它的每一列都能从台账恢复，故落库失败只记日志、
	// 不回滚已推进的窗口（回滚反而会导致下一次请求重复计费）。
	if err := b.subjects.RecordCharge(ctx, &model.SubjectCharge{
		LicenseID:       t.token.LicenseID,
		AppKey:          t.rec.AppKey,
		Route:           t.token.Route,
		CreditCode:      t.creditCode,
		Reqid:           t.token.Reqid,
		RequestID:       t.rec.RequestID,
		LedgerID:        t.token.LedgerID,
		Charged:         d.Charged,
		Covered:         d.Covered,
		FeeStandard:     d.Standard,
		AmountFen:       d.AmountFen,
		UpstreamCostFen: d.CostFen,
		Kind:            res.Kind(),
		WindowTo:        res.WindowTo(),
	}); err != nil {
		b.log.Error("计费流水落库失败（免费期已推进，台账仍可对账）",
			"reqid", t.token.Reqid, "requestId", t.rec.RequestID, "err", err)
	}
}
