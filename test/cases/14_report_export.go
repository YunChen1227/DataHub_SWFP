//go:build ignore

// 14_report_export: 用量统计页的出账/对账导出——上游列表、下游账单（全部客户/指定客户）、
// 上游对账（四家公司）都返回合法 xlsx；参数错误 400，无 token 401。
//
// Run: go run test/cases/14_report_export.go
package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/datahub/relay/test/harness"
)

// download 发 GET 并返回状态码、响应头与原始字节（harness.Call 只解析 JSON）。
func download(path string, headers map[string]string) (int, http.Header, []byte) {
	req, err := http.NewRequest(http.MethodGet, harness.BaseURL()+path, nil)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// sheetCount 返回 xlsx 里的 sheet 数；不是合法 xlsx 返回 -1。
func sheetCount(b []byte) int {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return -1
	}
	n := 0
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") {
			n++
		}
	}
	return n
}

func main() {
	rec := harness.NewRecorder("14_report_export", "出账/对账 Excel 导出")
	defer rec.Finish()

	token, raw := harness.AdminLogin()
	rec.Check("登录", "返回 token", token != "", raw)
	if token == "" {
		return
	}
	auth := harness.AuthHeader(token)
	base := harness.AdminBase("swfp")

	// 1. 上游列表
	st, m, vr := harness.Call(http.MethodGet, base+"/reports/vendors", nil, auth)
	vendors, _ := m["vendors"].([]any)
	rec.Check("上游列表", "HTTP 200 且 4 家上游", st == 200 && len(vendors) == 4, vr)

	cst := time.FixedZone("CST", 8*3600)
	to := time.Now().In(cst).Format("2006-01-02")
	from := time.Now().In(cst).AddDate(0, 0, -30).Format("2006-01-02")
	rng := "&from=" + from + "&to=" + to

	// 2. 下游账单（全部客户）
	sd, hd, bd := download(base+"/reports/downstream?customer="+rng, auth)
	cd := hd.Get("Content-Disposition")
	rec.Check("下游账单(全部客户)", "HTTP 200、xlsx、至少 4 个 sheet、文件名含「下游账单」",
		sd == 200 && strings.Contains(hd.Get("Content-Type"), "spreadsheetml") && sheetCount(bd) >= 4 &&
			strings.Contains(cd, url.PathEscape("下游账单")),
		"status="+strconv.Itoa(sd)+" sheets="+strconv.Itoa(sheetCount(bd))+" cd="+cd)

	// 3. 下游账单（指定客户：取用户列表第一个有名称的用户）
	_, um, _ := harness.Call(http.MethodGet, base+"/users", nil, auth)
	users, _ := um["users"].([]any)
	name := ""
	for _, u := range users {
		if n, _ := u.(map[string]any)["name"].(string); n != "" {
			name = n
			break
		}
	}
	if name != "" {
		sc, _, bc := download(base+"/reports/downstream?customer="+url.QueryEscape(name)+rng, auth)
		rec.Check("下游账单(指定客户)", "HTTP 200 且合法 xlsx", sc == 200 && sheetCount(bc) >= 4,
			"customer="+name+" status="+strconv.Itoa(sc))
	}

	// 4. 上游对账：四家公司
	for _, v := range vendors {
		vm, _ := v.(map[string]any)
		id, _ := vm["id"].(string)
		vn, _ := vm["name"].(string)
		su, _, bu := download(base+"/reports/upstream?vendor="+id+rng, auth)
		rec.Check("上游对账("+vn+")", "HTTP 200、5 个 sheet", su == 200 && sheetCount(bu) == 5,
			"status="+strconv.Itoa(su)+" sheets="+strconv.Itoa(sheetCount(bu)))
	}

	// 5. 参数错误
	s1, _, b1 := download(base+"/reports/upstream?vendor=nope"+rng, auth)
	rec.Check("未知上游", "HTTP 400", s1 == 400, string(b1))
	s2, _, b2 := download(base+"/reports/downstream?from="+to+"&to="+from, auth)
	rec.Check("起始晚于截止", "HTTP 400", s2 == 400 || from == to, string(b2))
	s3, _, b3 := download(base+"/reports/downstream?customer="+url.QueryEscape("不存在的客户-xyz")+rng, auth)
	rec.Check("不存在的客户", "HTTP 400", s3 == 400, string(b3))

	// 6. 无 token
	s4, _, _ := download(base+"/reports/downstream?customer="+rng, nil)
	rec.Check("无 token 导出", "HTTP 401", s4 == 401, "status="+strconv.Itoa(s4))
}
