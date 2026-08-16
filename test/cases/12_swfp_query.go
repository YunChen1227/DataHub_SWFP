//go:build ignore

// 12_swfp_query: swfp 版本 POST /v1/openapi/zlx/querySrmxSWFP（x1 信封格式；
// 企业维度入参 creditCode + 可选 scope / dataType）。
//
// 上游调用改为「按优先级串行、命中即停」后本用例的口径也随之变化：
//   - 证通发票源(源1+源2) 与 证通税务源(源3+源4) 各是一个逻辑源，源内两次调用互补，
//     必须一起发出；
//   - 源5 销项数据与证通发票源同供【发票】维度但优先级更低，因此证通发票源查得时
//     源5 根本不会被调用（sourceStatus=skipped，这就是省下来的钱）；
//   - 收费标准按【实际查得的维度】判定，range 里的 dataScope/feeStandard 对下游明示。
//
// 全场景：两项皆得(001, feeStandard=both) / scope=basic / dataType 单项 /
// 发票查无回落源5 / 税务查无按单发票计费 / 同源半边失败 / 全部查无(999) / 鉴权与参数错误。
//
// Run: go run test/cases/12_swfp_query.go
package main

import (
	"encoding/json"
	"strings"

	"github.com/datahub/relay/test/harness"
)

const version = "swfp"

// mock_entcredit.go / mock_salesdata.go 约定的场景驱动值（合法统一社会信用代码格式）。
const (
	creditCodeNormal    = "92500233MA60R5KW8M" // 证通四产品 + 源5 均查得
	creditCodeEmpty     = "91110000EMPTYEMPT0" // 全部查无
	creditCodePartial   = "91110000PARTFA0001" // P0130083(源2) 失败，其余查得
	creditCodeInvEmpty  = "91110000FPEMPTY001" // 证通发票聚合查无 → 回落源5
	creditCodeTaxEmpty  = "91110000TAXEMP0001" // 证通税务聚合查无 → 仅得发票
	creditCodeSalesFail = "91110000BADFA00001" // 源5 失败（证通查得时源5 不会被调用）
)

func body(creditCode string) map[string]string {
	return map[string]string{"creditCode": creditCode}
}

func bodyWith(creditCode, key, val string) map[string]string {
	return map[string]string{"creditCode": creditCode, key: val}
}

