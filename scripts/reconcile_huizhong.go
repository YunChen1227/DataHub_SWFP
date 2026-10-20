//go:build ignore

// reconcile_huizhong: 用我方库里的调用记录核对惠众 9 月账单（冠辰科技-月度对账单）（只读）。
// 账单里税、票、票税三类记录全部核对：我方是否调用过惠众「税票数据查询C」、查得还是查无，
// 这次请求还调了哪些上游、最终由哪个上游查得。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/reconcile_huizhong.go
//
// 可选环境变量：
//
//	MONTH=2026-09          账单所属北京时间自然月，用于「我方本月惠众调用」sheet
//	MATCH_SECONDS=120      账单时间与我方请求时间相差超过多少秒时在备注里标出（账单时间只精确到分钟）
//	OUT=test_res/xxx.xlsx  缺省 test_res/惠众账单对账-<月份>.xlsx
//
// 输出 sheet：总览、税明细、票明细、票税明细、我方本月惠众调用。
//
// 口径：
//   - 我方请求取 audit_log（按 credit_code 查，不限客户、不限时间），逐源调用取 upstream_call
//     （按 request_id 关联），status=skipped 表示该源没有发出请求。
//   - 账单「税」看覆盖税务维度的请求和调用，「票」看发票维度，「票税」两者都看。
//   - 每条账单记录匹配我方一次请求：优先取同税号、调用过惠众、时间差在 MATCH_SECONDS 以内的请求；
//     没有就取同税号同维度时间最近的请求。
//   - 最终成功上游 = 匹配到的请求里，第一个对该维度查得 (status=ok) 的上游。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

var cst = time.FixedZone("CST", 8*3600)

var billDims = []string{"税", "票", "票税"}

