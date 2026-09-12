//go:build ignore

// Mock 税票数据查询C 上游 (惠众征信, docs/【税票数据查询c】接口文档.docx)，swfp
// 源6 的全链路测试挡板。Run: go run scripts/mock_ctax.go
//
// 严格复刻客户端的协议假设 (internal/infrastructure/upstream/ctax.go)：
//
//	POST /c/tax  {"param":{"identity":"…","type":"1|2|3"}}
//	          →  {"result":{"code":"SYS200","msg":"查询成功","data":{…}}}
//
// 按 identity（= 下游 creditCode）驱动场景（与 mock_entcredit/mock_salesdata 同一惯例）：
//   - 91110000CTAXALL001 → SYS200，按 type 返回两维/单维数据（综合源一次拿全）
//   - 91110000CTAXTAX001 → SYS200，但只有税务段（发票维度需由证通发票源补齐）
//   - 91110000CTAX500001 → 500 异常（该源失败）
//   - 其余税号            → SYS404 查无数据
//
// ⚠ 其余税号一律查无是**有意**的：源6 是综合源，寻源器在两项请求时先试它，若它对
// 既有场景税号也查得，证通/源5 的既有用例会全部被短路掉、断言全废。要验证"综合源
// 命中即停"用上面专属的场景税号。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var addr = env("CTAX_ADDR", ":9126")

const (
	creditAll  = "91110000CTAXALL001" // 两维皆查得
	creditTax  = "91110000CTAXTAX001" // 只有税务段
	creditFail = "91110000CTAX500001" // 上游异常
)

// type 入参 (文档 §3)。
const (
	typeTax     = "1"
	typeInvoice = "2"
	typeBoth    = "3"
)

// invoiceData 是发票维度的业务段样例 (文档 表4/表6/表11/表12/表13)。
func invoiceData(identity string) map[string]any {
	return map[string]any{
		"kphzxx": []map[string]any{{
			"ssyf": "2026-05", "kpqj": "2026-05-31", "nsrsbh": identity,
			"ljkpcs": "12", "kpje": "888.00", "ljse": "115.44",
			"hpsl": "1", "hpje": "-10.00", "hpse": "-1.30",
			"fpsl": "0", "fpje": "0.00", "fpse": "0.00",
			"dzzgkpjejlp": "500.00", "dzzgkpjehhfp": "600.00",
			"dykptsqb": "6", "dykptslp": "5", "zjybkpsj": "2026-05-28", "dqlxwjyjlts": "3",
			"yxhpje": "-10.00", // xlsx 之外的字段：契约层白名单应剔除
		}},
		"spxx": []map[string]any{{
			"ssyf": "2026-05", "nsrsbh": identity, "hwhlwbm": "1090511020000000000",
			"hwhlwmc": "咨询服务", "sl": "0.06000", "spzsl": "3", "spzje": "600.00", "spzse": "36.00",
			"gxfxdhwhlwbmzls": "4", "sphlwzslzb": "0.300", "sphlwzjezb": "0.675", "jyjezbpm": "1",
		}},
		"xyhzxx": []map[string]any{{
			"ssyf": "2026-05", "nsrsbh": identity, "kpqj": "2026-05-31",
			"gfnsrsbh": "91500233MA5YQWQ44M", "gfnsrmc": "重庆众合共赢科技有限公司", "gfsl": "7",
			"ljkpcs": "3", "ljkpjebhs": "300.00", "ljse": "39.00",
			"kpcszb": "0.250", "kpjezb": "0.338", "kpjepmbhs": "1",
			"xyqyhydm": "721", "xyqyhymc": "软件和信息技术服务业",
		}},
		"khxsdq": []map[string]any{{
			"ssyf": "2026-05", "nsrsbh": identity, "kpqj": "2026-05-31",
			"gfdjssl": "2", "jycs": "5", "jycszb": "0.416", "kpjebhs": "500.00",
			"jyjezb": "0.563", "jyjepm": "1", "gfdjssjjgdm": "150000",
			"gfdjsxzqydm": "500000", "gfdjsxzqymc": "重庆市", "ljse": "65.00",
		}},
		"syhzxx": []map[string]any{{
			"ssyf": "2026-05", "nsrsbh": identity, "kpqj": "2026-05-31",
			"xfnsrsbh": "91500000MA5YQWQ55M", "xfmc": "某供应商", "xfsl": "4",
			"ljkpcs": "2", "ljkpjebhs": "172.28", "ljse": "22.40",
			"syqyhydm": "511", "syqyhymc": "批发业",
		}},
	}
}

