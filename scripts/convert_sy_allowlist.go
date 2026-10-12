//go:build ignore

// convert_sy_allowlist: 把惠众月度对账单 xlsx 转成 sy 源白名单 data/sy_allowlist.csv。
//
//	go run ./scripts/convert_sy_allowlist.go <对账单.xlsx>
//
// 可调项：
//
//	DETAIL_SHEET=9月明细    # 明细 sheet 名（列：调用时间/税号/appId/计费税/计费票/备注）
//	SUMMARY_SHEET=对账单    # 汇总 sheet 名（列：日期/产品名称/调用计费量/单价/小计/备注）
//	OUT=data/sy_allowlist.csv
//
// 白名单是计费红线（sy 只能调已计过费的维度），故转换时做两道校验，任一不过即退出、
// 不写文件：
//  1. 明细逐行：税号是合法统一社会信用代码、计费税/计费票只能是 0/1 且至少一个为 1、
//     税号不重复、appId 只有一个。
//  2. 明细按 (税, 票) 组合计数，必须与汇总 sheet 各产品的「调用计费量」逐项相等。
package main

import (
	"archive/zip"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/parse"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf("FAIL: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("用法: go run ./scripts/convert_sy_allowlist.go <对账单.xlsx>")
	}
	detailName := env("DETAIL_SHEET", "9月明细")
	summaryName := env("SUMMARY_SHEET", "对账单")
	out := env("OUT", "data/sy_allowlist.csv")

	zr, err := zip.OpenReader(os.Args[1])
	if err != nil {
		fail("打开 xlsx: %v", err)
	}
	defer zr.Close()

	shared, err := readSharedStrings(&zr.Reader)
	if err != nil {
		fail("%v", err)
	}
	sheets, err := sheetPaths(&zr.Reader)
	if err != nil {
		fail("%v", err)
	}
	detail, err := readRows(&zr.Reader, sheets[detailName], shared)
	if err != nil {
		fail("读取 sheet %q: %v", detailName, err)
	}
	summary, err := readRows(&zr.Reader, sheets[summaryName], shared)
	if err != nil {
		fail("读取 sheet %q: %v", summaryName, err)
	}

	cst := time.FixedZone("CST", 8*3600)
	excelEpoch := time.Date(1899, 12, 30, 0, 0, 0, 0, cst)

	type entry struct {
		code, callTime string
		tax, invoice   int
	}
	var entries []entry
	seen := map[string]bool{}
	appIDs := map[string]bool{}
	combo := map[string]int{} // tax / invoice / both
	for i, row := range detail {
		if i == 0 {
			if row["B"] != "税号" || row["D"] != "计费税" || row["E"] != "计费票" {
				fail("明细表头不符合预期: %v", row)
			}
			continue
		}
		raw := strings.TrimSpace(row["B"])
		if raw == "" {
			continue
		}
		code, err := parse.NormalizeCreditCode(raw)
		if err != nil {
			fail("第 %d 行税号非法: %q", i+1, raw)
		}
		if seen[code] {
			fail("第 %d 行税号重复: %s", i+1, code)
		}
		seen[code] = true
		appIDs[strings.TrimSpace(row["C"])] = true
		tax, err1 := flag(row["D"])
		inv, err2 := flag(row["E"])
		if err1 != nil || err2 != nil {
			fail("第 %d 行计费税/计费票不是 0/1: %q/%q", i+1, row["D"], row["E"])
		}
		if tax == 0 && inv == 0 {
			fail("第 %d 行计费税/计费票全为 0: %s", i+1, code)
		}
		days, err := strconv.ParseFloat(strings.TrimSpace(row["A"]), 64)
		if err != nil {
			fail("第 %d 行调用时间非法: %q", i+1, row["A"])
		}
		t := excelEpoch.Add(time.Duration(math.Round(days*86400)) * time.Second)
		entries = append(entries, entry{code: code, tax: tax, invoice: inv, callTime: t.Format(time.RFC3339)})
		switch {
		case tax == 1 && inv == 1:
			combo["both"]++
		case tax == 1:
			combo["tax"]++
		default:
			combo["invoice"]++
		}
	}
	if len(entries) == 0 {
		fail("明细为空")
	}
	if len(appIDs) != 1 {
		fail("明细里出现多个 appId: %v", keys(appIDs))
	}

	// 汇总 sheet：按产品名归类「调用计费量」，与明细计数逐项核对。
	want := map[string]int{}
	for _, row := range summary {
		name := row["C"]
		n, err := strconv.Atoi(strings.TrimSpace(row["D"]))
		if err != nil {
			continue // 表头/标题/小计行
		}
		switch {
		case strings.Contains(name, "+"):
			want["both"] += n
		case strings.Contains(name, "发票"):
			want["invoice"] += n
		case strings.Contains(name, "税务"):
			want["tax"] += n
		}
	}
	for _, k := range []string{"tax", "invoice", "both"} {
		if want[k] != combo[k] {
			fail("明细与汇总不一致: %s 明细=%d 汇总=%d", k, combo[k], want[k])
		}
	}

	f, err := os.Create(out)
	if err != nil {
		fail("创建 %s: %v", out, err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"creditCode", "tax", "invoice", "callTime"})
	for _, e := range entries {
		_ = w.Write([]string{e.code, strconv.Itoa(e.tax), strconv.Itoa(e.invoice), e.callTime})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fail("写 %s: %v", out, err)
	}
	if err := f.Close(); err != nil {
		fail("写 %s: %v", out, err)
	}
	fmt.Printf("OK: %s  共 %d 家（仅税 %d / 仅票 %d / 两者 %d），appId=%v\n",
		out, len(entries), combo["tax"], combo["invoice"], combo["both"], keys(appIDs))
}

