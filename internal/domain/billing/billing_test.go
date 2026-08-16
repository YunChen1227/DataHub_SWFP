package billing

import (
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

// TestDecide_BillingScope verifies the口径: 成功查得数 only counts 查得数据 (001);
// 999 查无结果 is Resolved (确定结论 → BILLED) but NOT Returned (不累计查得数).
func TestDecide_BillingScope(t *testing.T) {
	svc := New(DefaultTable())

	cases := []struct {
		name         string
		code         string
		wantResolved bool // 上游确定结论 → 台账 BILLED
		wantReturned bool // 查得数据 → 累计成功查得数
	}{
		{"001 查得数据", "001", true, true},
		{"999 查无结果", "999", true, false},
		{"003 我方原因失败", "003", false, false},
		{"012 接口错误", "012", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := svc.Decide(&model.UpstreamResult{Code: c.code}, model.FeeRates{})
			if d.Resolved != c.wantResolved {
				t.Errorf("code=%s Resolved(确定结论)=%v, want %v", c.code, d.Resolved, c.wantResolved)
			}
			if d.Returned != c.wantReturned {
				t.Errorf("code=%s Returned(成功查得数)=%v, want %v", c.code, d.Returned, c.wantReturned)
			}
		})
	}
}

// TestDecide_StandardByGotDimensions 锁定新计费口径：收费标准只看【实际查得的维度】，
// 与下游请求了什么无关——请求两项只查得发票就按【单发票】收费。
func TestDecide_StandardByGotDimensions(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(model.FeeRates{BothFen: 1000, InvoiceFen: 600, TaxFen: 500})

	cases := []struct {
		name         string
		code         string
		got          model.DimSet
		wantStandard model.FeeStandard
		wantAmount   int64
	}{
		{"两项皆得", "001", model.AllDims(), model.FeeBoth, 1000},
		{"仅发票", "001", model.DimSet{Invoice: true}, model.FeeInvoice, 600},
		{"仅税务", "001", model.DimSet{Tax: true}, model.FeeTax, 500},
		{"查无不计费", "999", model.DimSet{}, model.FeeNone, 0},
		{"部分源异常且无实得不计费", "002", model.DimSet{}, model.FeeNone, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := svc.Decide(&model.UpstreamResult{Code: c.code, Got: c.got}, model.FeeRates{})
			if d.Standard != c.wantStandard {
				t.Errorf("Standard=%q, want %q", d.Standard, c.wantStandard)
			}
			if d.AmountFen != c.wantAmount {
				t.Errorf("AmountFen=%d, want %d", d.AmountFen, c.wantAmount)
			}
		})
	}
}

// TestDecide_LicenseRatesWinPerTier 客户合同价按档覆盖全局缺省；该客户未单独定价的
// 档位（0）仍走全局缺省，不能整份费率被一个 0 带偏。
func TestDecide_LicenseRatesWinPerTier(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(model.FeeRates{BothFen: 1000, InvoiceFen: 600, TaxFen: 500})
	contract := model.FeeRates{BothFen: 880} // 只谈了 both 档

	both := svc.Decide(&model.UpstreamResult{Code: "001", Got: model.AllDims()}, contract)
	if both.AmountFen != 880 {
		t.Errorf("both 档应走合同价 880, got %d", both.AmountFen)
	}
	inv := svc.Decide(&model.UpstreamResult{Code: "001", Got: model.DimSet{Invoice: true}}, contract)
	if inv.AmountFen != 600 {
		t.Errorf("未约定的 invoice 档应走全局缺省 600, got %d", inv.AmountFen)
	}
}

// TestDecide_CostIsCarriedEvenWhenNotBillable 上游成本与是否向下游计费无关：查无也
// 花了钱，必须带进结算（亏损单要在库里看得见）。
func TestDecide_CostIsCarriedEvenWhenNotBillable(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(model.FeeRates{BothFen: 1000})
	d := svc.Decide(&model.UpstreamResult{Code: "999", CostFen: 250}, model.FeeRates{})
	if d.AmountFen != 0 {
		t.Errorf("查无不应向下游计费, got %d", d.AmountFen)
	}
	if d.CostFen != 250 {
		t.Errorf("上游成本必须带出, got %d", d.CostFen)
	}
}

// TestFromRequery_KeepsExistingStandard 复查不带维度信息，故不重判档位：Standard 留空
// 由持久层"空值不覆盖"保住首次结算写下的标准/金额。
func TestFromRequery_KeepsExistingStandard(t *testing.T) {
	svc := New(DefaultTable()).WithDefaultRates(model.FeeRates{BothFen: 1000})
	d := svc.FromRequery(&model.RequeryResult{Reachable: true, Result: &model.UpstreamResult{Code: "001"}})
	if !d.Resolved || !d.Returned {
		t.Fatalf("复查到 001 应为确定结论且查得: %+v", d)
	}
	if d.Standard != "" || d.AmountFen != 0 {
		t.Fatalf("复查不应重判计费标准/金额, got %q/%d", d.Standard, d.AmountFen)
	}
}