type exportConfig struct {
	Versions map[string]struct {
		Database struct {
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
			Name     string `yaml:"name"`
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			SSLMode  string `yaml:"sslmode"`
		} `yaml:"database"`
	} `yaml:"versions"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
	os.Exit(1)
}

type billRow struct {
	dim, code string
	at        time.Time
}

type request struct {
	id, code, reqScope, dataScope string
	appKey, customer              string
	at                            time.Time
	calls                         []upCall
}

type upCall struct {
	seq                    int32
	provider, label, alias string
	dimInv, dimTax         bool
	status                 string
	upCode, upMsg, upUID   string
	skipReason             string
	at                     time.Time
}

func (c upCall) covers(dim string) bool {
	switch dim {
	case "票":
		return c.dimInv
	case "税":
		return c.dimTax
	}
	return c.dimInv || c.dimTax
}

func (r *request) wants(dim string) bool {
	if r.reqScope == "" || r.reqScope == "none" {
		return true
	}
	switch dim {
	case "票":
		return strings.Contains(r.reqScope, "invoice")
	case "税":
		return strings.Contains(r.reqScope, "tax")
	}
	return true
}

// dimCalls 返回本次请求里覆盖该维度、真正发出的上游调用（按调用顺序）。
func (r *request) dimCalls(dim string) []upCall {
	var out []upCall
	for _, c := range r.calls {
		if c.status != "skipped" && c.covers(dim) {
			out = append(out, c)
		}
	}
	return out
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	month := env("MONTH", "2026-09")
	from, err := time.ParseInLocation("2006-01", month, cst)
	if err != nil {
		fail("MONTH 格式应为 YYYY-MM: %v", err)
	}
	to := from.AddDate(0, 1, 0)
	matchSec, err := strconv.ParseFloat(env("MATCH_SECONDS", "120"), 64)
	if err != nil {
		fail("MATCH_SECONDS 应为数字: %v", err)
	}
	out := env("OUT", filepath.Join("test_res", fmt.Sprintf("惠众账单对账-%s.xlsx", month)))

	bill := parseBill()
	codeSet := map[string]bool{}
	for _, b := range bill {
		codeSet[b.code] = true
	}
	codes := make([]string, 0, len(codeSet))
	for c := range codeSet {
		codes = append(codes, c)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg exportConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fail("解析配置失败: %v", err)
	}
	db := cfg.Versions["swfp"].Database
	if db.Host == "" {
		fail("配置里没有 versions.swfp.database")
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		fail("连接失败: %v", err)
	}
	defer conn.Close(ctx)
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '180s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}

	reqs := loadRequests(ctx, conn, `a.credit_code = ANY($1)`, codes)
	monthReqs := loadRequests(ctx, conn,
		`a.request_id IN (SELECT request_id FROM upstream_call WHERE provider='ctax' AND status<>'skipped' AND created_at >= $1 AND created_at < $2)`,
		from, to)
	loadUpstreamCalls(ctx, conn, append(append([]*request{}, reqs...), monthReqs...))
	byCode := map[string][]*request{}
	for _, r := range reqs {
		byCode[r.code] = append(byCode[r.code], r)
	}

	hzResults := []string{"查得", "查无", "失败", "未调用惠众", "我方无请求"}
	hzCount := map[string]map[string]int{}
	finalCount := map[string]map[string]int{}
	for _, d := range billDims {
		hzCount[d], finalCount[d] = map[string]int{}, map[string]int{}
	}
	finalNames := map[string]bool{}
	mismatch := map[string]int{}
	details := map[string][][]any{}
	matched := map[string]bool{}

	for _, b := range bill {
		var cands []*request
		for _, r := range byCode[b.code] {
			if r.wants(b.dim) {
				cands = append(cands, r)
			}
		}
		best, diff := pickRequest(cands, b, matchSec)
		history := historySummary(cands, b.dim)

		if best == nil {
			hzCount[b.dim]["我方无请求"]++
			finalCount[b.dim]["我方无请求"]++
			finalNames["我方无请求"] = true
			details[b.dim] = append(details[b.dim], []any{
				b.code, b.at.Format("2006-01-02 15:04"), nil, nil, nil,
				"我方无请求", nil, nil, nil, nil, "我方无请求", nil, nil, nil, nil, int64(0), history,
				"所有客户都没有请求过这个税号的该维度",
			})
			continue
		}
		matched[best.id] = true

		calls := best.dimCalls(b.dim)
		hzResult, hzCode, hzMsg, hzUID := "未调用惠众", "", "", ""
		var chain []string
		final := ""
		for _, c := range calls {
			chain = append(chain, fmt.Sprintf("%s:%s", providerName(c), statusName(c.status)))
			if c.provider == "ctax" {
				hzResult, hzCode, hzMsg, hzUID = statusName(c.status), c.upCode, c.upMsg, c.upUID
			}
			if final == "" && c.status == "ok" {
				final = providerName(c)
			}
		}
		switch {
		case final != "":
		case len(calls) == 0:
			final = "未调用任何上游"
		default:
			final = "无（上游均未查得）"
		}
		note := ""
		if diff > matchSec {
			note = fmt.Sprintf("时间相差超过 %g 秒，可能不是同一次请求", matchSec)
			mismatch[b.dim]++
		}
		if hzResult == "未调用惠众" {
			if s := skipReason(best, b.dim, "ctax"); s != "" {
				note = strings.TrimPrefix(note+"；惠众未调用原因："+s, "；")
			}
		}
		hzCount[b.dim][hzResult]++
		finalCount[b.dim][final]++
		finalNames[final] = true
		details[b.dim] = append(details[b.dim], []any{
			b.code, b.at.Format("2006-01-02 15:04"),
			best.at.Format("2006-01-02 15:04:05"), int64(math.Round(diff)), scopeName(best.reqScope),
			hzResult, hzCode, hzMsg, hzUID, strings.Join(chain, " → "), final, scopeName(best.dataScope),
			best.customer, best.appKey, best.id, int64(len(cands)), history, note,
		})
	}

	// 我方本月全部惠众调用，标记是否被账单记录匹配到。
	var monthRows [][]any
	monthStat := map[string]map[string]int{}
	for _, r := range monthReqs {
		for _, c := range r.calls {
			if c.provider != "ctax" || c.status == "skipped" {
				continue
			}
			typ := hzType(r.reqScope)
			if monthStat[typ] == nil {
				monthStat[typ] = map[string]int{}
			}
			monthStat[typ][statusName(c.status)]++
			monthStat[typ]["合计"]++
			inBill := "否"
			if matched[r.id] {
				inBill = "是"
			}
			monthRows = append(monthRows, []any{
				c.at.Format("2006-01-02 15:04:05"), r.code, typ, statusName(c.status), c.upCode, c.upMsg, c.upUID,
				r.customer, r.appKey, r.id, inBill,
			})
		}
	}
	sort.Slice(monthRows, func(i, j int) bool { return monthRows[i][0].(string) < monthRows[j][0].(string) })

	overview := [][]any{{"一、惠众账单逐条核对：我方惠众调用结果", "", "", "", "", "", ""},
		{"账单类型", "账单条数", "查得", "查无", "失败", "未调用惠众", "我方无请求"}}
	hdr := map[int]bool{0: true, 1: true}
	tot := make([]int64, len(hzResults)+1)
	for _, dim := range billDims {
		row := []any{dim, int64(len(details[dim]))}
		tot[0] += int64(len(details[dim]))
		for i, r := range hzResults {
			row = append(row, int64(hzCount[dim][r]))
			tot[i+1] += int64(hzCount[dim][r])
		}
		overview = append(overview, row)
	}
	totRow := []any{"合计"}
	for _, v := range tot {
		totRow = append(totRow, v)
	}
	overview = append(overview, totRow, []any{})

	names := make([]string, 0, len(finalNames))
	for n := range finalNames {
		names = append(names, n)
	}
	sort.Strings(names)
	overview = append(overview, []any{"二、最终成功上游", ""})
	hdr[len(overview)-1] = true
	overview = append(overview, append([]any{"账单类型"}, toAny(names)...))
	hdr[len(overview)-1] = true
	for _, dim := range billDims {
		row := []any{dim}
		for _, n := range names {
			row = append(row, int64(finalCount[dim][n]))
		}
		overview = append(overview, row)
	}

	overview = append(overview, []any{}, []any{"三、我方 " + from.Format("2006年1月") + " 实际调用惠众（按请求类型）", ""})
	hdr[len(overview)-1] = true
	overview = append(overview, []any{"惠众 type", "合计", "查得", "查无", "失败"})
	hdr[len(overview)-1] = true
	for _, typ := range []string{hzType("tax"), hzType("invoice"), hzType("invoice+tax")} {
		m := monthStat[typ]
		overview = append(overview, []any{typ, int64(m["合计"]), int64(m["查得"]), int64(m["查无"]), int64(m["失败"])})
	}

	overview = append(overview,
		[]any{},
		[]any{"时间对不上", fmt.Sprintf("税 %d 条，票 %d 条，票税 %d 条（相差超过 %g 秒，仍按最近一次请求判定）",
			mismatch["税"], mismatch["票"], mismatch["票税"], matchSec)},
		[]any{"口径", "我方请求不限客户、不限时间；只看覆盖该维度且真正发出的上游调用（排除 skipped）"},
		[]any{"", "优先匹配调用过惠众、时间差在阈值内的请求，否则取时间最近的请求"},
		[]any{"", "查得 = 上游返回有数据；查无 = 上游返回无数据；失败 = 超时、网关错误或上游错误码"},
		[]any{"", "最终成功上游 = 该请求里第一个对该维度查得的上游"},
		[]any{"", "生成时间 " + time.Now().In(cst).Format("2006-01-02 15:04:05")},
	)

	wb := &workbook{}
	wb.add("总览", nil, overview, []float64{40, 14, 14, 14, 14, 14, 14, 14})
	wb.sheets[0].hdrRows = hdr
	headers := []string{"税号", "账单时间", "我方请求时间", "时间差(秒)", "请求维度",
		"惠众结果", "惠众返回码", "惠众返回信息", "惠众订单号", "本次上游调用链", "最终成功上游", "下游查得维度",
		"客户", "appKey", "request id", "该税号该维度请求次数", "历史全部请求的上游结果", "备注"}
	widths := []float64{22, 18, 20, 10, 12, 12, 10, 20, 26, 40, 18, 12, 28, 16, 40, 12, 46, 40}
	for _, dim := range billDims {
		wb.add(dim+"明细", headers, details[dim], widths)
	}
	wb.add("我方本月惠众调用",
		[]string{"调用时间", "税号", "惠众 type", "结果", "惠众返回码", "惠众返回信息", "惠众订单号",
			"客户", "appKey", "request id", "已匹配账单"},
		monthRows, []float64{20, 22, 14, 8, 10, 24, 26, 28, 16, 40, 10})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	if err := wb.save(out); err != nil {
		fail("写 Excel 失败: %v", err)
	}
	for _, r := range overview {
		if len(r) > 0 {
			fmt.Println(r...)
		}
	}
	fmt.Println("已输出:", out)
}

// pickRequest 优先取调用过惠众且时间差在阈值内的请求，否则取时间最近的请求。
func pickRequest(cands []*request, b billRow, matchSec float64) (*request, float64) {
	var best, bestHz *request
	bestDiff, hzDiff := math.MaxFloat64, math.MaxFloat64
	for _, r := range cands {
		d := math.Abs(r.at.Sub(b.at).Seconds())
		if d < bestDiff {
			best, bestDiff = r, d
		}
		for _, c := range r.dimCalls(b.dim) {
			if c.provider == "ctax" && d <= matchSec && d < hzDiff {
				bestHz, hzDiff = r, d
			}
		}
	}
	if bestHz != nil {
		return bestHz, hzDiff
	}
	return best, bestDiff
}

// historySummary 汇总该税号该维度所有请求里各上游的结果，如「惠众C:查得×2，证通税务:查无×1」。
func historySummary(cands []*request, dim string) string {
	if len(cands) == 0 {
		return "无"
	}
	count := map[string]int{}
	var order []string
	for _, r := range cands {
		for _, c := range r.dimCalls(dim) {
			k := providerName(c) + ":" + statusName(c.status)
			if count[k] == 0 {
				order = append(order, k)
			}
			count[k]++
		}
	}
	if len(order) == 0 {
		return "有请求但未调用任何上游"
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s×%d", k, count[k]))
	}
	return strings.Join(parts, "，")
}

func skipReason(r *request, dim, provider string) string {
	for _, c := range r.calls {
		if c.provider == provider && c.covers(dim) && c.status == "skipped" {
			return c.skipReason
		}
	}
	return ""
}

func loadRequests(ctx context.Context, conn *pgx.Conn, where string, args ...any) []*request {
	rows, err := conn.Query(ctx, `
SELECT a.request_id, COALESCE(a.credit_code,''), a.created_at, COALESCE(a.req_scope,''), COALESCE(a.data_scope,''),
       a.app_key, COALESCE(l.name,'')
FROM audit_log a
LEFT JOIN license l ON l.app_key = a.app_key
WHERE `+where+`
ORDER BY a.created_at`, args...)
	if err != nil {
		fail("查询请求失败: %v", err)
	}
	defer rows.Close()
	var out []*request
	for rows.Next() {
		r := &request{}
		if err := rows.Scan(&r.id, &r.code, &r.at, &r.reqScope, &r.dataScope, &r.appKey, &r.customer); err != nil {
			fail("读取请求失败: %v", err)
		}
		r.at = r.at.In(cst)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		fail("读取请求失败: %v", err)
	}
	return out
}

// loadUpstreamCalls 把逐源调用挂到请求上；同一个 request_id 可能在两个列表里各有一个对象，都要挂。
func loadUpstreamCalls(ctx context.Context, conn *pgx.Conn, reqs []*request) {
	byID := map[string][]*request{}
	ids := make([]string, 0, len(reqs))
	for _, r := range reqs {
		if _, ok := byID[r.id]; !ok {
			ids = append(ids, r.id)
		}
		byID[r.id] = append(byID[r.id], r)
	}
	if len(ids) == 0 {
		return
	}
	rows, err := conn.Query(ctx, `
SELECT request_id, seq, provider, source_label, source_alias, dim_invoice, dim_tax, status,
       upstream_code, upstream_msg, upstream_uid, skip_reason, created_at
FROM upstream_call
WHERE request_id = ANY($1)
ORDER BY request_id, (seq = 0), seq, id`, ids)
	if err != nil {
		fail("查询上游调用失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c upCall
		if err := rows.Scan(&id, &c.seq, &c.provider, &c.label, &c.alias, &c.dimInv, &c.dimTax, &c.status,
			&c.upCode, &c.upMsg, &c.upUID, &c.skipReason, &c.at); err != nil {
			fail("读取上游调用失败: %v", err)
		}
		c.at = c.at.In(cst)
		for _, r := range byID[id] {
			r.calls = append(r.calls, c)
		}
	}
	if err := rows.Err(); err != nil {
		fail("读取上游调用失败: %v", err)
	}
}

func providerName(c upCall) string {
	switch c.provider {
	case "entcredit":
		if strings.HasPrefix(c.label, "invoice") {
			return "证通发票"
		}
		return "证通税务"
	case "ctax":
		return "惠众C"
	case "sy":
		return "税友"
	case "salesdata":
		return "销项"
	}
	return c.provider + "/" + c.label
}

// hzType 把我方请求维度换成惠众接口的 type 入参。
func hzType(scope string) string {
	switch scope {
	case "tax":
		return "1（税务）"
	case "invoice":
		return "2（发票）"
	}
	return "3（税务+发票）"
}

func scopeName(s string) string {
	switch s {
	case "invoice":
		return "发票"
	case "tax":
		return "税务"
	case "invoice+tax":
		return "发票+税务"
	case "", "none":
		return "无"
	}
	return s
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func parseBill() []billRow {
	var out []billRow
	for _, line := range strings.Split(strings.TrimSpace(billData), "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) != 3 {
			fail("内嵌账单行格式错误: %q", line)
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", f[2], cst)
		if err != nil {
			fail("内嵌账单时间错误: %q", line)
		}
		out = append(out, billRow{dim: f[0], code: f[1], at: t})
	}
	return out
}

func statusName(s string) string {
	switch s {
	case "ok":
		return "查得"
	case "empty":
		return "查无"
	case "error":
		return "失败"
	}
	return s
}

// 冠辰科技-月度对账单(3).xlsx「9月明细」：账单类型,税号,调用时间（北京时间，账单只精确到分钟）
const billData = `
票税,913101165647980623,2026-09-13 15:27:00
票税,91320583MA1PCENB13,2026-09-13 15:27:00
票税,914413035645245121,2026-09-13 15:27:00
票税,91441303MA52HE1729,2026-09-13 15:27:00
票税,91310115MA1K41YN6W,2026-09-13 15:27:00
票税,91320583MA1YLAK333,2026-09-13 15:27:00
票税,91440300MA5F0M1Y40,2026-09-16 09:36:00
票税,91130104MA0F82EJ4A,2026-09-16 10:58:00
税,91320115MA1W153W81,2026-09-22 10:47:00
票,91130705MAE3BFRT5E,2026-09-24 14:15:58
票,91440605MACW6XP77B,2026-09-24 15:29:38
票,91110105MA00319A5A,2026-09-24 18:56:02
税,91430500MA4PYRWXXF,2026-09-24 19:21:40
税,91120222MABWJ7QUX4,2026-09-24 19:22:05
税,91440507MA56JGL7X1,2026-09-24 19:22:43
税,91500107MAK709GKXK,2026-09-24 19:23:09
税,91370306MA3U4FXB2P,2026-09-24 19:23:33
税,91440106MAE1R82M53,2026-09-24 19:24:21
税,91440300MA5DA0AN4B,2026-09-24 19:25:19
税,91410100MAK2RHNQ8G,2026-09-24 19:25:25
税,91330185MA2GY8HA29,2026-09-24 19:25:27
税,91441521MA56GNPR1E,2026-09-24 19:26:28
税,91310109MACUKHFP14,2026-09-24 19:26:36
税,91320114MA27EGAX0N,2026-09-24 19:26:52
税,91330681MAK3JY0C0E,2026-09-24 19:27:11
税,91610136MAB0QMP08E,2026-09-24 19:27:43
税,91440606MAEQHABX67,2026-09-24 19:28:10
税,91330782MA2K0Y8R3T,2026-09-24 19:28:18
税,91310116MA1JBBAXX4,2026-09-24 19:28:29
税,91310116MAK15KG086,2026-09-24 19:28:53
税,91652801MA7927LNXT,2026-09-24 19:29:10
税,91320582MA2571GQ00,2026-09-24 19:29:54
税,91350802MA34RU7BX4,2026-09-24 19:30:34
税,92440605MAC4PNPX0C,2026-09-24 19:30:37
税,91130628MAERRQR743,2026-09-24 19:31:21
税,91330225MA2KNX8L7R,2026-09-24 19:31:24
税,91370105MAEU6PMP5L,2026-09-24 19:31:41
税,91350581MAE17WCU0D,2026-09-24 19:32:01
税,91441402MADTT3L07R,2026-09-24 19:32:02
税,91360202MA38LFGX7P,2026-09-24 19:32:38
票,91441900MADNNBLHXW,2026-09-24 20:09:51
票,91320506552466335P,2026-09-24 20:31:48
票,91110105MAE1B5H05H,2026-09-24 23:00:56
票,92500117MACDUGJY7A,2026-09-25 01:01:16
票,91440300MA5FL5E4XK,2026-09-25 01:36:02
票,91310230MABTDN0Y51,2026-09-25 06:29:25
票,91350603MA33QKE78N,2026-09-25 07:59:48
票,91120118MA06PU7N3A,2026-09-25 08:22:00
票,91330110MA2GYWN47T,2026-09-25 08:34:42
票,91330304MAE0B3FT3B,2026-09-25 09:16:06
票,92500108MAEX92M829,2026-09-25 10:16:59
票,91310000MACU33HQ18,2026-09-25 11:35:10
票,92350583MA33K5681C,2026-09-25 12:07:39
票,91130925MA7N8KEE2Y,2026-09-25 12:30:24
票,91341002MA8QGRP99C,2026-09-25 12:49:08
票,92500154MAAC91L1XP,2026-09-25 13:14:02
票,91610501MA6Y8XXJ8D,2026-09-25 13:37:29
票,92330401MA2LBATU7R,2026-09-25 13:53:52
票,92330411MA29G47D81,2026-09-25 15:03:34
票,91620200MA72H9L02E,2026-09-25 15:05:49
票,91130606MA0EYYQC1C,2026-09-25 15:17:41
票,91150691MA0QPJH741,2026-09-25 15:59:10
票,91440300MA5GL0UC4N,2026-09-25 16:06:51
票,91441900MA57AWF34A,2026-09-25 16:23:54
票,91330481MAC920JQ19,2026-09-25 18:05:30
税,91500106MAEUB3XH77,2026-09-25 19:22:10
税,91370128MAC65B3Q3J,2026-09-25 19:22:29
税,91130828MA088XGH92,2026-09-25 19:22:37
税,91310116MAK50K8B38,2026-09-25 19:22:46
税,91350583MA8UX34W4X,2026-09-25 19:23:17
税,91410105MAD0L9BN8D,2026-09-25 19:23:23
税,91445381MAK1WT7Q5N,2026-09-25 19:23:25
税,91411381MAK26LKB23,2026-09-25 19:23:28
税,91440604MADQRWT46F,2026-09-25 19:23:37
税,91440101MA9UK0AB3K,2026-09-25 19:23:51
税,91440300MA5FYKYU12,2026-09-25 19:24:01
税,91440300MAEL6XPW5T,2026-09-25 19:24:08
税,91320509MAEU0H794K,2026-09-25 19:24:08
税,91320191MA1TAXP09Q,2026-09-25 19:24:40
税,91441900MA55BEB921,2026-09-25 19:25:14
税,91130302MAEKQ11M1D,2026-09-25 19:25:19
税,91330206MA2GRKC38Q,2026-09-25 19:25:22
税,91440606MACMKATT7P,2026-09-25 19:25:44
税,91330726MA7L512F45,2026-09-25 19:25:54
税,91310120MA1JK9KA9X,2026-09-25 19:25:56
税,92440705MA52UFJF65,2026-09-25 19:26:10
税,91310112MADDW8KC4R,2026-09-25 19:26:13
税,91440101MA5D66NM3U,2026-09-25 19:26:58
税,91440111MAECYEXL9D,2026-09-25 19:27:20
税,91130302MABX57H95P,2026-09-25 19:27:34
税,91141002MAD59B0Q78,2026-09-25 19:27:55
税,91440300MA5EUY97XJ,2026-09-25 19:28:15
税,91440300760461168Y,2026-09-25 19:28:20
税,91440111MACX9C8J7C,2026-09-25 19:28:22
税,91330483MA2CW8MK1Y,2026-09-25 19:29:25
税,91610131MAK49FQH14,2026-09-25 19:29:38
税,91441900MAD9EFNM26,2026-09-25 19:29:55
税,91440300MA5G5CA39Y,2026-09-25 19:30:14
税,91440300MA5GXL8GXJ,2026-09-25 19:30:18
税,91330106MAKDAT2J77,2026-09-25 19:30:46
税,91330782MA8GUWEG70,2026-09-25 19:31:07
税,91440101MA5D0C4P0B,2026-09-25 19:31:15
票,91360521MAEFNK2D54,2026-09-25 19:52:52
票,91321181MA1N0MLU9Y,2026-09-25 23:36:03
票,92500120MACKQ0NQ8W,2026-09-26 02:10:34
票,91330702MAE3YUWG77,2026-09-26 03:52:23
票,91320191MAEMTUP604,2026-09-26 10:36:27
票,91510181MAC8TW2121,2026-09-26 10:40:45
票,91130502320127369B,2026-09-26 10:45:16
票,92321322MA25WWTJ70,2026-09-26 11:24:54
票,91350582MA32TX9J5R,2026-09-26 11:44:15
票,92350524MA30UMXKXC,2026-09-26 13:16:31
票,91440705MAEWL1C13X,2026-09-26 14:05:41
票,916209023322937199,2026-09-26 15:59:55
票,91420116MA49NUN22P,2026-09-26 18:05:35
税,91430104MAC578Q41K,2026-09-26 19:21:40
税,91440113MAEULNYL5N,2026-09-26 19:22:39
税,91350503MACBNCRE32,2026-09-26 19:22:49
税,91320213MA1YG3P982,2026-09-26 19:23:22
税,91511521MABNRAF08L,2026-09-26 19:23:49
税,91411324MA9G2KJ8XC,2026-09-26 19:24:00
税,91440300MA5FTUR367,2026-09-26 19:24:05
税,91110105MACYECT2X9,2026-09-26 19:24:21
税,91210106MADP04776E,2026-09-26 19:24:40
税,91331100MAD7TLE28F,2026-09-26 19:24:55
税,91340103MAE6E16T2Q,2026-09-26 19:25:13
税,91320594MA1MRNAL4Q,2026-09-26 19:25:28
税,91140502MAEA0YW08M,2026-09-26 19:25:55
税,91330105MAG19NN136,2026-09-26 19:26:30
税,91340881MAEXKN813A,2026-09-26 19:27:03
税,91430111MA7FFDN82G,2026-09-26 19:27:15
税,91510104MA6C6JP456,2026-09-26 19:27:25
税,92510107MACMREQ34J,2026-09-26 19:27:37
税,91330205MA7LTM939U,2026-09-26 19:27:37
税,91450200MACT06K76Y,2026-09-26 19:27:57
税,91440902MACKBXL001,2026-09-26 19:28:38
税,91371302MADDKM677C,2026-09-26 19:28:48
税,91330203MA2J45FF1J,2026-09-26 19:28:49
税,91140109MADXMPWR3B,2026-09-26 19:29:04
税,92511681MA67W6RE1R,2026-09-26 19:29:11
税,91441900MAE7YH692A,2026-09-26 19:29:23
票,92533401MACPTUGJ95,2026-09-27 08:14:00
票,91130523MA07K6XF1M,2026-09-27 08:17:56
票,91330702MA7C69MB8D,2026-09-27 08:45:00
票,91340826MADM4KDQ22,2026-09-27 15:19:49
票,91130533MA7FDJKT0W,2026-09-27 16:16:49
票,91445102MAE2TNQG12,2026-09-27 17:15:31
票,92331004MAC6767H25,2026-09-27 18:28:30
税,91650102MAK74NN008,2026-09-27 19:21:15
税,91330281MA2CKUJJ92,2026-09-27 19:21:27
税,91330114MA8GJ4KLX3,2026-09-27 19:21:31
税,91131126MADXP83R9Y,2026-09-27 19:22:11
税,91410223MAEXDC4791,2026-09-27 19:22:16
税,91331002MAKEJM3E36,2026-09-27 19:22:53
税,91141081MA0KBBAF1M,2026-09-27 19:23:52
税,91310120MA7FKY5D34,2026-09-27 19:24:21
税,91411302MAG03HNX5X,2026-09-27 19:24:36
税,91654202MAELTANK7E,2026-09-27 19:25:13
税,92652701MAK47WPT9M,2026-09-27 19:25:17
税,91440106MAEPXG578G,2026-09-27 19:25:21
税,91140109MA0M4XKG7X,2026-09-27 19:26:04
税,91130903MAEWATRX7D,2026-09-27 19:26:13
税,91441900MA7L111542,2026-09-27 19:26:15
税,91440106MAE6959B85,2026-09-27 19:26:27
税,91440606MAK1TC3Y26,2026-09-27 19:26:30
税,92510100MADG71CF5Q,2026-09-27 19:26:40
税,91440605MAE1JNC304,2026-09-27 19:26:52
票,91130108598274347F,2026-09-27 19:57:19
票,92440784L40206882D,2026-09-27 23:53:18
票,92510181L700093965,2026-09-28 00:14:46
票,91330105341801551J,2026-09-28 10:33:23
票,91510504MA6224RL5Q,2026-09-28 10:45:31
票,91441900MA53EX881A,2026-09-28 11:17:55
票,91130503MAEBMDH22B,2026-09-28 11:36:01
票,91420900MAEGXQLW3X,2026-09-28 12:36:52
票,92331081MA2DTFXJ7B,2026-09-28 13:12:09
票,91341000MA2N9TM345,2026-09-28 13:34:14
票,92330206MACM96BD0J,2026-09-28 13:38:01
票,91440607MAE3QWX12L,2026-09-28 14:50:39
票,91370705MA3WDLG012,2026-09-28 15:33:15
票,91320113MAER7MYX1K,2026-09-29 13:13:25
票,91440300MA5ET3MF83,2026-09-29 14:16:23
票,92330402MA7ETL2K4A,2026-09-29 14:42:12
票,912301030563189235,2026-09-29 15:05:35
票,92140215MA7XNMEY2R,2026-09-29 18:19:21
税,91445103MAD8G3TQ0K,2026-09-29 19:21:18
税,91510106MA628KDY9E,2026-09-29 19:21:19
票,91330602MA7J0R3A10,2026-09-29 19:21:52
税,91110108MACU02GK7W,2026-09-29 19:22:07
税,91510100MA6CTW9H0X,2026-09-29 19:22:18
税,91330782MAK60WH04G,2026-09-29 19:22:41
税,91330783MAK1DJWA21,2026-09-29 19:22:57
税,91310230MA1JY68D5A,2026-09-29 19:23:27
税,91360402MAK7GYE5X0,2026-09-29 19:23:39
税,91440118MAKD1K4X07,2026-09-29 19:23:57
税,91442000MAEWEX6H5T,2026-09-29 19:24:01
税,91440981MAEU8AG296,2026-09-29 19:24:14
税,91510100MAE510H51M,2026-09-29 19:24:16
税,91440605MA4WKJ7J46,2026-09-29 19:24:52
税,91370105307102300H,2026-09-29 19:25:00
税,91441900MAK1NULB5Y,2026-09-29 19:25:12
税,91510100MAET4NJT1D,2026-09-29 19:26:21
税,91440101MA59GJHQ4F,2026-09-29 19:26:28
税,91430104MAECMTYB9J,2026-09-29 19:26:37
税,91440105MADNHUHUXM,2026-09-29 19:27:32
税,91650105686462233N,2026-09-29 19:27:34
税,91320213MA1Y3NL10L,2026-09-29 19:28:10
税,91330900MABPM4WM4M,2026-09-29 19:28:13
税,91440300MA5FJPWL72,2026-09-29 19:28:30
税,91440604MACF02040E,2026-09-29 19:28:38
税,91440300MAD766B81P,2026-09-29 19:28:43
税,91440300MA5G8L3HXL,2026-09-29 19:28:55
税,91440300MA5F1L3H02,2026-09-29 19:28:56
税,91330523MAK4NC910G,2026-09-29 19:29:03
税,91500109MA61AULK23,2026-09-29 19:29:05
税,91440101MA9XXQUH4D,2026-09-29 19:29:28
税,913101070781286960,2026-09-29 19:29:52
税,91441900MAK4K0LK3E,2026-09-29 19:29:55
税,91440604MADX5XXP21,2026-09-29 19:30:22
税,91441900MAC3G4G368,2026-09-29 19:30:37
税,91230606MAEP1N3101,2026-09-29 19:31:00
税,91653101MA77505212,2026-09-29 19:31:05
税,91310106MADCMEHD4C,2026-09-29 19:31:25
税,92140105MA0M214N4Y,2026-09-29 19:31:49
税,91442000MAG1WT8K5M,2026-09-29 19:32:17
税,91510108MA69XE6G33,2026-09-29 19:32:26
税,91130229MA7LYAMD82,2026-09-29 19:32:34
税,91440300312024474M,2026-09-29 19:32:41
税,91440106MACQ3X7X3X,2026-09-29 19:32:49
税,91441900MAK3447E85,2026-09-29 19:33:05
税,91440114MADDDJK886,2026-09-29 19:33:11
税,91330106MAD4DWJB3P,2026-09-29 19:33:28
税,91330783MAKD7JCCXC,2026-09-29 19:33:37
税,91310115MAEMNUTE94,2026-09-29 19:33:40
税,91440605084531211A,2026-09-29 19:34:00
税,91440300MA5H6G2F3J,2026-09-29 19:34:00
税,91442000MA4UQNRB84,2026-09-29 19:34:40
税,91610103MA6UTE6N8E,2026-09-29 19:34:42
票,91310120MA7KYWMM0Y,2026-09-29 19:45:40
票,91440300782787492K,2026-09-29 21:52:37
票,91310114084097398Y,2026-09-29 22:28:13
票,92130926MA7LFTQW7L,2026-09-29 23:23:12
票,91440300MA5F9K8UX9,2026-09-29 23:55:43
票,91110108668410931L,2026-09-30 07:40:31
票,92441900MA5258N387,2026-09-30 08:01:39
票,91130403MA0G5XHD3J,2026-09-30 08:40:41
票,92320413MADY3WTXXA,2026-09-30 08:47:08
票,91441283MADNAH2H4T,2026-09-30 10:41:44
票,92371312MA3FFN6E33,2026-09-30 11:15:38
票,92610103MACW3NAM0P,2026-09-30 13:32:19
票,91532800316274788X,2026-09-30 14:56:24
票,91441900MAD3PDLL0B,2026-09-30 15:23:22
票,91330784MA8G4EW34E,2026-09-30 15:47:33
票,91411426MA9GTFKM41,2026-09-30 16:01:57
票,91441422MADMPWGU1U,2026-09-30 16:05:05
票,91440101MA5APB749P,2026-09-30 16:34:29
票,91440101MA5D14BE9K,2026-09-30 16:37:35
票,91350581MAK12UFU88,2026-09-30 18:24:59
票,91310120MADDUDNG9D,2026-09-30 18:53:10
税,91320902559302050H,2026-09-30 19:21:42
税,91330110MAE8AFFF3Y,2026-09-30 19:21:50
税,91610423MA6XW9NK72,2026-09-30 19:21:56
税,9161010466318332X5,2026-09-30 19:22:01
税,91330783MA2JX0N715,2026-09-30 19:23:17
税,91440606MA55UMTC72,2026-09-30 19:23:30
税,91340111MADQWN5J9F,2026-09-30 19:23:57
税,91441303MAEPL47Y33,2026-09-30 19:24:21
税,91440101MA5CQCPW2R,2026-09-30 19:25:31
税,91130108MAK0E2N793,2026-09-30 19:25:38
税,91370900MAK2Y7XP4L,2026-09-30 19:25:50
税,91442000MA51MJJE42,2026-09-30 19:26:26
税,91340202MA2WU5MN5R,2026-09-30 19:26:50
税,91440101MA9XPLH80K,2026-09-30 19:27:07
税,91420100MA4KL5DF6K,2026-09-30 19:27:08
税,91440113MAK3R7Q75J,2026-09-30 19:27:19
税,91450102MAK422AY5D,2026-09-30 19:27:21
税,91500106MAEX9AJ26E,2026-09-30 19:27:31
税,91350602MA8TFG1E48,2026-09-30 19:28:06
税,91410184MAK06UWCXW,2026-09-30 19:28:20
税,91330782MA28DW0G7A,2026-09-30 19:28:45
税,91441208MAEWC2950Y,2026-09-30 19:28:55
税,91110108MAD97H6P6F,2026-09-30 19:28:55
税,91445103MACTANHE5Q,2026-09-30 19:29:06
税,91320581MAE1RNTY66,2026-09-30 19:29:57
税,91360805MABTQA7B77,2026-09-30 19:30:19
税,91440605MAK3DKHY8W,2026-09-30 19:30:21
税,91110106MAC7PGK10C,2026-09-30 19:30:29
税,91653022MAKA1XAD8C,2026-09-30 19:30:49
税,91513401MAEUAQLN7R,2026-09-30 19:31:01
税,914403001924587954,2026-09-30 19:31:52
税,91440608MA55WUP227,2026-09-30 19:32:55
税,91321395MAK48XDH2K,2026-09-30 19:33:03
税,91420112MAG14HG90Y,2026-09-30 19:33:18
票,92350402MA8UL74F0Q,2026-09-30 21:00:00
票,91310120MA1HUMB59D,2026-09-30 22:13:14
`

// ---------------- 最小 xlsx 写出（仅标准库） ----------------

type sheet struct {
	name    string
	headers []string
	rows    [][]any
	widths  []float64
	hdrRows map[int]bool // rows 中按表头样式输出的行号（0 起）
}

type workbook struct{ sheets []sheet }

func (w *workbook) add(name string, headers []string, rows [][]any, widths []float64) {
	w.sheets = append(w.sheets, sheet{name: name, headers: headers, rows: rows, widths: widths})
}

func (w *workbook) save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	z := zip.NewWriter(f)
	put := func(name, body string) error {
		fw, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(fw, body)
		return err
	}

	var ct, wbx, rels strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
`)
	wbx.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i, s := range w.sheets {
		n := i + 1
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`+"\n", n)
		fmt.Fprintf(&wbx, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEsc(s.name), n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n)
		if err := put(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), sheetXML(s)); err != nil {
			return err
		}
	}
	ct.WriteString(`</Types>`)
	wbx.WriteString(`</sheets></workbook>`)
	fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(w.sheets)+1)

	for _, p := range [][2]string{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", wbx.String()},
		{"xl/_rels/workbook.xml.rels", rels.String()},
		{"xl/styles.xml", stylesXML},
	} {
		if err := put(p[0], p[1]); err != nil {
			return err
		}
	}
	return z.Close()
}

// 样式 0=普通 1=表头(白字深蓝底) 2=自动换行
const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Calibri"/></font></fonts>
<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF1F4E78"/><bgColor indexed="64"/></patternFill></fill></fills>
<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment wrapText="1" vertical="top"/></xf></cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`

func sheetXML(s sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	if len(s.headers) > 0 {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	}
	if len(s.widths) > 0 {
		b.WriteString("<cols>")
		for i, wd := range s.widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, i+1, i+1, wd)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	rowNum := 1
	if len(s.headers) > 0 {
		header := make([]any, len(s.headers))
		for i, h := range s.headers {
			header[i] = h
		}
		writeRow(&b, rowNum, header, 1)
		rowNum++
	}
	for i, r := range s.rows {
		style := 2
		if s.hdrRows[i] {
			style = 1
		}
		writeRow(&b, rowNum, r, style)
		rowNum++
	}
	if len(s.rows) == 0 {
		writeRow(&b, rowNum, []any{"(无数据)"}, 2)
	}
	b.WriteString("</sheetData></worksheet>")
	return b.String()
}

func writeRow(b *strings.Builder, rowNum int, vals []any, style int) {
	fmt.Fprintf(b, `<row r="%d">`, rowNum)
	for i, v := range vals {
		ref := colName(i) + strconv.Itoa(rowNum)
		switch t := v.(type) {
		case nil:
			continue
		case int, int32, int64, float32, float64:
			fmt.Fprintf(b, `<c r="%s" s="%d"><v>%v</v></c>`, ref, style, t)
		default:
			fmt.Fprintf(b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlEsc(fmt.Sprint(t)))
		}
	}
	b.WriteString("</row>")
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\t' || r == '\n' || r == '\r' || r >= 0x20:
			b.WriteRune(r)
		}
	}
	return b.String()
}