// taxData 是税务维度的业务段样例 (文档 表2/表3/表7/表9)。利润表与资产负债表带
// 一层嵌套 data（表8/表10 的项目行），契约层需与父级拍平。
func taxData(identity string) map[string]any {
	return map[string]any{
		"sbsj": []map[string]any{{
			"nsrsbh": identity, "sbrq": "2026-04-15", "sfzl": "增值税",
			"sssjq": "2026-03-01", "sssjz": "2026-03-31",
			"qbxssr": "10000.00", "ysxssr": "10000.00", "ynse": "1300.00",
			"yjse": "0.00", "ybtse": "1300.00", "jmse": "0.00", "sbqx": "2026-04-20",
		}},
		"zsbxx": []map[string]any{{
			"nsrsbh": identity, "sssjq": "2026-03-01", "sssjz": "2026-03-31",
			"jkjzrq": "2026-04-20", "jkfsrq": "2026-04-15", "jkzt": "扣款成功",
			"zsxm": "增值税", "skzl": "正税", "jsje": "10000.00", "sl": "0.13000", "sjje": "1300.00",
		}},
		"lrbxx": []map[string]any{{
			"nsrsbh": identity, "sbrq": "2026-04", "sssjq": "2026-01-01", "sssjz": "2026-03-31",
			"data": []map[string]any{
				{"xmmc": "营业收入", "sqje": "9000.00", "bys": "3000.00", "bnljje": "10000.00"},
				{"xmmc": "营业成本", "sqje": "6000.00", "bys": "2000.00", "bnljje": "7000.00"},
			},
		}},
		"zcfzbxx": []map[string]any{{
			"nsrsbh": identity, "sbrq": "2026-04", "sssjq": "2026-01-01", "sssjz": "2026-03-31",
			"cwbblxdm": "101", "zlbsxlmc": "资产负债表",
			"data": []map[string]any{{
				"ewbxh": "1", "zcxmmc": "货币资金", "qmyezc": "5000.00", "ncyezc": "4000.00",
				"fzhsyzqyxmmc": "短期借款", "qmyeqy": "1000.00", "ncyeqy": "800.00",
			}},
		}},
	}
}

// nsrjbxx 是两个维度共有的纳税人基本信息 (文档 表5)。
func nsrjbxx(identity string) map[string]any {
	return map[string]any{
		"nsrsbh": identity, "nsrmc": "重庆某某科技有限公司",
		"hybmdl": "651", "hymcdl": "软件和信息技术服务业",
		"cybm": "3", "cymc": "第三产业", "yqjydz": "重庆市南岸区某街道",
		"szdjsswjgdm": "500000", "szdjsswjgmc": "国家税务总局重庆市税务局",
		"qydyckpsj": "2019-03-11", "sjjyys": "86", "kyrq": "2018-12-28",
		"nsrzt": "03", "zzsnsrlx": "2000010001", "nsrxypddj": "B",
		"djzclxDm": "1100", "djzclxmc": "内资企业",
	}
}

// buildData 按 type 组装 data：只给要的那一维（客户端按 dataType 传 type，
// 上游也只该回那一维——否则我们会为没交付的维度付钱）。
func buildData(identity, queryType string, taxOnly bool) map[string]any {
	data := map[string]any{"nsrjbxx": nsrjbxx(identity)}
	if queryType != typeInvoice {
		for k, v := range taxData(identity) {
			data[k] = v
		}
	}
	if queryType != typeTax && !taxOnly {
		for k, v := range invoiceData(identity) {
			data[k] = v
		}
	}
	return data
}

func respond(w http.ResponseWriter, code, msg string, data map[string]any) {
	out := map[string]any{"code": code, "msg": msg}
	if data != nil {
		out["data"] = data
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": out})
}

func handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Param struct {
			Identity string `json:"identity"`
			Type     string `json:"type"`
		} `json:"param"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		respond(w, "500", "报文解析失败", nil)
		return
	}
	p := req.Param
	if p.Identity == "" || p.Type == "" {
		respond(w, "500", "identity/type 为必填项", nil)
		return
	}
	switch p.Type {
	case typeTax, typeInvoice, typeBoth:
	default:
		respond(w, "500", "type 取值非法", nil)
		return
	}

	switch p.Identity {
	case creditAll:
		respond(w, "SYS200", "查询成功", buildData(p.Identity, p.Type, false))
	case creditTax:
		if p.Type == typeInvoice {
			respond(w, "SYS404", "查无数据", nil) // 只有税务的主体，被问发票即查无
			return
		}
		respond(w, "SYS200", "查询成功", buildData(p.Identity, p.Type, true))
	case creditFail:
		respond(w, "500", "系统异常", nil)
	default:
		respond(w, "SYS404", "查无数据", nil)
	}
}

func main() {
	http.HandleFunc("/c/tax", handle)
	fmt.Printf("mock ctax (税票数据查询C, swfp 源6) listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
