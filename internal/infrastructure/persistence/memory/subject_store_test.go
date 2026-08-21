package memory

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

const (
	testLic  = "LIC-SUB"
	testCode = "91310000MA1FL0Q40X"
)

func req(reqid string, got model.DimSet) model.CoverageRequest {
	return model.CoverageRequest{
		LicenseID: testLic, Route: "swfp", CreditCode: testCode,
		Got: got, Reqid: reqid, RequestID: "req-" + reqid,
	}
}

// chargedOf 汇总一次判定里真正产生应收的类目。
func chargedOf(t *testing.T, res *model.CoverageResult) model.DimSet {
	t.Helper()
	if res == nil {
		t.Fatal("判定结果为 nil")
	}
	return res.Charged()
}

// TestApplyCoverage_FirstChargesThenFree 是最核心的一条：同主体同类目首次计费，
// 之后一年内无论查多少次都不再计费。
func TestApplyCoverage_FirstChargesThenFree(t *testing.T) {
	s := New()
	ctx := context.Background()

	first, err := s.ApplyCoverage(ctx, req("r1", model.AllDims()))
	if err != nil {
		t.Fatalf("首次判定: %v", err)
	}
	if got := chargedOf(t, first); got != model.AllDims() {
		t.Fatalf("首次应两项皆计费, got %s", got.String())
	}
	if k := first.Kind(); k != "first" {
		t.Errorf("Kind=%q, want first", k)
	}

	for i := 2; i <= 5; i++ {
		res, err := s.ApplyCoverage(ctx, req("r"+strconv.Itoa(i), model.AllDims()))
		if err != nil {
			t.Fatalf("第 %d 次判定: %v", i, err)
		}
		if got := chargedOf(t, res); !got.Empty() {
			t.Fatalf("第 %d 次应全部免费, 却对 %s 计费", i, got.String())
		}
	}

	cov, err := s.SubjectCoverage(ctx, testLic, "swfp", testCode)
	if err != nil {
		t.Fatalf("读免费期: %v", err)
	}
	if !cov.Invoice.Covered || !cov.Tax.Covered {
		t.Errorf("两个类目都应在免费期内, got %+v", cov)
	}
	if cov.Invoice.ChargeCount != 1 || cov.Tax.ChargeCount != 1 {
		t.Errorf("计费轮数应各为 1, got invoice=%d tax=%d", cov.Invoice.ChargeCount, cov.Tax.ChargeCount)
	}
	if cov.Invoice.FreeHits != 4 || cov.Tax.FreeHits != 4 {
		t.Errorf("免费命中数应各为 4, got invoice=%d tax=%d", cov.Invoice.FreeHits, cov.Tax.FreeHits)
	}
}

// TestApplyCoverage_SecondCategoryChargedSeparately 用户明确要求的场景：先只查得
// 发票，之后某次查得了税务，则**只对补上的税务计费**，发票仍在免费期内。
func TestApplyCoverage_SecondCategoryChargedSeparately(t *testing.T) {
	s := New()
	ctx := context.Background()

	if got := chargedOf(t, mustApply(t, s, req("r1", model.DimSet{Invoice: true}))); got != (model.DimSet{Invoice: true}) {
		t.Fatalf("首次只得发票应只对发票计费, got %s", got.String())
	}

	second := mustApply(t, s, req("r2", model.AllDims()))
	if got := chargedOf(t, second); got != (model.DimSet{Tax: true}) {
		t.Fatalf("补查应只对税务计费, got %s", got.String())
	}
	if got := second.Covered(); got != (model.DimSet{Invoice: true}) {
		t.Fatalf("发票应命中免费期, got %s", got.String())
	}
	// 两个类目各自独立开窗，都只计过一次费。
	cov, _ := s.SubjectCoverage(ctx, testLic, "swfp", testCode)
	if cov.Invoice.ChargeCount != 1 || cov.Tax.ChargeCount != 1 {
		t.Errorf("两类目各应计费 1 轮, got invoice=%d tax=%d", cov.Invoice.ChargeCount, cov.Tax.ChargeCount)
	}
}

