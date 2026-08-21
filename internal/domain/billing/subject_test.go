package billing

import (
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

var (
	dimNone  = model.DimSet{}
	dimInv   = model.DimSet{Invoice: true}
	dimTax   = model.DimSet{Tax: true}
	dimBoth  = model.AllDims()
	subRates = model.FeeRates{BothFen: 1000, InvoiceFen: 600, TaxFen: 500}
)

// TestChargeable 锁定主体年度计费的规范表述：应收类目 = 实得类目 − 免费期覆盖的类目。
// 尤其是第 3/4 行——「先查了一项，后来补查另一项」时只对补上的那一项计费，这是本次
// 改动最容易写错的地方。
func TestChargeable(t *testing.T) {
	cases := []struct {
		name         string
		got, covered model.DimSet
		want         model.DimSet
	}{
		{"首次两项皆得：全额", dimBoth, dimNone, dimBoth},
		{"两项都在免费期：全免", dimBoth, dimBoth, dimNone},
		{"发票已免、税务新得：只收税务", dimBoth, dimInv, dimTax},
		{"税务已免、发票新得：只收发票", dimBoth, dimTax, dimInv},
		{"只得发票且已免：全免", dimInv, dimInv, dimNone},
		{"只得发票，税务的免费期与本次无关", dimInv, dimTax, dimInv},
		{"只得税务且已免：全免", dimTax, dimTax, dimNone},
		{"查无：无从计费", dimNone, dimNone, dimNone},
		{"查无：免费期状态不影响结论", dimNone, dimBoth, dimNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Chargeable(c.got, c.covered); got != c.want {
				t.Errorf("Chargeable(got=%s, covered=%s)=%s, want %s",
					c.got.String(), c.covered.String(), got.String(), c.want.String())
			}
		})
	}
}

// mkResult 按逐类目的计费结论构造判定结果。charged 为 false 即该类目命中免费期。
func mkResult(verdicts map[model.Category]bool) *model.CoverageResult {
	out := &model.CoverageResult{}
	for _, cat := range []model.Category{model.CatInvoice, model.CatTax} {
		charged, ok := verdicts[cat]
		if !ok {
			continue // 该类目本次未实得，不参与判定
		}
		out.Verdicts = append(out.Verdicts, model.CategoryVerdict{Category: cat, Charged: charged})
	}
	return out
}

// TestApplyCoverage 是计费判定表：免费期扣减后按剩下的类目重算档位与金额。
// 关键在于**不新增定价档位**——补查单项时按该单项的合同价收，而不是 both 的差价。
func TestApplyCoverage(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(subRates)

	cases := []struct {
		name         string
		got          model.DimSet
		verdicts     map[model.Category]bool
		wantStandard model.FeeStandard
		wantAmount   int64
		wantState    model.ChargeState
		wantCharged  string
		wantCovered  string
	}{
		{
			name: "首次两项皆得：both 打包价",
			got:  dimBoth, verdicts: map[model.Category]bool{model.CatInvoice: true, model.CatTax: true},
			wantStandard: model.FeeBoth, wantAmount: 1000, wantState: model.ChargeCharged,
			wantCharged: "invoice+tax", wantCovered: "none",
		},
		{
			name: "两项都在免费期：查得了但不收钱",
			got:  dimBoth, verdicts: map[model.Category]bool{model.CatInvoice: false, model.CatTax: false},
			wantStandard: model.FeeNone, wantAmount: 0, wantState: model.ChargeFree,
			wantCharged: "none", wantCovered: "invoice+tax",
		},
		{
			name: "发票已免、税务补上：按 tax 单价而非 both 差价",
			got:  dimBoth, verdicts: map[model.Category]bool{model.CatInvoice: false, model.CatTax: true},
			wantStandard: model.FeeTax, wantAmount: 500, wantState: model.ChargeCharged,
			wantCharged: "tax", wantCovered: "invoice",
		},
		{
			name: "税务已免、发票补上：按 invoice 单价",
			got:  dimBoth, verdicts: map[model.Category]bool{model.CatInvoice: true, model.CatTax: false},
			wantStandard: model.FeeInvoice, wantAmount: 600, wantState: model.ChargeCharged,
			wantCharged: "invoice", wantCovered: "tax",
		},
		{
			name: "首次只得发票",
			got:  dimInv, verdicts: map[model.Category]bool{model.CatInvoice: true},
			wantStandard: model.FeeInvoice, wantAmount: 600, wantState: model.ChargeCharged,
			wantCharged: "invoice", wantCovered: "none",
		},
		{
			name: "只得发票且在免费期内",
			got:  dimInv, verdicts: map[model.Category]bool{model.CatInvoice: false},
			wantStandard: model.FeeNone, wantAmount: 0, wantState: model.ChargeFree,
			wantCharged: "none", wantCovered: "invoice",
		},
		{
			name: "首次只得税务",
			got:  dimTax, verdicts: map[model.Category]bool{model.CatTax: true},
			wantStandard: model.FeeTax, wantAmount: 500, wantState: model.ChargeCharged,
			wantCharged: "tax", wantCovered: "none",
		},
		{
			name: "只得税务且在免费期内",
			got:  dimTax, verdicts: map[model.Category]bool{model.CatTax: false},
			wantStandard: model.FeeNone, wantAmount: 0, wantState: model.ChargeFree,
			wantCharged: "none", wantCovered: "tax",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := svc.Decide(&model.UpstreamResult{Code: "001", Got: c.got}, model.FeeRates{})
			svc.ApplyCoverage(d, mkResult(c.verdicts), model.FeeRates{})
			if d.Standard != c.wantStandard {
				t.Errorf("Standard=%q, want %q", d.Standard, c.wantStandard)
			}
			if d.AmountFen != c.wantAmount {
				t.Errorf("AmountFen=%d, want %d", d.AmountFen, c.wantAmount)
			}
			if d.ChargeState != c.wantState {
				t.Errorf("ChargeState=%q, want %q", d.ChargeState, c.wantState)
			}
			if got := d.Charged.String(); got != c.wantCharged {
				t.Errorf("Charged=%q, want %q", got, c.wantCharged)
			}
			if got := d.Covered.String(); got != c.wantCovered {
				t.Errorf("Covered=%q, want %q", got, c.wantCovered)
			}
			// 不变式：应收 ∪ 免费 == 实得。判定结论必须完整覆盖本次拿到的类目，
			// 漏了就意味着某个类目既没收钱也没进免费期，下次会被重复计费。
			if union := d.Charged.Union(d.Covered); union != c.got {
				t.Errorf("Charged∪Covered=%s, want Got=%s", union.String(), c.got.String())
			}
		})
	}
}

