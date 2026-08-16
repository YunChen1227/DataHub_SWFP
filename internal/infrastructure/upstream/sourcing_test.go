package upstream

import (
	"context"
	"errors"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

// fakePort 是一个可编排返回值的上游子源桩。
type fakePort struct {
	res  *model.UpstreamResult
	err  error
	hits *int // 非 nil 时记录被调用次数（验证「命中即停」与「不重复付费」）
}

func (f fakePort) Query(context.Context, *model.UpstreamRequest) (*model.UpstreamResult, error) {
	if f.hits != nil {
		*f.hits++
	}
	return f.res, f.err
}
func (f fakePort) Requery(context.Context, string) (*model.RequeryResult, error) {
	return &model.RequeryResult{Reachable: false}, nil
}

func okPort(hits *int) fakePort {
	return fakePort{res: &model.UpstreamResult{Code: "001", Msg: "成功", UID: "ORD-OK", LogID: "ORD-OK", Range: `{"nsrjbxx":{}}`}, hits: hits}
}

func emptyPort(hits *int) fakePort {
	return fakePort{res: &model.UpstreamResult{Code: "999", Msg: "查无", UID: "ORD-EMPTY", LogID: "ORD-EMPTY"}, hits: hits}
}

func errPort(code, uid string, hits *int) fakePort {
	return fakePort{err: &model.UpstreamError{Code: code, Msg: "上游拒绝", UID: uid, LogID: uid}, hits: hits}
}

// src 组一个单调用逻辑源。
func src(name string, dims model.DimSet, priority int, costFen int64, p fakePort) Source {
	return Source{
		Name: name, Provider: "test", Provides: dims, Priority: priority,
		Calls: []Call{{Label: name, Dims: dims, CostFen: costFen, Port: p}},
	}
}

func rowOf(t *testing.T, rows []model.SourceCall, label string) model.SourceCall {
	t.Helper()
	for _, r := range rows {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("轨迹里没有 %s: %+v", label, rows)
	return model.SourceCall{}
}

var invoiceDim = model.DimSet{Invoice: true}
var taxDim = model.DimSet{Tax: true}

// TestSourcerShortCircuit 单项请求：按优先级串行，命中即停——后面的源一次都不能调，
// 成本只算命中的那个，未调用的源仍要出 skipped 轨迹（对账要能看出"没花这笔钱"）。
func TestSourcerShortCircuit(t *testing.T) {
	var firstHits, secondHits int
	s, err := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 300, okPort(&firstHits)),
		src("inv_b", invoiceDim, 2, 500, okPort(&secondHits)),
	}, 0)
	if err != nil {
		t.Fatalf("NewSourcer: %v", err)
	}

	res, callErr := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r1", Want: invoiceDim})
	if callErr != nil {
		t.Fatalf("Query: %v", callErr)
	}
	if firstHits != 1 || secondHits != 0 {
		t.Fatalf("命中即停失效: 高优先级源调用 %d 次, 低优先级源调用 %d 次(应为 0)", firstHits, secondHits)
	}
	if res.Code != "001" || res.Got != invoiceDim {
		t.Fatalf("want 001/invoice, got %s/%s", res.Code, res.Got)
	}
	if res.CostFen != 300 {
		t.Fatalf("成本应只含命中源 300, got %d", res.CostFen)
	}
	if r := rowOf(t, res.Sources, "inv_b"); r.Status != model.CallSkipped || r.Reason == "" || r.CostFen != 0 {
		t.Fatalf("未调用的源必须留 skipped 轨迹且不计成本: %+v", r)
	}
}