// TestApplyCoverage_NotFoundDoesNotOpenWindow 查无/失败不得开窗：否则客户第一次
// 查无、第二次查得时会白拿一年数据。
func TestApplyCoverage_NotFoundDoesNotOpenWindow(t *testing.T) {
	s := New()
	ctx := context.Background()

	res := mustApply(t, s, req("r1", model.DimSet{}))
	if len(res.Verdicts) != 0 {
		t.Fatalf("无实得类目不应产生任何判定, got %+v", res.Verdicts)
	}
	cov, _ := s.SubjectCoverage(ctx, testLic, "swfp", testCode)
	if cov.Invoice.Covered || cov.Tax.Covered {
		t.Fatalf("查无不得开窗, got %+v", cov)
	}
	// 随后真的查得时必须正常计费。
	if got := chargedOf(t, mustApply(t, s, req("r2", model.AllDims()))); got != model.AllDims() {
		t.Fatalf("查无之后首次查得应全额计费, got %s", got.String())
	}
}

// TestApplyCoverage_ExpiryRenews 免费期满后下一次查得重新计费并开新窗口，
// charge_count 递增（跨年续费的账单依据）。
func TestApplyCoverage_ExpiryRenews(t *testing.T) {
	s := New()
	mustApply(t, s, req("r1", model.AllDims()))

	// 把窗口回拨到已过期（真实环境靠时间流逝，此处直接改内存状态）。
	s.mu.Lock()
	for _, cat := range []model.Category{model.CatInvoice, model.CatTax} {
		rec := s.coverage[coverageKey(testLic, "swfp", testCode, cat)]
		rec.expiresAt = time.Now().Add(-time.Hour)
	}
	s.mu.Unlock()

	renewed := mustApply(t, s, req("r2", model.AllDims()))
	if got := chargedOf(t, renewed); got != model.AllDims() {
		t.Fatalf("过期后应重新全额计费, got %s", got.String())
	}
	if k := renewed.Kind(); k != "renew" {
		t.Errorf("Kind=%q, want renew（非首次而是续期）", k)
	}
	for _, v := range renewed.Verdicts {
		if v.FirstEver {
			t.Errorf("%s 续期不应标为历史首次", v.Category)
		}
		if v.ChargeCount != 2 {
			t.Errorf("%s ChargeCount=%d, want 2", v.Category, v.ChargeCount)
		}
		if !v.ExpiresAt.After(time.Now().AddDate(0, 11, 0)) {
			t.Errorf("%s 新窗口应约一年后到期, got %v", v.Category, v.ExpiresAt)
		}
	}
}

// TestApplyCoverage_ConcurrentChargesExactlyOnce 是并发安全的守门测试：50 个请求
// 同时命中同一个尚未计费的主体，**恰好一次**计费。多收一次就是重复收费事故，
// 少收（全部判为免费）则永远收不到这一单。
func TestApplyCoverage_ConcurrentChargesExactlyOnce(t *testing.T) {
	s := New()
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	chargedInvoice, chargedTax := 0, 0

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := s.ApplyCoverage(ctx, req("c"+strconv.Itoa(i), model.AllDims()))
			if err != nil {
				t.Errorf("并发判定 %d: %v", i, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if res.Charged().Invoice {
				chargedInvoice++
			}
			if res.Charged().Tax {
				chargedTax++
			}
		}(i)
	}
	wg.Wait()

	if chargedInvoice != 1 {
		t.Errorf("发票计费次数=%d, want 恰好 1", chargedInvoice)
	}
	if chargedTax != 1 {
		t.Errorf("税务计费次数=%d, want 恰好 1", chargedTax)
	}
	cov, _ := s.SubjectCoverage(ctx, testLic, "swfp", testCode)
	if cov.Invoice.FreeHits != n-1 || cov.Tax.FreeHits != n-1 {
		t.Errorf("免费命中数应各为 %d, got invoice=%d tax=%d",
			n-1, cov.Invoice.FreeHits, cov.Tax.FreeHits)
	}
}

