//go:build ignore

// export_found_records: 导出某客户某月全部「查得」请求的对账明细（只读）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/export_found_records.go
//
// 可选环境变量：
//
//	CUSTOMER=北京联信征信咨询有限责任公司   license.name，精确匹配
//	MONTH=2026-09                           北京时间自然月
//	OUT=test_res/xxx.xlsx                   缺省 test_res/<客户>-<月份>-查得明细.xlsx
//
// 口径：audit_log.found_data = true（下游 body.code=001）；查得维度取 data_scope，
// 不区分由哪个上游源查得。request id 即返回给下游的 head.logId（audit_log.request_id）。
// 「计费」列取主体年度计费后的 charged_scope：免费期内的查得记为查得但不计费。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

var cst = time.FixedZone("CST", 8*3600)

type exportConfig struct {
	Billing struct {
		Rates struct {
			BothFen    int64 `yaml:"bothFen"`
			InvoiceFen int64 `yaml:"invoiceFen"`
			TaxFen     int64 `yaml:"taxFen"`
		} `yaml:"rates"`
	} `yaml:"billing"`
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

type license struct {
	appKey                  string
	bothFen, invFen, taxFen int64
}

type record struct {
	at                          time.Time
	requestID, creditCode, mask string
	appKey                      string
	dataScope, chargedScope     string
	chargeState                 string
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	customer := env("CUSTOMER", "北京联信征信咨询有限责任公司")
	month := env("MONTH", "2026-09")
	from, err := time.ParseInLocation("2006-01", month, cst)
	if err != nil {
		fail("MONTH 格式应为 YYYY-MM: %v", err)
	}
	to := from.AddDate(0, 1, 0)
	out := env("OUT", filepath.Join("test_res", fmt.Sprintf("%s-%s-查得明细.xlsx", customer, month)))

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
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '120s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}

	lics := loadLicenses(ctx, conn, customer)
	if len(lics) == 0 {
		fail("license 里没有名称为 %q 的客户", customer)
	}
	appKeys := make([]string, 0, len(lics))
	for _, l := range lics {
		appKeys = append(appKeys, l.appKey)
	}
	recs := loadRecords(ctx, conn, appKeys, from, to)

	// 单价：客户合同价优先，缺档走全局缺省（与 FeeRates 兜底口径一致）。
	pick := func(lic, def int64) int64 {
		if lic > 0 {
			return lic
		}
		return def
	}
	l0 := lics[0]
	price := map[string]int64{
		"invoice":     pick(l0.invFen, cfg.Billing.Rates.InvoiceFen),
		"tax":         pick(l0.taxFen, cfg.Billing.Rates.TaxFen),
		"invoice+tax": pick(l0.bothFen, cfg.Billing.Rates.BothFen),
	}

	found := map[string]int64{}
	charged := map[string]int64{}
	detail := make([][]any, 0, len(recs))
	for _, r := range recs {
		inv, tax := dims(r.dataScope)
		cInv, cTax := dims(r.chargedScope)
		code := r.creditCode
		var notes []string
		if code == "" {
			code = r.mask
			notes = append(notes, "税号为脱敏值（主体计费上线前的记录）")
		}
		switch {
		case r.chargeState == "FREE":
			notes = append(notes, "免费期内，本次未计费")
		case r.chargeState == "DEFERRED":
			notes = append(notes, "计费判定待补记")
		case r.chargeState == "CHARGED" && r.chargedScope != r.dataScope:
			notes = append(notes, fmt.Sprintf("仅计费%s，其余类目在免费期内", scopeName(r.chargedScope)))
		}
		found[normScope(r.dataScope)]++
		if r.chargeState == "CHARGED" && r.chargedScope != "" && r.chargedScope != "none" {
			charged[normScope(r.chargedScope)]++
		}
		detail = append(detail, []any{
			r.at.In(cst).Format("2006-01-02 15:04:05"), r.requestID, code, r.appKey,
			b2i(tax), b2i(inv), scopeName(r.dataScope), b2i(cTax), b2i(cInv), strings.Join(notes, "；"),
		})
	}

	wb := &workbook{}
	monthLabel := from.Format("2006年1月")
	var totFound, totCharged, totAmount int64
	summary := [][]any{
		{"客户名称", customer},
		{"账期", monthLabel},
		{"产品名称", "查得量", "计费量", "单价（元）", "计费小计（元）", "备注"},
	}
	scopes := []string{"invoice", "tax", "invoice+tax"}
	if found["unknown"] > 0 {
		scopes = append(scopes, "unknown")
	}
	for _, s := range scopes {
		amount := charged[s] * price[s]
		totFound += found[s]
		totCharged += charged[s]
		totAmount += amount
		note := ""
		switch {
		case s == "unknown":
			note = "查得维度未记录（字段上线前的老记录）"
		case price[s] == 0:
			note = "未配置单价"
		}
		summary = append(summary, []any{scopeName(s), found[s], charged[s], fen(price[s]), fen(amount), note})
	}
	summary = append(summary, []any{"小计", totFound, totCharged, "", fen(totAmount), ""})
	wb.add("对账单", nil, summary, []float64{30, 12, 12, 14, 16, 30})
	wb.sheets[len(wb.sheets)-1].hdrRows = map[int]bool{0: true, 1: true, 2: true, len(summary) - 1: true}

	wb.add(from.Format("1月")+"明细",
		[]string{"调用时间", "request id", "税号", "appKey", "查得税", "查得票", "查得类型", "计费税", "计费票", "备注"},
		detail, []float64{20, 40, 22, 16, 8, 8, 14, 8, 8, 36})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	if err := wb.save(out); err != nil {
		fail("写 Excel 失败: %v", err)
	}
	fmt.Printf("%s %s：查得 %d 条（票 %d / 税 %d / 票+税 %d），计费 %d 条\n",
		customer, monthLabel, totFound, found["invoice"], found["tax"], found["invoice+tax"], totCharged)
	fmt.Println("已输出:", out)
}

func loadLicenses(ctx context.Context, conn *pgx.Conn, name string) []license {
	rows, err := conn.Query(ctx, `SELECT app_key, rate_both_fen, rate_invoice_fen, rate_tax_fen FROM license WHERE name=$1 ORDER BY created_at`, name)
	if err != nil {
		fail("查询 license 失败: %v", err)
	}
	defer rows.Close()
	var out []license
	for rows.Next() {
		var l license
		if err := rows.Scan(&l.appKey, &l.bothFen, &l.invFen, &l.taxFen); err != nil {
			fail("读取 license 失败: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		fail("读取 license 失败: %v", err)
	}
	return out
}

func loadRecords(ctx context.Context, conn *pgx.Conn, appKeys []string, from, to time.Time) []record {
	rows, err := conn.Query(ctx, `
SELECT created_at, request_id, COALESCE(credit_code,''), COALESCE(id_card_mask,''), app_key,
       COALESCE(data_scope,''), COALESCE(charged_scope,''), COALESCE(charge_state,'')
FROM audit_log
WHERE app_key = ANY($1) AND found_data AND created_at >= $2 AND created_at < $3
ORDER BY created_at`, appKeys, from, to)
	if err != nil {
		fail("查询审计失败: %v", err)
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.at, &r.requestID, &r.creditCode, &r.mask, &r.appKey,
			&r.dataScope, &r.chargedScope, &r.chargeState); err != nil {
			fail("读取审计失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		fail("读取审计失败: %v", err)
	}
	return out
}

func dims(scope string) (invoice, tax bool) {
	switch scope {
	case "invoice+tax":
		return true, true
	case "invoice":
		return true, false
	case "tax":
		return false, true
	}
	return false, false
}

// normScope 把维度串归一到三档；空值（字段上线前的老记录）单独成一档以免静默丢失。
func normScope(s string) string {
	switch s {
	case "invoice", "tax", "invoice+tax":
		return s
	}
	return "unknown"
}

func scopeName(s string) string {
	switch s {
	case "invoice":
		return "发票"
	case "tax":
		return "税务"
	case "invoice+tax":
		return "发票+税务"
	}
	return "未记录"
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func fen(v int64) float64 { return float64(v) / 100 }

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
