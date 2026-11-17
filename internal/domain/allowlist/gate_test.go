package allowlist

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/datahub/relay/internal/common/errs"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

func TestGateClosedAlwaysAllows(t *testing.T) {
	g := New(nil)
	if err := g.Check("91110108551385082Q", model.AllDims()); err != nil {
		t.Fatalf("关闭时应放行: %v", err)
	}
	if d := g.Allowed("91110108551385082Q", "entcredit", time.Now()); !d.Both() {
		t.Fatalf("关闭时 Allowed 应为全维度, got %s", d)
	}
}

func TestGateOpenRejectsUnknownAndWrongDim(t *testing.T) {
	g := New(nil)
	if err := g.LoadEntries([]*model.UpstreamAllowEntry{
		{CreditCode: "91110108551385082Q", Provider: "entcredit", Invoice: true},
	}); err != nil {
		t.Fatal(err)
	}
	_ = g.SetEnabled(context.Background(), true)

	if err := g.Check("913101165647980623", model.DimSet{Invoice: true}); err == nil {
		t.Fatal("不在名单应拒绝")
	} else if ae := errs.AsAppError(err); ae.Busi != errs.BusiServiceNotOpen {
		t.Fatalf("busi=%v", ae.Busi)
	}

	if err := g.Check("91110108551385082Q", model.DimSet{Tax: true}); err == nil {
		t.Fatal("维度不符应拒绝")
	}
	if err := g.Check("91110108551385082Q", model.DimSet{Invoice: true}); err != nil {
		t.Fatalf("匹配维度应放行: %v", err)
	}
	if d := g.Allowed("91110108551385082Q", "ctax", time.Now()); !d.Empty() {
		t.Fatalf("未授权上游应为空, got %s", d)
	}
	if d := g.Allowed("91110108551385082Q", "entcredit", time.Now()); d != (model.DimSet{Invoice: true}) {
		t.Fatalf("entcredit 仅票, got %s", d)
	}
}

func TestGatePersistsEnabled(t *testing.T) {
	store := memory.New()
	g := New(store)
	if err := g.SetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	v, err := store.GetSetting(context.Background(), SettingKeyEnabled)
	if err != nil || v != "true" {
		t.Fatalf("setting=%q err=%v", v, err)
	}
	g2 := New(store)
	if err := g2.LoadFromRepo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g2.Enabled() {
		t.Fatal("应从仓储恢复开启状态")
	}
}

func TestLoadProductionUpstreamAllowlistCSV(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	path := filepath.Join(root, "data", "upstream_allowlist.csv")
	entries, err := LoadCSV(path)
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}
	g := New(nil)
	if err := g.LoadEntries(entries); err != nil {
		t.Fatal(err)
	}
	st := g.Stats()
	if st.Total != 2687 {
		t.Fatalf("total=%d want 2687", st.Total)
	}
	if st.ByProvider["entcredit"] != 1583 || st.ByProvider["ctax"] != 451 ||
		st.ByProvider["sy"] != 635 || st.ByProvider["salesdata"] != 18 {
		t.Fatalf("byProvider=%v", st.ByProvider)
	}
	if st.TaxOnly != 740 || st.InvoiceOnly != 1938 || st.Both != 9 {
		t.Fatalf("dims tax=%d inv=%d both=%d", st.TaxOnly, st.InvoiceOnly, st.Both)
	}
	if st.UniqueCodes != 2661 {
		t.Fatalf("uniqueCodes=%d want 2661", st.UniqueCodes)
	}
}
