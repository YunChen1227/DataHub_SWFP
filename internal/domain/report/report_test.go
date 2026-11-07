package report

import (
	"context"
	"testing"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/persistence/memory"
)

func ptrB(b bool) *bool     { return &b }
func ptrS(s string) *string { return &s }

var day = time.Date(2026, 10, 10, 10, 0, 0, 0, beijing)

func seed(t *testing.T) *memory.Store {
	t.Helper()
	s := memory.New()
	s.SeedLicense(&model.LicenseView{LicenseID: "L1", AppKey: "ak1", Status: "ACTIVE"}, "x", "客户A", "")
	s.SeedLicense(&model.LicenseView{LicenseID: "L2", AppKey: "ak2", Status: "ACTIVE"}, "x", "客户B", "")
	ctx := context.Background()

	audit := func(id, appKey, credit, reqScope, dataScope string, found bool, state model.ChargeState, charged string, at time.Time) {
		if err := s.AppendAudit(ctx, &model.AuditRecord{
			RequestID: id, Version: "swfp", AppKey: appKey, Reqid: id, CreditCode: credit,
			FoundData: found, ReqScope: reqScope, DataScope: dataScope,
			FeeStandard: "none", ChargeState: state, ChargedScope: charged, CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	payload := func(id string, inv, tax *bool, invAmt, taxAmt string) {
		var ia, ta *string
		if invAmt != "" {
			ia = ptrS(invAmt)
		}
		if taxAmt != "" {
			ta = ptrS(taxAmt)
		}
		if err := s.AppendPayload(ctx, &model.RequestPayload{RequestID: id,
			ZeroVerdict: model.ZeroVerdict{InvZero: inv, TaxZero: tax, InvAmt12m: ia, TaxAmt12m: ta},
			ResponseRange: []byte(`{"发票数据聚合":{"nsrjbxx":{"nsrmc":"企业` + id + `"}}}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 税号 T1：本月查 3 次发票，首次 CHARGED 有金额，后两次 FREE —— 只计 1 个本月收费
	audit("a1", "ak1", "T1", "invoice", "invoice", true, model.ChargeCharged, "invoice", day)
	payload("a1", ptrB(false), nil, "100.00", "")
	audit("a2", "ak1", "T1", "invoice", "invoice", true, model.ChargeFree, "none", day.Add(time.Hour))
	payload("a2", ptrB(false), nil, "100.00", "")
	audit("a3", "ak1", "T1", "invoice", "invoice", true, model.ChargeFree, "none", day.Add(2*time.Hour))
	payload("a3", ptrB(false), nil, "100.00", "")

	// 税号 T2：查得但金额为 0
	audit("b1", "ak1", "T2", "invoice", "invoice", true, model.ChargeZero, "none", day)
	payload("b1", ptrB(true), nil, "0.00", "")

	// 税号 T3：查无
	audit("c1", "ak1", "T3", "tax", "", false, model.ChargeNoCharge, "none", day)

	// 税号 T4：查得无 payload（待人工）
	audit("d1", "ak1", "T4", "tax", "tax", true, model.ChargeCharged, "tax", day)

	// 税号 T5：有金额，命中免费期（前期已收费）
	audit("e1", "ak1", "T5", "tax", "tax", true, model.ChargeFree, "none", day)
	payload("e1", nil, ptrB(false), "", "50.00")

	// 税号 T6：客户B，税务本月收费
	audit("g1", "ak2", "T6", "tax", "tax", true, model.ChargeCharged, "tax", day)
	payload("g1", nil, ptrB(false), "", "8.00")

	// 区间外
	audit("f1", "ak1", "T9", "invoice", "invoice", true, model.ChargeCharged, "invoice", day.AddDate(0, -2, 0))
	payload("f1", ptrB(false), nil, "1.00", "")

	// 上游调用：证通对 T1 发票 ok；T2 发票 ok 金额0；T3 税 empty；另客户对同一税号 U1 的历史计费点在区间外
	call := func(id, label, provider, status, appKey string, dims model.DimSet, at time.Time) *model.UpstreamCallRecord {
		return &model.UpstreamCallRecord{SourceCall: model.SourceCall{Label: label, Source: label, Provider: provider,
			Status: status, Dims: dims, UID: "uid-" + id}, AppKey: appKey, Version: "swfp", Reqid: id,
			RequestID: id, CreatedAt: at}
	}
	if err := s.AppendUpstreamCalls(ctx, []*model.UpstreamCallRecord{
		call("a1", "invoice2", "entcredit", model.CallOK, "ak1", model.DimSet{Invoice: true}, day),
		call("b1", "invoice2", "entcredit", model.CallOK, "ak1", model.DimSet{Invoice: true}, day),
		call("c1", "tax2", "entcredit", model.CallEmpty, "ak1", model.DimSet{Tax: true}, day),
		call("g1", "tax2", "entcredit", model.CallOK, "ak2", model.DimSet{Tax: true}, day),
		// 跨客户：U1 在 9 月已被 ak2 查得计费，10 月 ak1 再查应落免费期
		call("u0", "tax2", "entcredit", model.CallOK, "ak2", model.DimSet{Tax: true}, day.AddDate(0, -1, 0)),
		call("u1", "tax2", "entcredit", model.CallOK, "ak1", model.DimSet{Tax: true}, day),
	}); err != nil {
		t.Fatal(err)
	}
	audit("u0", "ak2", "U1", "tax", "tax", true, model.ChargeCharged, "tax", day.AddDate(0, -1, 0))
	payload("u0", nil, ptrB(false), "", "9.00")
	audit("u1", "ak1", "U1", "tax", "tax", true, model.ChargeFree, "none", day)
	payload("u1", nil, ptrB(false), "", "9.00")
	// 给上游 payload 源级零判定
	_ = s.AppendPayload(ctx, &model.RequestPayload{RequestID: "a1", Sources: []*model.UpstreamPayload{
		{SourceLabel: "invoice2", ZeroVerdict: model.ZeroVerdict{InvAmt12m: ptrS("100.00"), InvZero: ptrB(false)}},
	}})
	_ = s.AppendPayload(ctx, &model.RequestPayload{RequestID: "b1", Sources: []*model.UpstreamPayload{
		{SourceLabel: "invoice2", ZeroVerdict: model.ZeroVerdict{InvAmt12m: ptrS("0.00"), InvZero: ptrB(true)}},
	}})
	_ = s.AppendPayload(ctx, &model.RequestPayload{RequestID: "g1", Sources: []*model.UpstreamPayload{
		{SourceLabel: "tax2", ZeroVerdict: model.ZeroVerdict{TaxAmt12m: ptrS("8.00"), TaxZero: ptrB(false)}},
	}})
	_ = s.AppendPayload(ctx, &model.RequestPayload{RequestID: "u0", Sources: []*model.UpstreamPayload{
		{SourceLabel: "tax2", ZeroVerdict: model.ZeroVerdict{TaxAmt12m: ptrS("9.00"), TaxZero: ptrB(false)}},
	}})
	_ = s.AppendPayload(ctx, &model.RequestPayload{RequestID: "u1", Sources: []*model.UpstreamPayload{
		{SourceLabel: "tax2", ZeroVerdict: model.ZeroVerdict{TaxAmt12m: ptrS("9.00"), TaxZero: ptrB(false)}},
	}})

	// 开 T5 的免费期窗口（前期已收费）
	_, _ = s.ApplyCoverage(ctx, model.CoverageRequest{
		LicenseID: "L1", Route: "swfp", CreditCode: "T5", Got: model.DimSet{Tax: true},
		Reqid: "prev", RequestID: "prev-T5", Window: "1 year",
	})
	return s
}

func svc(s *memory.Store) *Service {
	r := New("swfp", s)
	r.now = func() time.Time { return day }
	return r
}

func findSheet(t *testing.T, names []string, want string) {
	t.Helper()
	for _, n := range names {
		if n == want {
			return
		}
	}
	t.Fatalf("缺少 sheet %q in %v", want, names)
}

func summaryValue(t *testing.T, rows [][]any, label string) int {
	t.Helper()
	for _, r := range rows {
		if len(r) > 1 && r[0] == label {
			n, ok := r[1].(int)
			if !ok {
				t.Fatalf("%s 值类型不符: %v", label, r[1])
			}
			return n
		}
	}
	t.Fatalf("汇总里缺少 %q", label)
	return 0
}

func TestDownstreamByTaxNumber(t *testing.T) {
	s := seed(t)
	from, to, err := ParseRange("2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatal(err)
	}
	wb, err := svc(s).Downstream(context.Background(), "客户A", []string{"ak1"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(wb.Sheets))
	for _, sh := range wb.Sheets {
		names = append(names, sh.Name)
	}
	for _, want := range []string{"汇总", "本月收费", "金额为0", "未查得", "免费期内", "数据库无记录（待人工）", "税号明细", "请求明细"} {
		findSheet(t, names, want)
	}

	sum := wb.Sheets[0].Rows
	if n := summaryValue(t, sum, "查询了的税号总数"); n != 6 { // T1-T5 + U1
		t.Fatalf("查询税号=%d, want 6", n)
	}
	if n := summaryValue(t, sum, "查得了的税号总数"); n != 5 { // T1 T2 T4 T5 U1
		t.Fatalf("查得=%d, want 5", n)
	}
	if n := summaryValue(t, sum, "未查得的税号总数"); n != 1 {
		t.Fatalf("未查得=%d, want 1", n)
	}
	if n := summaryValue(t, sum, "查得成功但金额为0的税号总数"); n != 1 {
		t.Fatalf("金额为0=%d, want 1", n)
	}
	if n := summaryValue(t, sum, "本月需要收费的税号总数"); n != 1 { // 仅 T1
		t.Fatalf("本月收费=%d, want 1（T1 三次只计一次）", n)
	}
	if n := summaryValue(t, sum, "免费期内（前期已收费）的税号数"); n != 2 { // T5 + U1
		t.Fatalf("免费期内=%d, want 2", n)
	}
	if n := summaryValue(t, sum, "无原始数据（待人工）的税号数"); n != 1 {
		t.Fatalf("待人工=%d, want 1", n)
	}
	found := summaryValue(t, sum, "查得了的税号总数")
	zero := summaryValue(t, sum, "查得成功但金额为0的税号总数")
	bill := summaryValue(t, sum, "本月需要收费的税号总数")
	free := summaryValue(t, sum, "免费期内（前期已收费）的税号数")
	manual := summaryValue(t, sum, "无原始数据（待人工）的税号数")
	if found != zero+bill+free+manual {
		t.Fatalf("恒等式不成立: %d != %d+%d+%d+%d", found, zero, bill, free, manual)
	}

	// 税号明细一行一税号；T1 请求次数=3
	var detail [][]any
	for _, sh := range wb.Sheets {
		if sh.Name == "税号明细" {
			detail = sh.Rows
		}
	}
	if len(detail) != 6 {
		t.Fatalf("税号明细 %d 行, want 6", len(detail))
	}
	var t1 []any
	for _, r := range detail {
		if r[0] == "T1" {
			t1 = r
		}
	}
	if t1 == nil || t1[5] != 3 {
		t.Fatalf("T1 请求次数应为 3: %v", t1)
	}
	if t1[14] != verdictLabel[verdictBillable] {
		t.Fatalf("T1 结论应为本月收费: %v", t1[14])
	}
}

func TestUpstreamAnnualWindowAcrossCustomers(t *testing.T) {
	s := seed(t)
	from, to, _ := ParseRange("2026-10-01", "2026-10-31")
	v, _ := VendorByID("zhengtong")
	wb, err := svc(s).Upstream(context.Background(), v, from, to)
	if err != nil {
		t.Fatal(err)
	}
	sum := wb.Sheets[0].Rows
	bill := summaryValue(t, sum, "本月需要收费的总数")
	free := summaryValue(t, sum, "免费期内（前期已收费）")
	zero := summaryValue(t, sum, "查得成功但金额为0的总数")
	// a1 发票本月计费、b1 发票金额0、g1 税务本月计费、u1 税务免费期（u0 在区间外开窗）
	if bill < 2 {
		t.Fatalf("本月收费=%d, want >=2 (T1发票 + T6税务)", bill)
	}
	if free < 1 {
		t.Fatalf("免费期内=%d, want >=1 (U1 跨客户一年窗口)", free)
	}
	if zero < 1 {
		t.Fatalf("金额为0=%d, want >=1", zero)
	}
	var failSheet bool
	for _, sh := range wb.Sheets {
		if sh.Name == "查无失败调用" && len(sh.Rows) >= 1 {
			failSheet = true
		}
	}
	if !failSheet {
		t.Fatal("应有查无失败调用 sheet")
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