// TestApplyCoverage_IsolatedByLicenseAndSubject 免费期严格按 (客户, 主体, 类目)
// 隔离：A 客户查过的企业不能让 B 客户白拿，同客户查过的企业也不能覆盖别的企业。
func TestApplyCoverage_IsolatedByLicenseAndSubject(t *testing.T) {
	s := New()
	mustApply(t, s, req("r1", model.AllDims()))

	other := req("r2", model.AllDims())
	other.LicenseID = "LIC-OTHER"
	if got := chargedOf(t, mustApply(t, s, other)); got != model.AllDims() {
		t.Errorf("另一客户查同一主体应照常计费, got %s", got.String())
	}

	otherCode := req("r3", model.AllDims())
	otherCode.CreditCode = "91110000MA0000000X"
	if got := chargedOf(t, mustApply(t, s, otherCode)); got != model.AllDims() {
		t.Errorf("同客户查另一主体应照常计费, got %s", got.String())
	}

	otherRoute := req("r4", model.AllDims())
	otherRoute.Route = "other"
	if got := chargedOf(t, mustApply(t, s, otherRoute)); got != model.AllDims() {
		t.Errorf("另一路由应独立计费, got %s", got.String())
	}
}

// TestRecordCharge_Idempotent 同一 reqid 重放（对账补记/重试）不得产生第二笔应收。
func TestRecordCharge_Idempotent(t *testing.T) {
	s := New()
	ctx := context.Background()
	c := &model.SubjectCharge{
		LicenseID: testLic, Route: "swfp", CreditCode: testCode, Reqid: "r1",
		Charged: model.AllDims(), FeeStandard: model.FeeBoth, AmountFen: 1000,
	}
	for i := 0; i < 3; i++ {
		if err := s.RecordCharge(ctx, c); err != nil {
			t.Fatalf("RecordCharge #%d: %v", i, err)
		}
	}
	list, err := s.ListCharges(ctx, testLic, 0)
	if err != nil {
		t.Fatalf("ListCharges: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("计费流水条数=%d, want 1（幂等）", len(list))
	}
}

// TestCountActiveCoverage 按企业去重：同一主体的两个类目算一家。
func TestCountActiveCoverage(t *testing.T) {
	s := New()
	ctx := context.Background()
	mustApply(t, s, req("r1", model.AllDims()))
	second := req("r2", model.AllDims())
	second.CreditCode = "91110000MA0000000X"
	mustApply(t, s, second)

	n, err := s.CountActiveCoverage(ctx, testLic, "swfp")
	if err != nil {
		t.Fatalf("CountActiveCoverage: %v", err)
	}
	if n != 2 {
		t.Errorf("免费期内主体数=%d, want 2", n)
	}
}

// TestAddInterval 窗口必须是日历语义（周年制），不是固定 8760 小时——否则闰年会
// 差一天，客户在 2 月 29 日附近会发现被提前收费。
func TestAddInterval(t *testing.T) {
	// 2024 是闰年：2024-01-01 + 1 year 必须是 2025-01-01（跨过 366 天），
	// 而非 2024-12-31（+8760h 的结果）。
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := addInterval(base, "1 year"); !got.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("闰年 +1 year = %v, want 2025-01-01", got)
	}
	// 周年制而非日历自然年：12 月的查询免到次年 12 月，不是当年 12-31。
	dec := time.Date(2026, 12, 28, 10, 0, 0, 0, time.UTC)
	if got := addInterval(dec, "1 year"); got.Year() != 2027 || got.Month() != time.December || got.Day() != 28 {
		t.Errorf("2026-12-28 +1 year = %v, want 2027-12-28", got)
	}
	if got := addInterval(base, ""); !got.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("空 interval 应退回 1 年, got %v", got)
	}
	if got := addInterval(base, "6 months"); got.Month() != time.July {
		t.Errorf("6 months = %v, want 7 月", got)
	}
}

func mustApply(t *testing.T, s *Store, r model.CoverageRequest) *model.CoverageResult {
	t.Helper()
	res, err := s.ApplyCoverage(context.Background(), r)
	if err != nil {
		t.Fatalf("ApplyCoverage(%s): %v", r.Reqid, err)
	}
	return res
}
