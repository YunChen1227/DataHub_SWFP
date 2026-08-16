//go:build ignore

// Mock 证通 entcreditapi 聚合平台 implementing /ectcispserver/api/entcreditapi/query
// for swfp full-link testing. Run: go run scripts/mock_entcredit.go
//
// 严格复刻真实协议 (docs/java-api-demo)：application/x-www-form-urlencoded 表单，
// HMAC-SHA256 签名（SignedRequestsHelper.java），args/signature 双重 URLEncode。
// 按 creditCode（统一社会信用代码）驱动场景（与单上游 mock 用 13800000000 触发查无
// 同一惯例）：
//   - 92500233MA60R5KW8M → 四产品全部查得 (resultCode=00000, Status=4)
//   - 91110000EMPTYEMPT0 → 四产品全部查无 (resultCode=00000, Status=1)
//   - 91110000PARTFA0001 → P0130083 返回错误，其余查得（下游同源另一半仍查得）
//   - 91110000FPEMPTY001 → 发票聚合(P0130081/83) 查无、税务查得（下游回落源5 补发票）
//   - 91110000TAXEMP0001 → 税务聚合(P0130082/84) 查无、发票查得（下游按单发票计费）
//   - 验签失败 / 版本号缺失 → 对应文档附录错误码 (E1010 / E1005)
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	orgCode         = env("ENTCREDIT_ORG_CODE", "0100600007")
	accessKeyID     = env("ENTCREDIT_ACCESS_KEY_ID", "demo-swfp-ak")
	secretAccessKey = env("ENTCREDIT_SECRET_ACCESS_KEY", "ZGVtby1zd2ZwLXNrLTMyLWJ5dGVzLWxvbmctc2VjcmV0")
	// endpoint 必须与客户端 config 里的 baseURL 完全一致（参与签名拼接），
	// 与 config.local.mem.yaml 的 versions.swfp.upstream.baseURL 保持同步。
	endpoint = env("ENTCREDIT_ENDPOINT", "http://localhost:9116")
	addr     = env("ENTCREDIT_ADDR", ":9116")
)

const (
	creditCodeNormal   = "92500233MA60R5KW8M"
	creditCodeEmpty    = "91110000EMPTYEMPT0"
	creditCodePartial  = "91110000PARTFA0001"
	creditCodeInvEmpty = "91110000FPEMPTY001" // 仅发票聚合查无（税务照常查得）
	creditCodeTaxEmpty = "91110000TAXEMP0001" // 仅税务聚合查无（发票照常查得）

	requestURI = "/ectcispserver/api/entcreditapi/query"
)