// TestSourcerGapFillingDeduplicates 两项请求的核心路径：综合源只拿到一半时，缺项
// 补齐必须跳过"本次已请求过的源"（绝不重复付费），并用别的源把缺的维度补上。
func TestSourcerGapFillingDeduplicates(t *testing.T) {
	var comboInv, comboTax, taxHits int
	combined := Source{
		Name: "combo", Provider: "test", Provides: model.AllDims(), Priority: 1,
		Calls: []Call{
			{Label: "invoice1", Dims: invoiceDim, CostFen: 200, Port: okPort(&comboInv)},
			{Label: "tax1", Dims: taxDim, CostFen: 200, Port: emptyPort(&comboTax)},
		},
	}
	s, err := NewSourcer([]Source{
		combined,
		src("tax_b", taxDim, 2, 100, okPort(&taxHits)),
	}, 0)
	if err != nil {
		t.Fatalf("NewSourcer: %v", err)
	}

	res, callErr := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r2", Want: model.AllDims()})
	if callErr != nil {
		t.Fatalf("Query: %v", callErr)
	}
	if comboInv != 1 || comboTax != 1 {
		t.Fatalf("综合源的互补调用应各调一次, got inv=%d tax=%d", comboInv, comboTax)
	}
	if taxHits != 1 {
		t.Fatalf("缺税务时应补齐调用 tax_b 一次, got %d", taxHits)
	}
	if !res.Got.Both() {
		t.Fatalf("补齐后应两项皆得, got %s", res.Got)
	}
	// 综合源查无的那次调用不计成本（缺省 costOn=hit）。
	if res.CostFen != 300 {
		t.Fatalf("成本应为 综合源命中 200 + 补齐源 100 = 300, got %d", res.CostFen)
	}
	if r := rowOf(t, res.Sources, "tax1"); r.Status != model.CallEmpty {
		t.Fatalf("综合源税务调用应为 empty: %+v", r)
	}
}

// TestSourcerSingleDimSkipsOtherDim 单项请求绝不触碰另一维度的源：省钱的第一现场。
func TestSourcerSingleDimSkipsOtherDim(t *testing.T) {
	var invHits, taxHits int
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 100, okPort(&invHits)),
		src("tax_a", taxDim, 1, 100, okPort(&taxHits)),
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r3", Want: taxDim})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if taxHits != 1 || invHits != 0 {
		t.Fatalf("单税务请求不应调发票源: inv=%d tax=%d", invHits, taxHits)
	}
	if res.Got != taxDim {
		t.Fatalf("want got=tax, got %s", res.Got)
	}
	if r := rowOf(t, res.Sources, "inv_a"); r.Status != model.CallSkipped {
		t.Fatalf("发票源应为 skipped: %+v", r)
	}
}

// TestSourcerEmptyWantDefaultsToBoth 老下游不带 dataType 时按两项都要处理（兼容）。
func TestSourcerEmptyWantDefaultsToBoth(t *testing.T) {
	var invHits, taxHits int
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 100, okPort(&invHits)),
		src("tax_a", taxDim, 1, 100, okPort(&taxHits)),
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r4"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if invHits != 1 || taxHits != 1 {
		t.Fatalf("未指定维度应两项都查: inv=%d tax=%d", invHits, taxHits)
	}
	if !res.Got.Both() {
		t.Fatalf("want got=invoice+tax, got %s", res.Got)
	}
}

// TestSourcerAllFailedCarriesUpstreamIDs 锁定「上游 requestId 无论成功失败都必须落
// 审计」铁律：所有源都以业务失败返回时，必须回传 *model.UpstreamError 带出
// uid/logId/code 与已产生的成本/轨迹——绝不能退化成裸 error 让审计三列变空。
func TestSourcerAllFailedCarriesUpstreamIDs(t *testing.T) {
	s, _ := NewSourcer([]Source{
		{Name: "inv_a", Provider: "test", Provides: invoiceDim, Priority: 1,
			Calls: []Call{{Label: "invoice1", Dims: invoiceDim, CostFen: 100, CostOn: CostOnCall,
				Port: errPort("E1099", "ORD-A", nil)}}},
		src("tax_a", taxDim, 1, 100, errPort("E1010", "ORD-B", nil)),
	}, 0)

	res, callErr := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r5", Want: model.AllDims()})
	if res != nil {
		t.Fatalf("全失败应返回 nil 结果, got %+v", res)
	}
	var ue *model.UpstreamError
	if !errors.As(callErr, &ue) {
		t.Fatalf("全失败必须返回 *model.UpstreamError（否则审计三列为空）, got %T: %v", callErr, callErr)
	}
	if ue.UID == "" || ue.LogID == "" || ue.Code == "" {
		t.Fatalf("全失败必须带上游 code/uid/logId, got %+v", ue)
	}
	if len(ue.Sources) == 0 {
		t.Fatalf("全失败也必须带逐源轨迹（对账要看到这些失败调用）")
	}
	// costOn=call 的源即便失败也已产生成本，必须带出，否则亏损单在库里看不见。
	if ue.CostFen != 100 {
		t.Fatalf("失败单成本应为 100 (costOn=call), got %d", ue.CostFen)
	}
}

