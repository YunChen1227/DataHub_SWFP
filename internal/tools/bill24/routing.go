package bill24

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	if !strings.EqualFold(customer, "ALL") {
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
	rowsAgg, err := fetchRouting(ctx, db, route, keys, span.From, span.To)
	if err != nil {
		return err
	}

	outDir := env("OUT_DIR", filepath.Join("test_res", "bill_found"))
	out := filepath.Join(outDir, fmt.Sprintf("%s-税号上游路由-%s.xlsx", label, span.Name))
	return writeRouting(out, label, span, rowsAgg)
}

func writeRouting(path, label string, span Month, rows []RouteRow) error {
	type key struct{ credit, prov string }
	agg := map[key]RouteRow{}
	seen := map[string]bool{}
	var credits []string
	for _, r := range rows {
		agg[key{r.Credit, r.Provider}] = r
		if !seen[r.Credit] {
			seen[r.Credit] = true
			credits = append(credits, r.Credit)
		}
	}
	sort.Strings(credits)

	wb := &xlsx.Workbook{}

	// 矩阵页：税号 × 上游。
	header := []string{"税号"}
	for _, p := range providerOrder {
		header = append(header, provName(p))
	}
	header = append(header, "命中上游", "命中上游数")
	matrix := make([][]any, 0, len(credits))
	provHit := map[string]int{}
	served := 0
	for _, credit := range credits {
		row := []any{credit}
		var hit []string
		called := 0
		for _, p := range providerOrder {
			r, ok := agg[key{credit, p}]
			if !ok {
				row = append(row, "")
				continue
			}
			called++
			row = append(row, cell(r))
			if r.OK > 0 {
				hit = append(hit, provName(p))
				provHit[p]++
			}
		}
		if len(hit) > 0 {
			served++
		}
		row = append(row, strings.Join(hit, "+"), called)
		matrix = append(matrix, row)
	}
	widths := []float64{22}
	for range providerOrder {
		widths = append(widths, 14)
	}
	widths = append(widths, 18, 10)
	ms := wb.Add("税号-上游矩阵", header, matrix, widths)
	ms.HeaderRows = map[int]bool{1: true}

	// 明细页：每个 (税号, 上游) 一行。
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Credit != rows[j].Credit {
			return rows[i].Credit < rows[j].Credit
		}
		return provIndex(rows[i].Provider) < provIndex(rows[j].Provider)
	})
	detail := make([][]any, 0, len(rows))
	for _, r := range rows {
		detail = append(detail, []any{
			r.Credit, provName(r.Provider), dims(r.Inv, r.Tax), r.OK,
			r.First.In(cst).Format(timeLayout), r.Last.In(cst).Format(timeLayout),
			r.AppKey, r.SampleReq,
		})
	}
	wb.Add("命中明细",
		[]string{"税号", "上游", "查得维度", "查得次数", "首次查得", "末次查得", "下游appKey", "示例requestId"},
		detail, []float64{22, 10, 10, 10, 20, 20, 16, 36})

	// 汇总页。
	sum := [][]any{
		{"税号上游路由汇总"},
		{"客户", label},
		{"区间", span.dateLabel()},
		{"查得税号数", len(credits)},
		{"至少命中一个上游的税号数", served},
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
	fmt.Printf("%s %s：税号 %d 个，至少命中一个上游 %d 个\n写出 %s\n", label, span.dateLabel(), len(credits), served, path)
	return nil
}

func provIndex(p string) int {
	for i, x := range providerOrder {
		if x == p {
			return i
		}
	}
	return len(providerOrder)
}

func dims(inv, tax bool) string {
	switch {
	case inv && tax:
		return "票税"
	case inv:
		return "票"
	case tax:
		return "税"
	}
	return ""
}

// cell 是矩阵单元格文案：命中写 ✓（含票/税维度），否则写调用结果。
func cell(r RouteRow) string {
	if r.OK > 0 {
		if d := dims(r.Inv, r.Tax); d != "" {
			return "✓" + d
		}
		return "✓"
	}
	if r.LastStatus == "" {
		return "调用"
	}
	return "✗" + r.LastStatus
}