// sampleData 按产品码返回一段明细样例。结构对齐四份 PDF 样例 base64 解码后的真实
// 形态：发票产品 {nsrjbxx, nsrfpxx{五个 List}}、税务产品 {nsrjbxx, nsrswxx{四个
// List}}；并故意携带 xlsx 契约之外的字段（如 kphzxxList 的 yxhpje/yxhpsl），供
// swfp 契约层白名单过滤做回归。
func sampleData(prodCode string) map[string]any {
	nsrjbxx := map[string]string{
		"nsrsbh": creditCodeNormal, "nsrmc": "重庆某某科技有限公司", "hymcdl": "批发和零售业",
		"cybm": "3", "kyrq": "2018-06-01", "nsrzt": "正常", "zzsnsrlx": "2",
		"qydyckpsj": "2024-04-11", "sjjyys": "14", "szdjsswjgdm": "13500233",
		"szdjsswjgmc": "国家税务总局某区税务局",
	}
	switch prodCode {
	case "P0130081", "P0130083": // 发票数据聚合 part1/part2（同构）
		return map[string]any{
			"nsrjbxx": nsrjbxx,
			"nsrfpxx": map[string]any{
				"syhzxxList": []map[string]string{{
					"ssyf": "2025-05", "nsrsbh": creditCodeNormal, "kpqj": "2025-05-31",
					"xfnsrsbh": "92222401MA16M04W14", "xfmc": "某某洗浴城", "xfsl": "1",
					"ljkpcs": "1", "ljkpjebhs": "172.28", "ljse": "1.72", "kpcszb": "1",
					"kpjezb": "1.00000", "kpjepmbhs": "1", "hpjebhs": "0", "hpsl": "0",
					"hpse": "0", "fpjebhs": "0", "fpsl": "0", "fpse": "0",
				}},
				"xyhzxxList": []map[string]string{{
					"ssyf": "2025-04", "nsrsbh": creditCodeNormal, "kpqj": "2025-04-30",
					"gfnsrsbh": "91500233MA5YQWQ44M", "gfnsrmc": "重庆众合共赢科技有限公司",
					"gfsl": "1", "ljkpcs": "1", "ljkpjebhs": "4408.52", "ljse": "44.09",
					"kpcszb": "1", "kpjezb": "1.00000", "kpjepmbhs": "1", "hpjebhs": "0",
					"hpsl": "0", "hpse": "0", "fpjebhs": "0", "fpsl": "0", "fpse": "0",
				}},
				"spxxList": []map[string]string{{
					"ssyf": "2025-04", "nsrsbh": creditCodeNormal, "hwhlwmc": "现代服务",
					"sl": "0.010000", "spzsl": "1", "spzje": "4408.52", "spzse": "44.09",
					"gxfxdhwhlwbmzls": "1", "sphlwzslzb": "1.00000", "sphlwzjezb": "1.00000",
					"jyjezbpm": "1",
				}},
				"khxsdqList": []map[string]string{{
					"ssyf": "2025-04", "nsrsbh": creditCodeNormal, "kpqj": "2025-04-30",
					"gfdjssl": "1", "jycs": "1", "jycszb": "1", "kpjebhs": "4408.52",
					"jyje": "4452.61", "jyjezb": "1.00000", "jyjepm": "1",
					"gfdjsxzqydm": "5002", "gfdjsxzqymc": "重庆市县", "ljse": "44.09",
				}},
				"kphzxxList": []map[string]string{{
					"ssyf": "2026-05", "kpqj": "2026-05-31", "nsrsbh": creditCodeNormal,
					"ljkpcs": "0", "kpje": "0", "ljse": "0", "hpsl": "0", "hpje": "0",
					"hpse": "0", "fpsl": "0", "fpje": "0", "fpse": "0", "dzzgkpjejlp": "0",
					"dzzgkpjehhfp": "0", "dykptsqb": "0", "dykptslp": "null",
					"zjybkpsj": "", "dqlxwjyjlts": "999",
					"yxhpje": "0", "yxhpsl": "0", // xlsx 之外的字段：契约层必须剔除
				}},
			},
		}
	default: // P0130082 / P0130084 税务数据聚合 part1/part2（同构）
		return map[string]any{
			"nsrjbxx": nsrjbxx,
			"nsrswxx": map[string]any{
				"sbsjList": []map[string]string{{
					"nsrsbh": creditCodeNormal, "sbrq": "2026-04-10", "sfzl": "增值税小规模申报表",
					"sssjq": "2026-01-01", "sssjz": "2026-03-31", "qbxssr": "57.09",
					"ysxssr": "57.09", "ynse": "1.71", "yjse": "0.0", "ybtse": "1.71",
					"jmse": "0.0", "sbqx": "2026-03-16",
				}},
				"lrbxxList": []map[string]string{{
					"nsrsbh": creditCodeNormal, "sbrq": "2026-04-10", "sssjq": "2026-01-01",
					"sssjz": "2026-03-31", "xmmc": "营业收入", "bnljje": "57.09", "bys": "57.09",
					"mc": "1",
				}},
				"zcfzbxxList": []map[string]any{{
					"nsrsbh": creditCodeNormal, "sbrq": "2026-04-10", "sssjq": "2026-01-01",
					"sssjz": "2026-03-31", "cwbblxdm": "", "zlbsxlmc": "", "ewbxh": "",
					"zcxmmc": "货币资金", "qmyezc": 23000.44, "ncyezc": 23000.44,
					"fzhsyzqyxmmc": "短期借款", "qmyeqy": 0.0, "ncyeqy": 0.0,
					"sbqx": "2026-03-16", // xlsx 之外的字段：契约层必须剔除
				}},
				"zsbxxList": []map[string]any{{
					"nsrsbh": creditCodeNormal, "sssjq": "2025-01-01", "sssjz": "2025-12-31",
					"jkfsrq": "", "jkzt": "无需扣款", "zsxm": "财务报表", "skzl": "其他税款",
					"sjje": 0, "sl": "", "rkrq": "",
				}},
			},
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// sign 复刻 SignedRequestsHelper.sign()：HMAC-SHA256(toSign, base64decode(sk))，
// 结果 base64 编码后做一次 URLEncode（服务端校验时对收到的 signature 做同样比较，
// 故这里对"已被表单解码一次"的 signature 值，需要先 QueryUnescape 抵消双重编码
// 中的第二层，再与自算的一次编码结果比较）。
func sign(endpoint, uri, version, msgID, org, ak, timestamp, args string) string {
	toSign := strings.Join([]string{
		http.MethodPost, endpoint, uri, version, msgID, org, ak, timestamp, args,
	}, "\n")
	keyBytes, _ := base64.StdEncoding.DecodeString(secretAccessKey)
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(toSign))
	b64 := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return url.QueryEscape(b64)
}

func main() {
	http.HandleFunc(requestURI, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, map[string]any{"resultCode": "E1000", "resultDesc": "查询参数校验不通过"})
			return
		}
		version := r.FormValue("version")
		msgID := r.FormValue("msgId")
		org := r.FormValue("orgCode")
		ak := r.FormValue("accessKeyId")
		timestamp := r.FormValue("timestamp")
		// args/signature 在真实协议里被双重 URLEncode：Go 的 r.ParseForm 已经解码了
		// "表单层"那一次，此处拿到的是仍带一层 URLEncode 的原始字符串（与客户端
		// callProduct 里 url.QueryEscape 后再交给 form.Encode 的结果对应）。
		argsEscaped := r.FormValue("args")
		sigEscaped := r.FormValue("signature")

		if version == "" {
			writeJSON(w, map[string]any{"resultCode": "E1005", "resultDesc": "版本号错误", "orderNo": msgID})
			return
		}
		if msgID == "" {
			writeJSON(w, map[string]any{"resultCode": "E1006", "resultDesc": "MSGID错误", "orderNo": msgID})
			return
		}

		args, err := url.QueryUnescape(argsEscaped)
		if err != nil {
			writeJSON(w, map[string]any{"resultCode": "E1000", "resultDesc": "查询参数校验不通过", "orderNo": msgID})
			return
		}
		expectSig := sign(endpoint, requestURI, version, msgID, org, ak, timestamp, args)
		if ak != accessKeyID {
			writeJSON(w, map[string]any{"resultCode": "E1009", "resultDesc": "accessKeyId错误", "orderNo": msgID})
			return
		}
		if org != orgCode {
			writeJSON(w, map[string]any{"resultCode": "E1012", "resultDesc": "机构代码错误", "orderNo": msgID})
			return
		}
		if sigEscaped != expectSig {
			log.Printf("sign mismatch: got=%s want=%s toSign-args=%s", sigEscaped, expectSig, args)
			writeJSON(w, map[string]any{"resultCode": "E1010", "resultDesc": "signature错误", "orderNo": msgID})
			return
		}

		var argsMap struct {
			ProdCode   string `json:"prodCode"`
			CreditCode string `json:"creditCode"`
		}
		if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
			writeJSON(w, map[string]any{"resultCode": "E1000", "resultDesc": "查询参数校验不通过", "orderNo": msgID})
			return
		}
		if argsMap.CreditCode == "" {
			writeJSON(w, map[string]any{"resultCode": "E1000", "resultDesc": "查询参数校验不通过,creditCode:为必填项", "orderNo": msgID})
			return
		}

		empty := func() {
			writeJSON(w, map[string]any{
				"orderNo":    msgID,
				"resultCode": "00000",
				"resultDesc": "成功",
				"resultData": map[string]any{argsMap.ProdCode + "Status": "1"},
			})
		}
		isInvoiceProd := argsMap.ProdCode == "P0130081" || argsMap.ProdCode == "P0130083"

		switch {
		case argsMap.CreditCode == creditCodeEmpty:
			empty()
		case argsMap.CreditCode == creditCodeInvEmpty && isInvoiceProd:
			empty() // 发票聚合查无 → 下游回落源5 补发票维度
		case argsMap.CreditCode == creditCodeTaxEmpty && !isInvoiceProd:
			empty() // 税务聚合查无 → 下游只得发票维度，按【单发票】计费
		case argsMap.CreditCode == creditCodePartial && argsMap.ProdCode == "P0130083":
			writeJSON(w, map[string]any{"resultCode": "E0400", "resultDesc": "查询征信数据出错", "orderNo": msgID})
		default:
			plain, _ := json.Marshal(sampleData(argsMap.ProdCode))
			writeJSON(w, map[string]any{
				"orderNo":    msgID,
				"resultCode": "00000",
				"resultDesc": "成功",
				"packetCnt":  1,
				"resultData": map[string]any{
					argsMap.ProdCode + "Status": "4",
					argsMap.ProdCode + "Data": map[string]any{
						"result": []map[string]string{{"data": base64.StdEncoding.EncodeToString(plain)}},
					},
				},
			})
		}
	})

	fmt.Printf("mock entcredit (证通 entcreditapi 聚合, 四产品) listening on %s  orgCode=%s accessKeyId=%s\n", addr, orgCode, accessKeyID)
	log.Fatal(http.ListenAndServe(addr, nil))
}
