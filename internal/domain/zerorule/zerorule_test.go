package zerorule

import (
	"testing"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// 请求时间 2026-10-15（北京时间），窗口为 2025-10 .. 2026-10。
var at = time.Date(2026, 10, 15, 12, 0, 0, 0, beijing)

func amt(t *testing.T, label, raw string) Amounts {
	t.Helper()
	a := Extract(label, []byte(raw), at)
	if !a.Known {
		t.Fatalf("%s: 应有取数规则", label)
	}
	return a
}

func TestExtractPerSource(t *testing.T) {
	cases := []struct {
		name, label, raw string
		inv, tax         string
	}{
		{
			name: "证通发票：窗口外与无法解析的条目不计入", label: "invoice2",
			raw: `{"nsrfpxx":{"kphzxxList":[{"ssyf":"2026-05","kpje":"100.00"},{"ssyf":"2025-09","kpje":"999"},
			      {"ssyf":"2026-06","kpje":"null"},{"kpje":"5"}]}}`,
			inv: "100.00", tax: "0.00",
		},
		{
			name: "证通税务：各项正数合计", label: "tax2",
			raw: `{"nsrswxx":{"sbsjList":[{"sssjz":"2026-03-31","ysxssr":"1000","ynse":1.71}],
			      "zsbxxList":[{"sssjq":"2026-01-01","sjje":0}]}}`,
			inv: "0.00", tax: "1001.71",
		},
		{
			name: "证通税务：负数项不抵消其它项", label: "tax1",
			raw: `{"nsrswxx":{"sbsjList":[{"sssjz":"2026-03-31","ysxssr":"-50","ynse":"0"}],
			      "zsbxxList":[{"sssjz":"2026-03-31","sjje":"20"}]}}`,
			inv: "0.00", tax: "20.00",
		},
		{
			name: "凯盈云销项：yyyyMM 数字月份", label: "sales",
			raw: `{"salesInvoice":[{"belongMonth":202604,"invoiceAmtMonth":1234.56},{"belongMonth":202401,"invoiceAmtMonth":9}]}`,
			inv: "1234.56", tax: "0.00",
		},
		{
			name: "惠众税票C：发票与税务同时给", label: "ctax",
			raw: `{"kphzxx":[{"ssyf":"2026-05","kpje":"888.00"}],"sbsj":[{"sssjz":"2026-06-30","ynse":"13"}],
			      "zsbxx":[{"sssjz":"2026-06-30","sjje":"13"}]}`,
			inv: "888.00", tax: "26.00",
		},
		{
			name: "税友：kpyf 月份、ybtse 实缴", label: "sy",
			raw: `{"kphzxxList":[{"kpyf":"202605","kpje":1130.00}],"sbxxList":[{"skssqz":"2026-03-31","ysx":0,"ynse":0}],
			      "zsxxList":[{"skssqz":"2026-03-31","ybtse":0}]}`,
			inv: "1130.00", tax: "0.00",
		},
	}
	for _, c := range cases {
		a := amt(t, c.label, c.raw)
		if a.Invoice.FloatString(2) != c.inv || a.Tax.FloatString(2) != c.tax {
			t.Errorf("%s: inv=%s tax=%s, want inv=%s tax=%s", c.name, a.Invoice.FloatString(2), a.Tax.FloatString(2), c.inv, c.tax)
		}
	}
}

func TestJudgeAndMerge(t *testing.T) {
	zeroInv := amt(t, "invoice2", `{"nsrfpxx":{"kphzxxList":[{"ssyf":"2026-05","kpje":"0"}]}}`)
	posTax := amt(t, "tax2", `{"nsrswxx":{"sbsjList":[{"sssjz":"2026-05-31","ynse":"5"}]}}`)

	v := Judge(zeroInv, model.DimSet{Invoice: true})
	if v.InvZero == nil || !*v.InvZero || v.TaxZero != nil {
		t.Fatalf("单源发票为 0 判定不符: %+v", v)
	}

	m := Merge([]Source{
		{Amounts: zeroInv, Got: model.DimSet{Invoice: true}},
		{Amounts: posTax, Got: model.DimSet{Tax: true}},
	})
	if m.InvZero == nil || !*m.InvZero || m.TaxZero == nil || *m.TaxZero {
		t.Fatalf("请求级判定不符: inv=%v tax=%v", m.InvZero, m.TaxZero)
	}

	// 字段缺失按 0：查得发票但没有任何开票条目 → 发票为 0。
	empty := amt(t, "ctax", `{"nsrjbxx":{"nsrmc":"x"}}`)
	if v := Judge(empty, model.DimSet{Invoice: true, Tax: true}); !*v.InvZero || !*v.TaxZero {
		t.Fatalf("无条目应判为 0: %+v", v)
	}

	// 未知源：无从判定，请求级该维度也不下结论。
	unknown := Extract("mystery", []byte(`{}`), at)
	if unknown.Known {
		t.Fatal("未知段名不应有规则")
	}
	m = Merge([]Source{{Amounts: zeroInv, Got: model.DimSet{Invoice: true}}, {Amounts: unknown, Got: model.DimSet{Invoice: true}}})
	if m.InvZero != nil {
		t.Fatalf("含未知源时发票维度应无从判定, got %v", *m.InvZero)
	}
}

func TestWindowBoundaries(t *testing.T) {
	from, to := window(at)
	if from != 202510 || to != 202610 {
		t.Fatalf("window = %d..%d", from, to)
	}
	for in, want := range map[string]int{"2026-05": 202605, "202605": 202605, "2026-05-31": 202605, "20260531": 202605, "2026-13": 0, "abc": 0, "": 0} {
		if got := yearMonth(in); got != want {
			t.Errorf("yearMonth(%q) = %d, want %d", in, got, want)
		}
	}
}
