package report

import (
	"context"
	"testing"
	"time"

	"github.com/datahub/relay/internal/common/xlsx"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

func ptrB(b bool) *bool     { return &b }
func ptrS(s string) *string { return &s }

var day = time.Date(2026, 10, 10, 10, 0, 0, 0, beijing)

// seed 播种 7 条审计：
//
//	a 票税 有效（证通 tax2 源级税务为0）  b 票 发票为0（不计费；证通 invoice2 源级有数）      c 税 无 payload（历史数据）
//	d 票税 仅发票为0    e 查无（不入账单）          f 区间外        g 另一客户 税 有效
func seed(t *testing.T) *memory.Store {
	t.Helper()
	s := memory.New()
	s.SeedLicense(&model.LicenseView{LicenseID: "L1", AppKey: "ak1", Status: "ACTIVE"}, "x", "客户A", "")
	s.SeedLicense(&model.LicenseView{LicenseID: "L2", AppKey: "ak2", Status: "ACTIVE"}, "x", "客户B", "")
	ctx := context.Background()
	audit := func(id, appKey, scope string, found bool, at time.Time) {
		if err := s.AppendAudit(ctx, &model.AuditRecord{RequestID: id, Version: "swfp", AppKey: appKey, Reqid: id,
			FoundData: found, DataScope: scope, FeeStandard: "none", ChargeState: model.ChargeFree,
			CreditCode: "CODE-" + id, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	payload := func(id string, inv, tax *bool, sources ...*model.UpstreamPayload) {
		if err := s.AppendPayload(ctx, &model.RequestPayload{RequestID: id,
			ZeroVerdict: model.ZeroVerdict{InvZero: inv, TaxZero: tax}, Sources: sources}); err != nil {
			t.Fatal(err)
		}
	}
	audit("a", "ak1", "invoice+tax", true, day)
	payload("a", ptrB(false), ptrB(false),
		&model.UpstreamPayload{SourceLabel: "invoice2", ZeroVerdict: model.ZeroVerdict{InvAmt12m: ptrS("100.00"), InvZero: ptrB(false)}},
		&model.UpstreamPayload{SourceLabel: "tax2", ZeroVerdict: model.ZeroVerdict{TaxAmt12m: ptrS("0.00"), TaxZero: ptrB(true)}})
	audit("b", "ak1", "invoice", true, day)
	payload("b", ptrB(true), nil,
		&model.UpstreamPayload{SourceLabel: "invoice2", ZeroVerdict: model.ZeroVerdict{InvAmt12m: ptrS("8.00"), InvZero: ptrB(false)}})
	audit("c", "ak1", "tax", true, day)
	audit("d", "ak1", "invoice+tax", true, day)
	payload("d", ptrB(true), ptrB(false))
	audit("e", "ak1", "none", false, day)
	audit("f", "ak1", "invoice", true, day.AddDate(0, -2, 0))
	audit("g", "ak2", "tax", true, day)
	payload("g", nil, ptrB(false))

	call := func(id, label, provider, status string, dims model.DimSet) *model.UpstreamCallRecord {
		return &model.UpstreamCallRecord{SourceCall: model.SourceCall{Label: label, Source: label, Provider: provider,
			Status: status, Dims: dims, UID: "uid-" + id + label}, AppKey: "ak1", Version: "swfp", Reqid: id,
			RequestID: id, CreatedAt: day}
	}
	if err := s.AppendUpstreamCalls(ctx, []*model.UpstreamCallRecord{
		call("a", "invoice2", "entcredit", model.CallOK, model.DimSet{Invoice: true}),
		call("a", "tax2", "entcredit", model.CallOK, model.DimSet{Tax: true}),
		call("a", "sales", "salesdata", model.CallOK, model.DimSet{Invoice: true}),
		call("b", "invoice2", "entcredit", model.CallOK, model.DimSet{Invoice: true}),
		call("c", "tax2", "entcredit", model.CallEmpty, model.DimSet{Tax: true}),
		call("d", "tax1", "entcredit", model.CallSkipped, model.DimSet{Tax: true}),
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func svc(s *memory.Store) *Service {
	r := New("swfp", s)
	r.now = func() time.Time { return day }
	return r
}

func sheet(t *testing.T, wb *xlsx.Workbook, name string) *xlsx.Sheet {
	t.Helper()
	for _, sh := range wb.Sheets {
		if sh.Name == name {
			return sh
		}
	}
	t.Fatalf("缺少 sheet %q", name)
	return nil
}

// summaryRow 返回汇总 sheet 里首列为 label 的行。
func summaryRow(t *testing.T, wb *xlsx.Workbook, label string) []any {
	t.Helper()
	for _, r := range sheet(t, wb, "汇总").Rows {
		if len(r) > 0 && r[0] == label {
			return r
		}
	}
	t.Fatalf("汇总里缺少 %q 行", label)
	return nil
}

func TestDownstreamWorkbook(t *testing.T) {
	s := seed(t)
	from, to, err := ParseRange("2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatal(err)
	}
	wb, err := svc(s).Downstream(context.Background(), "客户A", []string{"ak1"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, sh := range wb.Sheets {
		names = append(names, sh.Name)
	}
	if len(names) != 4 || names[0] != "汇总" || names[1] != "票明细" || names[2] != "税明细" || names[3] != "票税明细" {
		t.Fatalf("sheet 不符: %v", names)
	}
	if n := len(sheet(t, wb, "票明细").Rows); n != 1 {
		t.Fatalf("票明细 %d 行, want 1", n)
	}
	if n := len(sheet(t, wb, "税明细").Rows); n != 1 {
		t.Fatalf("税明细 %d 行, want 1（不含另一客户）", n)
	}
	if n := len(sheet(t, wb, "票税明细").Rows); n != 2 {
		t.Fatalf("票税明细 %d 行, want 2", n)
	}
	// 列：类型, 查得, 建议计费, 零数据, 按单税, 按单票, 无原始数据, 无法判定, 免费期
	check := func(label string, want ...int) {
		r := summaryRow(t, wb, label)
		for i, w := range want {
			if r[i+1] != w {
				t.Errorf("%s 第 %d 列 = %v, want %d (row=%v)", label, i+1, r[i+1], w, r)
			}
		}
	}
	check("票", 1, 0, 1, 0, 0, 0, 0, 1)
	check("税", 1, 0, 0, 0, 0, 1, 0, 1)
	check("票税", 2, 1, 0, 1, 0, 0, 0, 2)

	// 全部客户：税多出客户B 的一条有效记录。
	all, err := svc(s).Downstream(context.Background(), "", nil, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r := summaryRow(t, all, "税"); r[1] != 2 || r[2] != 1 {
		t.Fatalf("全部客户税行不符: %v", r)
	}
}

func TestUpstreamWorkbook(t *testing.T) {
	s := seed(t)
	from, to, _ := ParseRange("2026-10-01", "2026-10-31")
	v, _ := VendorByID("zhengtong")
	wb, err := svc(s).Upstream(context.Background(), v, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// a：invoice2+tax2 合并为票税，tax2 源级税务为0 → 仅税务为0；b：票有效；c：查无进失败表；d：skipped 不计。
	if n := len(sheet(t, wb, "票税明细").Rows); n != 1 {
		t.Fatalf("票税明细 %d 行, want 1", n)
	}
	if n := len(sheet(t, wb, "票明细").Rows); n != 1 {
		t.Fatalf("票明细 %d 行, want 1", n)
	}
	if n := len(sheet(t, wb, "税明细").Rows); n != 0 {
		t.Fatalf("税明细 %d 行, want 0", n)
	}
	var fails *xlsx.Sheet
	for _, sh := range wb.Sheets {
		if sh.Name != "汇总" && sh.Name != "票明细" && sh.Name != "税明细" && sh.Name != "票税明细" {
			fails = sh
		}
	}
	if fails == nil || len(fails.Rows) != 1 {
		t.Fatalf("查无失败调用应 1 行: %+v", fails)
	}
	if r := summaryRow(t, wb, "票税"); r[1] != 1 || r[5] != 1 {
		t.Fatalf("票税汇总应为 1 条仅税务为0: %v", r)
	}
	if r := summaryRow(t, wb, "票"); r[1] != 1 || r[2] != 1 {
		t.Fatalf("票汇总应为 1 条有效: %v", r)
	}
}

func TestParseRange(t *testing.T) {
	from, to, err := ParseRange("2026-10-01", "2026-10-01")
	if err != nil || to.Sub(from) != 24*time.Hour {
		t.Fatalf("单日区间应含当天: %v %v %v", from, to, err)
	}
	if _, _, err := ParseRange("2026-10-02", "2026-10-01"); err == nil {
		t.Fatal("起始晚于截止应报错")
	}
	if _, _, err := ParseRange("2025-01-01", "2026-10-01"); err == nil {
		t.Fatal("超过 366 天应报错")
	}
	if _, _, err := ParseRange("2026/10/01", "2026-10-01"); err == nil {
		t.Fatal("日期格式错误应报错")
	}
}
