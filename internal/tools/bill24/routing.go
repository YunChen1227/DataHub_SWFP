package bill24

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/datahub/relay/internal/common/xlsx"
)

// providerOrder 是上游在矩阵里的固定列序；provName 是展示名。
var providerOrder = []string{"entcredit", "ctax", "sy", "salesdata"}

func provName(p string) string {
	if n, ok := upstreamNames[p]; ok {
		return n
	}
	return p
}

// RunRouting 输出「每个税号调用了哪些上游」。
//
// 环境变量：CUSTOMER（下游客户，缺省 北京联信；设为 ALL 则不分客户、全量统计）、
// FROM / TO（yyyy-MM-dd，含首尾，缺省 2026-09-01 / 2026-11-16）、
// OUT_DIR（缺省 test_res/bill_found）。
func RunRouting() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := connect(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close(ctx)

	route := env("ROUTE", "swfp")
	customer := env("CUSTOMER", "北京联信")
	var keys []string
	label := "全部客户"
	if strings.EqualFold(customer, "ALL") {
		label = "全部客户"
	} else {
		k, display, err := customerAppKeys(ctx, db, customer)
		if err != nil {
			return err
		}
		keys, label = k, display
		fmt.Printf("客户 %s  app_key=%v\n", display, keys)
	}

	span, err := parseDayRange(env("FROM", "2026-09-01"), env("TO", "2026-11-16"))
	if err != nil {
		return err
	}
	calls, err := fetchRouting(ctx, db, route, keys, span.From, span.To)
	if err != nil {
		return err
	}

	outDir := env("OUT_DIR", filepath.Join("test_res", "bill_found"))
	out := filepath.Join(outDir, fmt.Sprintf("%s-税号上游路由-%s.xlsx", label, span.Name))
	if err := writeRouting(out, label, span, calls); err != nil {
		return err
	}
	return nil
}

// routeAgg 汇总某税号对某上游的调用。
type routeAgg struct {
	ok, fail int
	inv, tax bool
	firstAt  time.Time
	lastSt   string
}

func writeRouting(path, label string, span Month, calls []RouteCall) error {
	// 按 税号 -> 上游 聚合。
	type key struct{ credit, prov string }
	agg := map[key]*routeAgg{}
	creditOrder := []string{}
	seenCredit := map[string]bool{}
	for _, c := range calls {
		if !seenCredit[c.Credit] {
			seenCredit[c.Credit] = true
			creditOrder = append(creditOrder, c.Credit)
		}
		k := key{c.Credit, c.Provider}
		a := agg[k]
		if a == nil {
			a = &routeAgg{firstAt: c.At}
			agg[k] = a
		}
		if c.At.Before(a.firstAt) {
			a.firstAt = c.At
		}
		if c.Status == "ok" {
			a.ok++
		} else {
			a.fail++
		}
		a.lastSt = c.Status
		a.inv = a.inv || c.Invoice
		a.tax = a.tax || c.Tax
	}
	sort.Strings(creditOrder)

	wb := &xlsx.Workbook{}

	// 矩阵页：税号 × 上游。单元格写命中状态。
	header := []string{"税号"}
	for _, p := range providerOrder {
		header = append(header, provName(p))
	}
	header = append(header, "命中上游", "调用上游数")
	matrix := make([][]any, 0, len(creditOrder))
	for _, credit := range creditOrder {
		row := []any{credit}
		var served []string
		called := 0
		for _, p := range providerOrder {
			a := agg[key{credit, p}]
			if a == nil {
				row = append(row, "")
				continue
			}
			called++
			row = append(row, cell(a))
			if a.ok > 0 {
				served = append(served, provName(p))
			}
		}
		row = append(row, strings.Join(served, "+"), called)
		matrix = append(matrix, row)
	}
	widths := []float64{22}
	for range providerOrder {
		widths = append(widths, 14)
	}
	widths = append(widths, 18, 10)
	ms := wb.Add("税号-上游矩阵", header, matrix, widths)
	ms.HeaderRows = map[int]bool{1: true}

	// 明细页：每次上游调用一行。
	detail := make([][]any, 0, len(calls))
	cs := make([]RouteCall, len(calls))
	copy(cs, calls)
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Credit != cs[j].Credit {
			return cs[i].Credit < cs[j].Credit
		}
		return cs[i].At.Before(cs[j].At)
	})
	for _, c := range cs {
		detail = append(detail, []any{
			c.Credit, provName(c.Provider), c.Status, flag(c.Invoice), flag(c.Tax),
			c.At.In(cst).Format(timeLayout), c.AppKey, c.RequestID,
		})
	}
	wb.Add("调用明细",
		[]string{"税号", "上游", "状态", "调用票", "调用税", "调用时间", "下游appKey", "requestId"},
		detail, []float64{22, 10, 10, 8, 8, 20, 16, 36})

	// 汇总页。
	total := len(creditOrder)
	servedCount := 0
	provHit := map[string]int{}
	for _, credit := range creditOrder {
		anyOK := false
		for _, p := range providerOrder {
			if a := agg[key{credit, p}]; a != nil && a.ok > 0 {
				provHit[p]++
				anyOK = true
			}
		}
		if anyOK {
			servedCount++
		}
	}
	sum := [][]any{
		{"税号上游路由汇总"},
		{"客户", label},
		{"区间", span.dateLabel()},
		{"去重税号数", total},
		{"至少命中一个上游的税号数", servedCount},
		{},
		{"各上游命中税号数（status=ok）"},
	}
	for _, p := range providerOrder {
		sum = append(sum, []any{provName(p), provHit[p]})
	}
	ss := wb.Add("汇总", nil, sum, []float64{30, 16})
	ss.HeaderRows = map[int]bool{1: true}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := save(path, wb); err != nil {
		return err
	}
	fmt.Printf("%s %s：税号 %d 个，至少命中一个上游 %d 个\n写出 %s\n", label, span.dateLabel(), total, servedCount, path)
	return nil
}

// cell 是矩阵单元格文案：命中写 ok（含票/税维度），否则写调用结果。
func cell(a *routeAgg) string {
	dims := ""
	switch {
	case a.inv && a.tax:
		dims = "票税"
	case a.inv:
		dims = "票"
	case a.tax:
		dims = "税"
	}
	if a.ok > 0 {
		if dims != "" {
			return "✓" + dims
		}
		return "✓"
	}
	if a.lastSt == "" {
		return "调用"
	}
	return "✗" + a.lastSt
}
