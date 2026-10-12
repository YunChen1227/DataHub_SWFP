package upstream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

var syUntil = mustRFC3339("2027-09-24T00:00:00+08:00")

func mustRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func loadCSV(t *testing.T, body string) *SyAllowlist {
	t.Helper()
	a, err := parseSyAllowlist(strings.NewReader(body), syUntil)
	if err != nil {
		t.Fatalf("parseSyAllowlist: %v", err)
	}
	return a
}

func TestParseSyAllowlistOK(t *testing.T) {
	a := loadCSV(t, "creditCode,tax,invoice,callTime\n"+
		"913101165647980623,1,1,2026-09-13T15:27:00+08:00\n"+
		"91320115MA1W153W81,1,0,2026-09-22T10:47:00+08:00\n"+
		"91130705MAE3BFRT5E,0,1,2026-09-24T14:15:00+08:00\n")
	st := a.Stats()
	if st.Total != 3 || st.Both != 1 || st.TaxOnly != 1 || st.InvoiceOnly != 1 {
		t.Fatalf("stats=%+v", st)
	}
	now := syUntil.Add(-time.Hour)
	if got := a.Allowed("91320115MA1W153W81", now); got != (model.DimSet{Tax: true}) {
		t.Fatalf("仅税企业允许维度=%s", got)
	}
	if got := a.Allowed(" 91320115ma1w153w81 ", now); got != (model.DimSet{Tax: true}) {
		t.Fatalf("归一化后应命中: %s", got)
	}
	if got := a.Allowed("91130705MAE3BFRT5E", now); got != (model.DimSet{Invoice: true}) {
		t.Fatalf("仅票企业允许维度=%s", got)
	}
	if got := a.Allowed("913101165647980623", now); !got.Both() {
		t.Fatalf("两者企业允许维度=%s", got)
	}
	if got := a.Allowed("91111111111111111X", now); !got.Empty() {
		t.Fatalf("不在白名单应为空: %s", got)
	}
}

func TestParseSyAllowlistRejects(t *testing.T) {
	until := syUntil
	cases := []struct {
		name string
		body string
	}{
		{"错表头", "code,tax,invoice,callTime\n913101165647980623,1,0,2026-09-13T15:27:00+08:00\n"},
		{"税号非法", "creditCode,tax,invoice,callTime\nNOT-A-CODE,1,0,2026-09-13T15:27:00+08:00\n"},
		{"重复税号", "creditCode,tax,invoice,callTime\n913101165647980623,1,0,2026-09-13T15:27:00+08:00\n913101165647980623,0,1,2026-09-13T16:00:00+08:00\n"},
		{"标志非法", "creditCode,tax,invoice,callTime\n913101165647980623,2,0,2026-09-13T15:27:00+08:00\n"},
		{"全为0", "creditCode,tax,invoice,callTime\n913101165647980623,0,0,2026-09-13T15:27:00+08:00\n"},
		{"时间非法", "creditCode,tax,invoice,callTime\n913101165647980623,1,0,2026-09-13\n"},
		{"空文件只有表头", "creditCode,tax,invoice,callTime\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSyAllowlist(strings.NewReader(tc.body), until); err == nil {
				t.Fatal("应拒绝")
			}
		})
	}
}

func TestLoadSyAllowlistUntilRequired(t *testing.T) {
	if _, err := LoadSyAllowlist("data/sy_allowlist.csv", time.Time{}); err == nil {
		t.Fatal("未配 allowUntil 应拒绝")
	}
}

func TestLoadSyAllowlistMissingFile(t *testing.T) {
	if _, err := LoadSyAllowlist("/no/such/sy_allowlist.csv", syUntil); err == nil {
		t.Fatal("文件缺失应拒绝")
	}
}

func TestAllowedPastCutoffIsEmpty(t *testing.T) {
	a := loadCSV(t, "creditCode,tax,invoice,callTime\n913101165647980623,1,1,2026-09-13T15:27:00+08:00\n")
	if got := a.Allowed("913101165647980623", syUntil); !got.Empty() {
		t.Fatalf("到点应停用, got %s", got)
	}
	if got := a.Allowed("913101165647980623", syUntil.Add(time.Second)); !got.Empty() {
		t.Fatalf("过点应停用, got %s", got)
	}
}

func TestGatedPortRejectsWithoutCallingInner(t *testing.T) {
	a := loadCSV(t, "creditCode,tax,invoice,callTime\n91320115MA1W153W81,1,0,2026-09-22T10:47:00+08:00\n")
	var hits int
	inner := okPort(&hits)
	now := func() time.Time { return syUntil.Add(-time.Hour) }
	g := NewGatedPort(inner, a, now)

	// 请求票，但白名单只允许税。
	_, err := g.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: "91320115MA1W153W81", Want: invoiceDim, Reqid: "g1",
	})
	if err == nil {
		t.Fatal("超出允许维度应拒绝")
	}
	if hits != 0 {
		t.Fatalf("被拒时不得发网络请求, hits=%d", hits)
	}

	// 空 Want 也拒。
	_, err = g.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: "91320115MA1W153W81", Reqid: "g2",
	})
	if err == nil || hits != 0 {
		t.Fatalf("空 Want 应拒绝且不调用, err=%v hits=%d", err, hits)
	}

	// 合法子集放行。
	if _, err := g.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: "91320115MA1W153W81", Want: taxDim, Reqid: "g3",
	}); err != nil {
		t.Fatalf("合法请求应放行: %v", err)
	}
	if hits != 1 {
		t.Fatalf("合法请求应调用一次, hits=%d", hits)
	}

	// 过了截止时间：即使维度匹配也不发请求。
	g2 := NewGatedPort(inner, a, func() time.Time { return syUntil })
	_, err = g2.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: "91320115MA1W153W81", Want: taxDim, Reqid: "g4",
	})
	if err == nil || hits != 1 {
		t.Fatalf("过截止应拒绝且不调用, err=%v hits=%d", err, hits)
	}
}

func TestLoadSyAllowlistProductionFile(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for {
		p := filepath.Join(dir, "data", "sy_allowlist.csv")
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("仓库根未找到 data/sy_allowlist.csv")
		}
		dir = parent
	}
	a, err := LoadSyAllowlist(path, syUntil)
	if err != nil {
		t.Fatalf("加载生产白名单: %v", err)
	}
	st := a.Stats()
	if st.Total != 295 || st.TaxOnly != 197 || st.InvoiceOnly != 90 || st.Both != 8 {
		t.Fatalf("白名单计数不符: %+v", st)
	}
}
