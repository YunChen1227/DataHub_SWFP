package bill24

import (
	"math/big"
	"testing"
	"time"
)

func TestIfaceOfScope(t *testing.T) {
	for in, want := range map[string]Iface{"invoice": IfaceInvoice, "tax": IfaceTax, "invoice+tax": IfaceBoth} {
		if got, ok := ifaceOfScope(in); !ok || got != want {
			t.Errorf("ifaceOfScope(%q) = %q,%v", in, got, ok)
		}
	}
	if _, ok := ifaceOfScope(""); ok {
		t.Error("空 scope 应无法识别")
	}
}

func TestDedupeKeepsFirstPerCreditAndIface(t *testing.T) {
	t0 := time.Date(2026, 9, 10, 8, 0, 0, 0, cst)
	items := dedupe([]Item{
		{Credit: "A", Iface: IfaceTax, At: t0.Add(time.Hour)},
		{Credit: "A", Iface: IfaceTax, At: t0},
		{Credit: "A", Iface: IfaceInvoice, At: t0.Add(2 * time.Hour)},
		{Credit: "", Iface: IfaceTax, At: t0},
	})
	if len(items) != 2 || !items[0].At.Equal(t0) || items[1].Iface != IfaceInvoice {
		t.Fatalf("dedupe = %+v", items)
	}
}

func TestJudgeByIfaceScope(t *testing.T) {
	r := Result{Item: Item{Iface: IfaceInvoice}}
	judge(&r, big.NewRat(0, 1), big.NewRat(500, 1))
	if r.Outcome != OutcomeZero || r.Tax != "" {
		t.Fatalf("只调用票只看开票金额: %+v", r)
	}
	r = Result{Item: Item{Iface: IfaceBoth}}
	judge(&r, big.NewRat(-10, 1), big.NewRat(11, 1))
	if r.Outcome != OutcomeBill || r.Total != "1.00" {
		t.Fatalf("票税看合计: %+v", r)
	}
}