func main() {
	rec := harness.NewRecorder("12_swfp_query", "swfp 主接口全场景 (串行寻源 + 按实得维度计费, xlsx 契约输出)")
	defer rec.Finish()

	appKey := harness.AppKeyFor(version)

	r := harness.Query(version, appKey, harness.Secret, body(creditCodeNormal), nil)
	rec.Check("两项皆得(源5 被短路)", "errorCode=0 & body.code=001 & 源1-源4 ok & 源5 skipped & feeStandard=both",
		r.ErrorCode == "0" && r.BodyCode == "001" && baseSourcesOK(r.Range) &&
			statusOf(r.Range, "源5") == "skipped" && feeOf(r.Range) == "both" &&
			scopeOf(r.Range, "发票") && scopeOf(r.Range, "税务"), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "scope", "basic"), nil)
	rec.Check("scope=basic 不调用源5", "errorCode=0 & body.code=001 & 源5 skipped & 无源5 数据",
		r.ErrorCode == "0" && r.BodyCode == "001" && baseSourcesOK(r.Range) &&
			statusOf(r.Range, "源5") == "skipped" && !hasSalesData(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "invoice"), nil)
	rec.Check("dataType=invoice 只调发票源", "errorCode=0 & body.code=001 & 源3/源4 skipped & feeStandard=invoice",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			statusOf(r.Range, "源1") == "ok" && statusOf(r.Range, "源3") == "skipped" &&
			statusOf(r.Range, "源4") == "skipped" && feeOf(r.Range) == "invoice" &&
			!scopeOf(r.Range, "税务"), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "tax"), nil)
	rec.Check("dataType=tax 只调税务源", "errorCode=0 & body.code=001 & 源1/源2/源5 skipped & feeStandard=tax",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			statusOf(r.Range, "源3") == "ok" && statusOf(r.Range, "源1") == "skipped" &&
			statusOf(r.Range, "源5") == "skipped" && feeOf(r.Range) == "tax" &&
			!scopeOf(r.Range, "发票"), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "wrong"), nil)
	rec.Check("dataType 非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeInvEmpty), nil)
	rec.Check("发票源查无回落源5", "errorCode=0 & body.code=001 & 源1 empty & 源5 ok(带映射后字段) & feeStandard=both",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			statusOf(r.Range, "源1") == "empty" && statusOf(r.Range, "源5") == "ok" &&
			hasSalesData(r.Range) && feeOf(r.Range) == "both", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeTaxEmpty), nil)
	rec.Check("税务源查无按单发票计费", "errorCode=0 & body.code=001 & 源3 empty & feeStandard=invoice",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			statusOf(r.Range, "源3") == "empty" && feeOf(r.Range) == "invoice" &&
			scopeOf(r.Range, "发票") && !scopeOf(r.Range, "税务"), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodePartial), nil)
	rec.Check("同源半边失败仍查得", "errorCode=0 & body.code=001 & 源2 error 且源1 ok & feeStandard=both",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			statusOf(r.Range, "源2") == "error" && statusOf(r.Range, "源1") == "ok" &&
			feeOf(r.Range) == "both", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeSalesFail), nil)
	rec.Check("源5 失败但已被短路", "errorCode=0 & body.code=001 & 源5 skipped(未调用即无失败)",
		r.ErrorCode == "0" && r.BodyCode == "001" && statusOf(r.Range, "源5") == "skipped", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeEmpty), nil)
	rec.Check("全部查无", "errorCode=0 & body.code=999", r.ErrorCode == "0" && r.BodyCode == "999", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeNormal), map[string]any{"sign": "deadbeef"})
	rec.Check("错误签名", "errorCode=505002 且无 body", r.ErrorCode == "505002" && r.BodyCode == "", r.Raw)

	r = harness.Query(version, "nonexistent-appkey", harness.Secret, body(creditCodeNormal), nil)
	rec.Check("未知 appKey", "errorCode=505004", r.ErrorCode == "505004", r.Raw)

	r = harness.Query(version, "", harness.Secret, body(creditCodeNormal), map[string]any{"appKey": ""})
	rec.Check("缺失 appKey", "errorCode=505001", r.ErrorCode == "505001", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body("12345"), nil)
	rec.Check("creditCode 非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "scope", "invalid"), nil)
	rec.Check("scope 非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	// 个人三要素入参对 swfp 无效（缺 creditCode → 参数拦截）。
	r = harness.Query(version, appKey, harness.Secret,
		map[string]string{"mobile": "13809091009", "idCard": "330129199109094312"}, nil)
	rec.Check("缺 creditCode 拦截", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeNormal), nil)
	rec.Check("二次两项皆得", "errorCode=0 & body.code=001", r.ErrorCode == "0" && r.BodyCode == "001", r.Raw)
}

// contract mirrors the 契约输出结构 (swfpcontract.go)。
type contract struct {
	Invoice      map[string]map[string]json.RawMessage `json:"发票数据聚合"`
	Tax          map[string]map[string]json.RawMessage `json:"税务数据聚合"`
	SourceStatus map[string]string                     `json:"sourceStatus"`
	DataScope    map[string]bool                       `json:"dataScope"`
	FeeStandard  string                                `json:"feeStandard"`
}

func parseContract(raw string) *contract {
	if raw == "" || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return nil
	}
	var c contract
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil
	}
	return &c
}

// baseSourcesOK 校验契约结构：两段齐全、证通四源全 ok、发票段 nsrjbxx 有源1、
// 税务段 lrbxxList 键存在。
func baseSourcesOK(raw string) bool {
	c := parseContract(raw)
	if c == nil || c.Invoice == nil || c.Tax == nil {
		return false
	}
	for _, s := range []string{"源1", "源2", "源3", "源4"} {
		if c.SourceStatus[s] != "ok" {
			return false
		}
	}
	if _, ok := c.Invoice["nsrjbxx"]["源1"]; !ok {
		return false
	}
	_, ok := c.Tax["lrbxxList"]
	return ok
}

// hasSalesData 校验源5 条目已完成字段映射（含 xlsx 契约名 ssyf 而非上游原名 belongMonth）。
func hasSalesData(raw string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	rows, ok := c.Invoice["kphzxxList"]["源5"]
	return ok && strings.Contains(string(rows), "ssyf") && !strings.Contains(string(rows), "belongMonth")
}

// statusOf 读取 sourceStatus 中某源的状态（缺失返回空串）。
func statusOf(raw, source string) string {
	c := parseContract(raw)
	if c == nil {
		return ""
	}
	return c.SourceStatus[source]
}

// feeOf 读取本次实得维度对应的收费标准 (both/invoice/tax/none)。
func feeOf(raw string) string {
	c := parseContract(raw)
	if c == nil {
		return ""
	}
	return c.FeeStandard
}

// scopeOf 读取某维度本次是否查得。
func scopeOf(raw, dim string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	return c.DataScope[dim]
}
