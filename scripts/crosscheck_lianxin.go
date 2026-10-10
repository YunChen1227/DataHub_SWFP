//go:build ignore

// crosscheck_lianxin: 把下游客户（北京联信）给的调用明细逐条拿到我方库里核对（只读）。
//
//	IN=test_res/lianxin_input.csv \
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/crosscheck_lianxin.go
//
// 可选环境变量：
//
//	CUSTOMER=北京联信征信咨询有限责任公司   license.name，精确匹配（可多条 license）
//	TOL_SEC=180                             税号相同时，调用时间允许的匹配误差（秒）
//	OUT=test_res/xxx.xlsx                   缺省 test_res/联信对账核对_<时间>.xlsx
//
// 输入 CSV 列：order_no,tax_code,result,month,call_time（call_time 形如
// 2026-10-07 10:13:30，北京时间）。匹配键为「税号 + 调用时间就近」，因为下游订单号
// (order_no) 是客户侧自编号，我方不接收也不落库，无法直接对。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
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

type ccConfig struct {
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

type theirRow struct {
	orderNo, taxCode, result, month string
	at                              time.Time
}

type ourRow struct {
	at         time.Time
	creditCode string
	requestID  string
	foundData  bool
	dataScope  string
	busiCode   int
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	in := env("IN", "test_res/lianxin_input.csv")
	customer := env("CUSTOMER", "北京联信征信咨询有限责任公司")
	tol, _ := strconv.Atoi(env("TOL_SEC", "180"))
	if tol <= 0 {
		tol = 180
	}
	out := env("OUT", filepath.Join("test_res", "联信对账核对_"+time.Now().In(cst).Format("20060102_1504")+".xlsx"))

	their := readCSV(in)
	if len(their) == 0 {
		fail("输入 CSV 没有数据行: %s", in)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg ccConfig
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
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '120s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}

	appKeys := loadAppKeys(ctx, conn, customer)
	if len(appKeys) == 0 {
		fail("license 里没有名称为 %q 的客户", customer)
	}

	// 一次性拉取客户在 [最早-1h, 最晚+1h] 的全部审计，内存里按 税号+就近时间 匹配。
	lo, hi := their[0].at, their[0].at
	for _, t := range their {
		if t.at.Before(lo) {
			lo = t.at
		}
		if t.at.After(hi) {
			hi = t.at
		}
	}
	byCode := loadOurRecords(ctx, conn, appKeys, lo.Add(-time.Hour), hi.Add(time.Hour))

	// 同区间内我方总体口径，供汇总对比（他们只给成功计费，我方给全量 + 查得）。
	var ourCalls, ourFound int64
	_ = conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE found_data)
		FROM audit_log WHERE app_key = ANY($1) AND created_at >= $2 AND created_at < $3`,
		appKeys, monthStart(their), monthEndExclusive(their)).Scan(&ourCalls, &ourFound)

	tolDur := time.Duration(tol) * time.Second
	detail := make([][]any, 0, len(their))
	var matched, matchedFound, notFound, noRecord int64
	monthHit := map[string][2]int64{} // month -> {命中, 其中我方也查得}
	for _, t := range their {
		best, diff := nearest(byCode[t.taxCode], t.at, tolDur)
		mh := monthHit[t.month]
		row := []any{t.orderNo, t.taxCode, t.at.In(cst).Format("2006-01-02 15:04:05"), t.month, t.result}
		if best == nil {
			noRecord++
			row = append(row, "无记录", "", "", "", "", "")
			detail = append(detail, row)
			monthHit[t.month] = mh
			continue
		}
		matched++
		mh[0]++
		foundTxt := "否"
		if best.foundData {
			foundTxt = "是"
			matchedFound++
			mh[1]++
		} else {
			notFound++
		}
		row = append(row,
			"已匹配", best.requestID, best.at.In(cst).Format("2006-01-02 15:04:05"),
			foundTxt, scopeName(best.dataScope), fmt.Sprintf("%ds", int(math.Round(diff.Seconds()))))
		detail = append(detail, row)
		monthHit[t.month] = mh
	}

	wb := &workbook{}
	summary := [][]any{
		{"客户名称", customer},
		{"核对口径", fmt.Sprintf("按「税号 + 调用时间就近（±%ds）」匹配；订单号为客户自编号，我方不落库", tol)},
		{"", ""},
		{"项目", "数值", "说明"},
		{"联信文件总行数", int64(len(their)), "全部为「查询成功/计费」"},
		{"  我方库中匹配到", matched, "同税号且时间吻合的请求存在"},
		{"    其中我方也判为查得", matchedFound, "found_data=true，与联信口径一致"},
		{"    其中我方判为未查得", notFound, "我方记为查无/失败（含惠众 SYS200 无业务段被判 999 的情况）"},
		{"  我方库中无记录", noRecord, "该时间点附近没有这个税号的请求"},
		{"", "", ""},
		{"同期我方该客户总调用数", ourCalls, "audit_log 全量（含查无/失败）"},
		{"同期我方该客户查得数", ourFound, "found_data=true"},
	}
	wb.add("核对汇总", nil, summary, []float64{30, 16, 60})
	wb.sheets[0].hdrRows = map[int]bool{3: true}

	months := make([]string, 0, len(monthHit))
	for m := range monthHit {
		months = append(months, m)
	}
	sort.Strings(months)
	monthRows := [][]any{}
	for _, m := range months {
		var cnt int64
		for _, t := range their {
			if t.month == m {
				cnt++
			}
		}
		mh := monthHit[m]
		monthRows = append(monthRows, []any{m, cnt, mh[0], mh[1], cnt - mh[0]})
	}
	wb.add("按月", []string{"月份", "联信成功数", "我方匹配到", "我方也查得", "我方无记录"},
		monthRows, []float64{12, 14, 12, 12, 12})

	wb.add("逐条核对",
		[]string{"联信订单号", "税号", "联信调用时间", "月份", "联信结果",
			"我方匹配", "我方request id", "我方调用时间", "我方查得", "我方查得类型", "时间差"},
		detail, []float64{34, 22, 20, 10, 12, 10, 40, 20, 10, 14, 10})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	if err := wb.save(out); err != nil {
		fail("写 Excel 失败: %v", err)
	}
	fmt.Printf("联信 %d 条：匹配 %d（其中我方查得 %d / 未查得 %d），无记录 %d\n",
		len(their), matched, matchedFound, notFound, noRecord)
	fmt.Printf("同期我方该客户：总调用 %d，查得 %d\n", ourCalls, ourFound)
	fmt.Println("已输出:", out)
}

func readCSV(path string) []theirRow {
	f, err := os.Open(path)
	if err != nil {
		fail("读取输入 CSV 失败: %v", err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	recs, err := r.ReadAll()
	if err != nil {
		fail("解析 CSV 失败: %v", err)
	}
	var out []theirRow
	for i, rec := range recs {
		if i == 0 || len(rec) < 5 {
			continue // 跳过表头/残行
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(rec[4]), cst)
		if err != nil {
			// 容错：也接受带 T/时区的格式
			if t2, e2 := time.Parse(time.RFC3339, strings.TrimSpace(rec[4])); e2 == nil {
				t = t2
			} else {
				fail("第 %d 行时间无法解析: %q", i+1, rec[4])
			}
		}
		out = append(out, theirRow{
			orderNo: strings.TrimSpace(rec[0]), taxCode: strings.TrimSpace(rec[1]),
			result: strings.TrimSpace(rec[2]), month: strings.TrimSpace(rec[3]), at: t,
		})
	}
	return out
}

func loadAppKeys(ctx context.Context, conn *pgx.Conn, name string) []string {
	rows, err := conn.Query(ctx, `SELECT app_key FROM license WHERE name=$1`, name)
	if err != nil {
		fail("查询 license 失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			fail("读取 license 失败: %v", err)
		}
		out = append(out, k)
	}
	return out
}

func loadOurRecords(ctx context.Context, conn *pgx.Conn, appKeys []string, from, to time.Time) map[string][]ourRow {
	rows, err := conn.Query(ctx, `
SELECT created_at, COALESCE(credit_code,''), request_id, found_data, COALESCE(data_scope,''), COALESCE(busi_code,0)
FROM audit_log
WHERE app_key = ANY($1) AND created_at >= $2 AND created_at < $3
ORDER BY created_at`, appKeys, from, to)
	if err != nil {
		fail("查询审计失败: %v", err)
	}
	defer rows.Close()
	out := map[string][]ourRow{}
	for rows.Next() {
		var r ourRow
		if err := rows.Scan(&r.at, &r.creditCode, &r.requestID, &r.foundData, &r.dataScope, &r.busiCode); err != nil {
			fail("读取审计失败: %v", err)
		}
		if r.creditCode != "" {
			out[r.creditCode] = append(out[r.creditCode], r)
		}
	}
	if err := rows.Err(); err != nil {
		fail("读取审计失败: %v", err)
	}
	return out
}

// nearest 在候选里找与 t 时间最近、且在 tol 之内的一条。
func nearest(cands []ourRow, t time.Time, tol time.Duration) (*ourRow, time.Duration) {
	var best *ourRow
	var bestDiff time.Duration
	for i := range cands {
		d := cands[i].at.Sub(t)
		if d < 0 {
			d = -d
		}
		if d <= tol && (best == nil || d < bestDiff) {
			best = &cands[i]
			bestDiff = d
		}
	}
	return best, bestDiff
}

func monthStart(their []theirRow) time.Time {
	min := their[0].at
	for _, t := range their {
		if t.at.Before(min) {
			min = t.at
		}
	}
	return time.Date(min.In(cst).Year(), min.In(cst).Month(), 1, 0, 0, 0, 0, cst)
}

func monthEndExclusive(their []theirRow) time.Time {
	max := their[0].at
	for _, t := range their {
		if t.at.After(max) {
			max = t.at
		}
	}
	m := max.In(cst)
	return time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, cst).AddDate(0, 1, 0)
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
		return "-"
	}
	return s
}

// ---------------- 最小 xlsx 写出（仅标准库） ----------------

type sheet struct {
	name    string
	headers []string
	rows    [][]any
	widths  []float64
	hdrRows map[int]bool
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