// TestApplyCoverage_NoVerdictsKeepsDecision 查无(999)/部分源异常(002)/复查路径不带
// 类目信息，此时不得改动 Decide 已给出的标准与金额——复查靠持久层「空值不覆盖」
// 保住首次结算的值，这里一旦写成 none/0 就会把台账上的金额抹掉。
func TestApplyCoverage_NoVerdictsKeepsDecision(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(subRates)

	notFound := svc.Decide(&model.UpstreamResult{Code: "999"}, model.FeeRates{})
	svc.ApplyCoverage(notFound, nil, model.FeeRates{})
	if notFound.ChargeState != model.ChargeNoCharge || notFound.AmountFen != 0 {
		t.Errorf("查无应为 NOCHARGE/0, got %q/%d", notFound.ChargeState, notFound.AmountFen)
	}

	requery := svc.FromRequery(&model.RequeryResult{Reachable: true, Result: &model.UpstreamResult{Code: "001"}})
	svc.ApplyCoverage(requery, &model.CoverageResult{}, model.FeeRates{})
	if requery.Standard != "" || requery.AmountFen != 0 {
		t.Errorf("复查路径不得被重判档位, got %q/%d", requery.Standard, requery.AmountFen)
	}
}

// TestApplyCoverage_ContractRateWins 免费期扣减后重算金额时，客户合同价仍然优先于
// 全局缺省，且只对已约定的档位生效。
func TestApplyCoverage_ContractRateWins(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(subRates)
	contract := model.FeeRates{TaxFen: 450} // 只谈了 tax 档

	d := svc.Decide(&model.UpstreamResult{Code: "001", Got: dimBoth}, contract)
	svc.ApplyCoverage(d, mkResult(map[model.Category]bool{model.CatInvoice: false, model.CatTax: true}), contract)
	if d.AmountFen != 450 {
		t.Errorf("补查税务应走合同价 450, got %d", d.AmountFen)
	}

	d2 := svc.Decide(&model.UpstreamResult{Code: "001", Got: dimBoth}, contract)
	svc.ApplyCoverage(d2, mkResult(map[model.Category]bool{model.CatInvoice: true, model.CatTax: false}), contract)
	if d2.AmountFen != 600 {
		t.Errorf("未约定的 invoice 档应走全局缺省 600, got %d", d2.AmountFen)
	}
}

// TestMarkCoverageDeferred fail-closed：免费期库不可用时按 0 收并标 DEFERRED，
// 绝不 fail-open 足额收费——后者会让客户在对账时发现同一主体一年内被收了两次。
func TestMarkCoverageDeferred(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(subRates)
	d := svc.Decide(&model.UpstreamResult{Code: "001", Got: dimBoth}, model.FeeRates{})
	if d.AmountFen != 1000 {
		t.Fatalf("前置条件：毛口径应为 1000, got %d", d.AmountFen)
	}
	MarkCoverageDeferred(d)
	if d.ChargeState != model.ChargeDeferred {
		t.Errorf("ChargeState=%q, want DEFERRED", d.ChargeState)
	}
	if d.AmountFen != 0 || d.Standard != model.FeeNone {
		t.Errorf("判定失败必须按 0 收, got %q/%d", d.Standard, d.AmountFen)
	}
}