// TestSourcerPartialFailureIs002 无实得 + 有源失败 + 有源查无 → 002（确定结论、不计费）。
func TestSourcerPartialFailureIs002(t *testing.T) {
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 100, errPort("E1010", "ORD-FAIL", nil)),
		src("tax_a", taxDim, 1, 100, emptyPort(nil)),
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r6", Want: model.AllDims()})
	if err != nil {
		t.Fatalf("部分失败不应返回 error: %v", err)
	}
	if res.Code != "002" {
		t.Fatalf("want 002, got %s", res.Code)
	}
	if !res.Got.Empty() {
		t.Fatalf("002 时实得维度应为空, got %s", res.Got)
	}
	if res.UID == "" || res.LogID == "" {
		t.Fatalf("002 也必须带上游 uid/logId: %+v", res)
	}
	if res.CostFen != 0 {
		t.Fatalf("缺省 costOn=hit 时查无/失败不计成本, got %d", res.CostFen)
	}
}

// TestSourcerAllEmptyIs999 全部查无 → 999 且 range 恒空（DESIGN §7.4）。
func TestSourcerAllEmptyIs999(t *testing.T) {
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 100, emptyPort(nil)),
		src("tax_a", taxDim, 1, 100, emptyPort(nil)),
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r7", Want: model.AllDims()})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Code != "999" || res.Range != "" {
		t.Fatalf("want 999/空 range, got %s/%q", res.Code, res.Range)
	}
}

// TestSourcerScopeBasicSkipsOptional scope=basic 时可选源（源5 销项数据）不调用。
func TestSourcerScopeBasicSkipsOptional(t *testing.T) {
	var optHits int
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 100, emptyPort(nil)),
		{Name: "sales", Provider: "test", Provides: invoiceDim, Priority: 2, Optional: true,
			Calls: []Call{{Label: "sales", Dims: invoiceDim, CostFen: 50, Port: okPort(&optHits)}}},
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r8", Scope: model.ScopeBasic, Want: invoiceDim})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if optHits != 0 {
		t.Fatalf("scope=basic 不应调用可选源, got %d 次", optHits)
	}
	if r := rowOf(t, res.Sources, "sales"); r.Status != model.CallSkipped {
		t.Fatalf("可选源应为 skipped: %+v", r)
	}
}

// TestSourcerFallbackWhenPrimaryEmpty 首选源查无时继续走下一个源（这才是"遍历直至
// 查得或遍历完"）。
func TestSourcerFallbackWhenPrimaryEmpty(t *testing.T) {
	var firstHits, secondHits int
	s, _ := NewSourcer([]Source{
		src("inv_a", invoiceDim, 1, 300, emptyPort(&firstHits)),
		src("inv_b", invoiceDim, 2, 500, okPort(&secondHits)),
	}, 0)

	res, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r9", Want: invoiceDim})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if firstHits != 1 || secondHits != 1 {
		t.Fatalf("首选查无应继续下一个源: a=%d b=%d", firstHits, secondHits)
	}
	if res.Code != "001" || res.CostFen != 500 {
		t.Fatalf("want 001/成本 500(仅命中源), got %s/%d", res.Code, res.CostFen)
	}
}

// TestSourcerPriorityBeatsConfigOrder 优先级覆盖配置顺序；同优先级按成本从低到高。
func TestSourcerPriorityBeatsConfigOrder(t *testing.T) {
	var cheapHits, expensiveHits int
	s, _ := NewSourcer([]Source{
		src("inv_expensive", invoiceDim, 0, 900, okPort(&expensiveHits)),
		src("inv_cheap", invoiceDim, 0, 100, okPort(&cheapHits)),
	}, 0)

	if _, err := s.Query(context.Background(), &model.UpstreamRequest{Reqid: "r10", Want: invoiceDim}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if cheapHits != 1 || expensiveHits != 0 {
		t.Fatalf("同优先级应先调便宜的源: cheap=%d expensive=%d", cheapHits, expensiveHits)
	}
}