func flag(s string) (int, error) {
	switch strings.TrimSpace(s) {
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	}
	return 0, fmt.Errorf("非 0/1: %q", s)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func attr(el xml.StartElement, key string) string {
	for _, a := range el.Attr {
		if a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}

func open(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("xlsx 里没有 %s", name)
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f, err := open(zr, "xl/sharedStrings.xml")
	if err != nil {
		return nil, nil // 没有共享字符串表的 xlsx 合法
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var out []string
	var cur strings.Builder
	inSI, inT := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sharedStrings: %w", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "si":
				out = append(out, cur.String())
				inSI = false
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				cur.Write(el)
			}
		}
	}
}

// sheetPaths 按 workbook.xml + rels 解出 sheet 名 → zip 内路径。
func sheetPaths(zr *zip.Reader) (map[string]string, error) {
	ids := map[string]string{} // sheet 名 → r:id
	if err := walk(zr, "xl/workbook.xml", func(el xml.StartElement) {
		if el.Name.Local == "sheet" {
			ids[attr(el, "name")] = attr(el, "id")
		}
	}); err != nil {
		return nil, err
	}
	targets := map[string]string{} // r:id → 路径
	if err := walk(zr, "xl/_rels/workbook.xml.rels", func(el xml.StartElement) {
		if el.Name.Local == "Relationship" {
			t := attr(el, "Target")
			if strings.HasPrefix(t, "/") {
				t = strings.TrimPrefix(t, "/")
			} else {
				t = path.Join("xl", t)
			}
			targets[attr(el, "Id")] = t
		}
	}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, id := range ids {
		out[name] = targets[id]
	}
	return out, nil
}

func walk(zr *zip.Reader, name string, fn func(xml.StartElement)) error {
	f, err := open(zr, name)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if el, ok := tok.(xml.StartElement); ok {
			fn(el)
		}
	}
}

// readRows 读一个 sheet，每行是「列字母 → 文本」（按单元格引用定位，空单元格不错位）。
func readRows(zr *zip.Reader, sheetPath string, shared []string) ([]map[string]string, error) {
	if sheetPath == "" {
		return nil, fmt.Errorf("sheet 不存在")
	}
	f, err := open(zr, sheetPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var rows []map[string]string
	var row map[string]string
	var col, typ string
	var val strings.Builder
	inV := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sheetPath, err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "row":
				row = map[string]string{}
			case "c":
				col = strings.TrimRight(attr(el, "r"), "0123456789")
				typ = attr(el, "t")
				val.Reset()
			case "v", "t":
				inV = true
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "row":
				rows = append(rows, row)
			case "c":
				v := val.String()
				if typ == "s" {
					idx, err := strconv.Atoi(v)
					if err != nil || idx < 0 || idx >= len(shared) {
						return nil, fmt.Errorf("%s: 共享字符串索引非法 %q", sheetPath, v)
					}
					v = shared[idx]
				}
				row[col] = v
			case "v", "t":
				inV = false
			}
		case xml.CharData:
			if inV {
				val.Write(el)
			}
		}
	}
}
