package application

import (
	"context"
	"testing"

	"github.com/datahub/relay/internal/domain/billing"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/quota"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

// seedBegin 在 memory store 里播种 license 并开一条 PENDING 台账，返回结算 token。
func seedBegin(t *testing.T, store *memory.Store, q *quota.Service, reqid string) *quota.ReserveToken {
	t.Helper()
	lic := &model.LicenseView{LicenseID: "LIC-T1", AppKey: "ak-test", ClientUUID: "u1", Status: "ACTIVE"}
	store.SeedLicense(lic, "sec", "测试商户", "13800000000")
	tok, existing, err := q.Begin(context.Background(), lic, "swfp", reqid, "", "req-"+reqid, true)
	if err != nil || existing != nil || tok == nil {
		t.Fatalf("Begin: tok=%v existing=%v err=%v", tok, existing, err)
	}
	return tok
}

// TestBookkeeperSettlesAndAudits 锁定异步记账的核心行为：入队的结算工作单在
// Close(drain) 后必须完成台账 BILLED + 成功查得数/调用次数累计 + 审计落库。
func TestBookkeeperSettlesAndAudits(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	tok := seedBegin(t, store, q, "r1")

	b := NewBookkeeper(q, store, 8, 1, nil)
	dec := &model.BillingDecision{Resolved: true, Returned: true, Result: &model.UpstreamResult{Code: "001"}}
	b.Submit(bookTask{token: tok, decision: dec, rec: &model.AuditRecord{RequestID: "req-r1", Version: "swfp", AppKey: "ak-test"}})
	b.Close() // drain

	l, err := store.FindByReqid(context.Background(), "ak-test", "swfp", "r1")
	if err != nil || l == nil {
		t.Fatalf("FindByReqid: %v %v", l, err)
	}
	if l.State != model.StateBilled || !l.CountedService {
		t.Fatalf("台账未结算: state=%s counted=%v", l.State, l.CountedService)
	}
	if used, _ := store.ServiceUsed(context.Background(), "LIC-T1", "swfp"); used != 1 {
		t.Fatalf("成功查得数=%d, want 1", used)
	}
	if calls, _ := store.TotalCalls(context.Background(), "LIC-T1", "swfp"); calls != 1 {
		t.Fatalf("调用次数=%d, want 1", calls)
	}
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 || audits[0].RequestID != "req-r1" {
		t.Fatalf("审计未落库: %+v", audits)
	}
}

// TestBookkeeperSubmitAfterClose 关闭后 Submit 必须降级为同步执行（不 panic、不丢）。
func TestBookkeeperSubmitAfterClose(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	tok := seedBegin(t, store, q, "r2")

	b := NewBookkeeper(q, store, 8, 1, nil)
	b.Close()
	dec := &model.BillingDecision{Resolved: true, Returned: false, Result: &model.UpstreamResult{Code: "999"}}
	b.Submit(bookTask{token: tok, decision: dec, rec: &model.AuditRecord{RequestID: "req-r2", Version: "swfp", AppKey: "ak-test"}})

	l, _ := store.FindByReqid(context.Background(), "ak-test", "swfp", "r2")
	if l == nil || l.State != model.StateBilled || l.CountedService {
		t.Fatalf("关闭后同步降级未生效: %+v", l)
	}
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 {
		t.Fatalf("审计条数=%d, want 1", len(audits))
	}
}

// subjectBooks 装配一台带主体年度计费的记账器（内存仓储 + 三档缺省价）。
func subjectBooks(store *memory.Store, q *quota.Service) *Bookkeeper {
	bill := billing.New(billing.DefaultTable()).
		WithDefaultRates(model.FeeRates{BothFen: 1000, InvoiceFen: 600, TaxFen: 500})
	return NewBookkeeper(q, store, 8, 1, nil).
		WithSubjectBilling(store, bill, model.DefaultFreeWindow)
}

// runQuery 走一遍完整记账：开台账 → 提交工作单 → drain。返回该次的台账。
func runQuery(t *testing.T, store *memory.Store, q *quota.Service, reqid, creditCode string, got model.DimSet) *model.Ledger {
	t.Helper()
	tok := seedBegin(t, store, q, reqid)
	dec := billing.New(billing.DefaultTable()).
		WithDefaultRates(model.FeeRates{BothFen: 1000, InvoiceFen: 600, TaxFen: 500}).
		Decide(&model.UpstreamResult{Code: "001", Got: got}, model.FeeRates{})

	b := subjectBooks(store, q)
	b.Submit(bookTask{
		token:      tok,
		decision:   dec,
		creditCode: creditCode,
		rec: &model.AuditRecord{
			RequestID: "req-" + reqid, Version: "swfp", AppKey: "ak-test",
			CreditCode: creditCode, BusiCode: 10, FoundData: true,
		},
	})
	b.Close() // drain

	l, err := store.FindByReqid(context.Background(), "ak-test", "swfp", reqid)
	if err != nil || l == nil {
		t.Fatalf("FindByReqid(%s): %v %v", reqid, l, err)
	}
	return l
}

// TestSubjectBilling_FirstChargesThenFree 是本次改动的端到端守门测试，一次断清
// 三件事：首次查得计费、一年内复查免费、以及**计费与查得统计彻底解耦**——
// 免费那次照样累加成功查得数，但不产生任何应收。
func TestSubjectBilling_FirstChargesThenFree(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	ctx := context.Background()
	const code = "91310000MA1FL0Q40X"

	first := runQuery(t, store, q, "s1", code, model.AllDims())
	if first.ChargeState != model.ChargeCharged {
		t.Errorf("首次 ChargeState=%q, want CHARGED", first.ChargeState)
	}
	if first.AmountFen != 1000 || first.FeeStandard != model.FeeBoth {
		t.Errorf("首次应按 both 档收 1000, got %q/%d", first.FeeStandard, first.AmountFen)
	}
	if first.ChargedScope != "invoice+tax" || first.CoveredScope != "none" {
		t.Errorf("首次 charged/covered=%q/%q, want invoice+tax/none", first.ChargedScope, first.CoveredScope)
	}
	if first.CreditCode != code {
		t.Errorf("台账未落计费主体, got %q", first.CreditCode)
	}

	second := runQuery(t, store, q, "s2", code, model.AllDims())
	if second.ChargeState != model.ChargeFree {
		t.Errorf("复查 ChargeState=%q, want FREE", second.ChargeState)
	}
	if second.AmountFen != 0 {
		t.Errorf("复查应免费, got %d 分", second.AmountFen)
	}
	if second.CoveredScope != "invoice+tax" || second.ChargedScope != "none" {
		t.Errorf("复查 charged/covered=%q/%q, want none/invoice+tax", second.ChargedScope, second.CoveredScope)
	}

	// 解耦断言：两次都查得数据 → 成功查得数 = 2；但只计费一次 → 应收仍是 1000。
	if used, _ := store.ServiceUsed(ctx, "LIC-T1", "swfp"); used != 2 {
		t.Errorf("成功查得数=%d, want 2（免费期不影响查得统计）", used)
	}
	bill, err := store.BillingCounters(ctx, "LIC-T1", "swfp")
	if err != nil {
		t.Fatalf("BillingCounters: %v", err)
	}
	if bill.ChargedBoth != 1 {
		t.Errorf("税务发票计费笔数=%d, want 1（第二次免费不计）", bill.ChargedBoth)
	}
	if bill.AmountFen != 1000 {
		t.Errorf("累计应收=%d, want 1000", bill.AmountFen)
	}

	// 审计侧：两行都 foundData=true，但只有第一行 billed=true。
	audits, _ := store.ListAudits(ctx, model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 2 {
		t.Fatalf("审计条数=%d, want 2", len(audits))
	}
	for _, a := range audits {
		if !a.FoundData {
			t.Errorf("requestId=%s foundData 应为 true", a.RequestID)
		}
	}
	var billed int
	for _, a := range audits {
		if a.Billed {
			billed++
		}
	}
	if billed != 1 {
		t.Errorf("审计里 billed=true 的条数=%d, want 1", billed)
	}

	// 计费流水只有一笔。
	charges, _ := store.ListCharges(ctx, "LIC-T1", 0)
	if len(charges) != 1 {
		t.Fatalf("计费流水=%d 笔, want 1", len(charges))
	}
	if charges[0].Kind != "first" || charges[0].AmountFen != 1000 {
		t.Errorf("流水 kind/amount=%q/%d, want first/1000", charges[0].Kind, charges[0].AmountFen)
	}
}

// TestSubjectBilling_SecondCategoryChargedSeparately 先只查得发票，后来补到税务：
// 只对税务按 tax 单价计费，发票命中免费期。
func TestSubjectBilling_SecondCategoryChargedSeparately(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	const code = "91310000MA1FL0Q40X"

	first := runQuery(t, store, q, "s1", code, model.DimSet{Invoice: true})
	if first.FeeStandard != model.FeeInvoice || first.AmountFen != 600 {
		t.Errorf("首次只得发票应收 600, got %q/%d", first.FeeStandard, first.AmountFen)
	}

	second := runQuery(t, store, q, "s2", code, model.AllDims())
	if second.FeeStandard != model.FeeTax || second.AmountFen != 500 {
		t.Errorf("补到税务应按 tax 档收 500（而非 both 差价）, got %q/%d", second.FeeStandard, second.AmountFen)
	}
	if second.ChargedScope != "tax" || second.CoveredScope != "invoice" {
		t.Errorf("charged/covered=%q/%q, want tax/invoice", second.ChargedScope, second.CoveredScope)
	}

	bill, _ := store.BillingCounters(context.Background(), "LIC-T1", "swfp")
	if bill.ChargedInvoice != 1 || bill.ChargedTax != 1 || bill.ChargedBoth != 0 {
		t.Errorf("两次应各计一笔单项, got invoice=%d tax=%d both=%d",
			bill.ChargedInvoice, bill.ChargedTax, bill.ChargedBoth)
	}
	if bill.AmountFen != 1100 {
		t.Errorf("累计应收=%d, want 1100", bill.AmountFen)
	}
}

// TestSubjectBilling_NotFoundDoesNotCharge 查无既不计费也不开窗——否则客户第一次
// 查无、第二次查得就白拿一年数据。
func TestSubjectBilling_NotFoundDoesNotCharge(t *testing.T) {
	store := memory.New()
	q := quota.New(store, store)
	ctx := context.Background()
	const code = "91310000MA1FL0Q40X"

	tok := seedBegin(t, store, q, "s1")
	dec := billing.New(billing.DefaultTable()).
		WithDefaultRates(model.FeeRates{BothFen: 1000}).
		Decide(&model.UpstreamResult{Code: "999"}, model.FeeRates{})
	b := subjectBooks(store, q)
	b.Submit(bookTask{token: tok, decision: dec, creditCode: code,
		rec: &model.AuditRecord{RequestID: "req-s1", Version: "swfp", AppKey: "ak-test", CreditCode: code}})
	b.Close()

	l, _ := store.FindByReqid(ctx, "ak-test", "swfp", "s1")
	if l.ChargeState != model.ChargeNoCharge || l.AmountFen != 0 {
		t.Errorf("查无应为 NOCHARGE/0, got %q/%d", l.ChargeState, l.AmountFen)
	}
	cov, _ := store.SubjectCoverage(ctx, "LIC-T1", "swfp", code)
	if cov.Invoice.Covered || cov.Tax.Covered {
		t.Errorf("查无不得开免费期, got %+v", cov)
	}

	// 随后真的查得时必须全额计费。
	second := runQuery(t, store, q, "s2", code, model.AllDims())
	if second.AmountFen != 1000 {
		t.Errorf("查无之后首次查得应全额计费, got %d", second.AmountFen)
	}
}

// TestBookkeeperAuditOnlyTask 无结算单（鉴权失败/PENDING 场景）只写审计。
func TestBookkeeperAuditOnlyTask(t *testing.T) {
	store := memory.New()
	b := NewBookkeeper(nil, store, 8, 1, nil)
	b.Submit(bookTask{rec: &model.AuditRecord{RequestID: "req-r3", Version: "swfp"}})
	b.Close()
	audits, _ := store.ListAudits(context.Background(), model.AuditFilter{Version: "swfp", Limit: 10})
	if len(audits) != 1 || audits[0].RequestID != "req-r3" {
		t.Fatalf("审计未落库: %+v", audits)
	}
}
