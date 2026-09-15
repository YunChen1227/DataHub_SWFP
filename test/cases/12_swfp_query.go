//go:build ignore

// 12_swfp_query: swfp 版本 POST /v1/openapi/zlx/querySrmxSWFP（x1 信封格式；
// 企业维度入参 creditCode + 可选 scope / dataType）。
//
// 上游调用按优先级串行、命中即停；契约层对下游输出扁平业务数据（无源编号 /
// sourceStatus / feeStandard），下游凭 dataScope 判断实得维度。逐源 hit/skip
// 由 sourcing 单测与后台轨迹覆盖，本用例只断言对外可观测结果。
//
// 全场景：综合源一次拿全 / 综合源半边 + 缺项补齐 / 综合源查无回落既有源 /
// scope=basic / dataType 单项 / 发票查无回落销项 / 税务查无按单发票 /
// 同源半边失败 / 全部查无(999) / 鉴权与参数错误。
//
// Run: go run test/cases/12_swfp_query.go
package main

import (
	"encoding/json"
	"strings"

	"github.com/datahub/relay/test/harness"
)

const version = "swfp"

// mock_entcredit.go / mock_salesdata.go / mock_ctax.go 约定的场景驱动值
// （合法统一社会信用代码格式；字符集不含 I/O/S/V/Z）。
//
// 注意 mock_ctax 只对下面两个 CTAX 专属税号查得，其余一律 SYS404：综合源两项
// 请求时排在最前，若它对既有场景税号也查得，证通/销项的用例会全部被短路掉。
const (
	creditCodeNormal    = "92500233MA60R5KW8M" // 证通四产品 + 销项均查得（综合源查无）
	creditCodeEmpty     = "91110000EMPTYEMPT0" // 全部查无
	creditCodePartial   = "91110000PARTFA0001" // P0130083 失败，其余查得
	creditCodeInvEmpty  = "91110000FPEMPTY001" // 证通发票聚合查无 → 回落销项
	creditCodeTaxEmpty  = "91110000TAXEMP0001" // 证通税务聚合查无 → 仅得发票
	creditCodeSalesFail = "91110000BADFA00001" // 销项失败（证通查得时销项不会被调用）
	creditCodeCTaxAll   = "91110000CTAXALL001" // 综合源一次拿全两维
	creditCodeCTaxTax   = "91110000CTAXTAX001" // 综合源只给税务 → 发票由证通补齐
)

func body(creditCode string) map[string]string {
	return map[string]string{"creditCode": creditCode}
}

func bodyWith(creditCode, key, val string) map[string]string {
	return map[string]string{"creditCode": creditCode, key: val}
}

func main() {
	rec := harness.NewRecorder("12_swfp_query", "swfp 主接口全场景 (串行寻源 + 扁平契约输出)")
	defer rec.Finish()

	appKey := harness.AppKeyFor(version)

	r := harness.Query(version, appKey, harness.Secret, body(creditCodeNormal), nil)
	rec.Check("两项皆得(扁平契约)", "errorCode=0 & body.code=001 & 两段齐全 & dataScope 全真 & 无源泄漏",
		r.ErrorCode == "0" && r.BodyCode == "001" && baseContractOK(r.Range) &&
			scopeOf(r.Range, "发票") && scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeCTaxAll), nil)
	rec.Check("综合源一次拿全两维", "errorCode=0 & body.code=001 & 税务段有申报数据 & dataScope 全真",
		r.ErrorCode == "0" && r.BodyCode == "001" && hasCTaxTaxData(r.Range) &&
			scopeOf(r.Range, "发票") && scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeCTaxTax), nil)
	rec.Check("综合源半边 + 缺项补齐", "errorCode=0 & body.code=001 & 税务有申报 & 发票有开票汇总",
		r.ErrorCode == "0" && r.BodyCode == "001" && hasCTaxTaxData(r.Range) &&
			hasInvoiceSummary(r.Range) && scopeOf(r.Range, "发票") &&
			scopeOf(r.Range, "税务") && !leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "scope", "basic"), nil)
	rec.Check("scope=basic 不调用销项", "errorCode=0 & body.code=001 & 无销项映射字段",
		r.ErrorCode == "0" && r.BodyCode == "001" && baseContractOK(r.Range) &&
			!hasSalesData(r.Range) && !leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "invoice"), nil)
	rec.Check("dataType=invoice 只得发票", "errorCode=0 & body.code=001 & dataScope 仅发票",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			scopeOf(r.Range, "发票") && !scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "tax"), nil)
	rec.Check("dataType=tax 只得税务", "errorCode=0 & body.code=001 & dataScope 仅税务",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			scopeOf(r.Range, "税务") && !scopeOf(r.Range, "发票") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, bodyWith(creditCodeNormal, "dataType", "wrong"), nil)
	rec.Check("dataType 非法", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeInvEmpty), nil)
	rec.Check("发票源查无回落销项", "errorCode=0 & body.code=001 & 销项映射字段在 & dataScope 全真",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			hasSalesData(r.Range) && scopeOf(r.Range, "发票") &&
			scopeOf(r.Range, "税务") && !leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeTaxEmpty), nil)
	rec.Check("税务源查无仅发票", "errorCode=0 & body.code=001 & dataScope 仅发票",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			scopeOf(r.Range, "发票") && !scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodePartial), nil)
	rec.Check("同源半边失败仍查得", "errorCode=0 & body.code=001 & dataScope 全真",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			scopeOf(r.Range, "发票") && scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeSalesFail), nil)
	rec.Check("销项失败但已被短路", "errorCode=0 & body.code=001 & 仍两项皆得",
		r.ErrorCode == "0" && r.BodyCode == "001" &&
			scopeOf(r.Range, "发票") && scopeOf(r.Range, "税务") &&
			!leaksSource(r.Range), r.Raw)

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

	r = harness.Query(version, appKey, harness.Secret,
		map[string]string{"mobile": "13809091009", "idCard": "330129199109094312"}, nil)
	rec.Check("缺 creditCode 拦截", "errorCode=505062", r.ErrorCode == "505062", r.Raw)

	r = harness.Query(version, appKey, harness.Secret, body(creditCodeNormal), nil)
	rec.Check("二次两项皆得", "errorCode=0 & body.code=001", r.ErrorCode == "0" && r.BodyCode == "001", r.Raw)
}

// contract mirrors the flattened 契约输出结构。
type contract struct {
	Invoice   map[string]json.RawMessage `json:"发票数据聚合"`
	Tax       map[string]json.RawMessage `json:"税务数据聚合"`
	DataScope map[string]bool            `json:"dataScope"`
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

func leaksSource(raw string) bool {
	for _, banned := range []string{"源1", "源2", "源3", "源4", "源5", "源6", "sourceStatus", "feeStandard"} {
		if strings.Contains(raw, banned) {
			return true
		}
	}
	return false
}

// baseContractOK 校验扁平契约：两段齐全、nsrjbxx 为对象、关键列表键存在。
func baseContractOK(raw string) bool {
	c := parseContract(raw)
	if c == nil || c.Invoice == nil || c.Tax == nil {
		return false
	}
	var jb map[string]string
	if err := json.Unmarshal(c.Invoice["nsrjbxx"], &jb); err != nil || jb["nsrmc"] == "" {
		return false
	}
	if _, ok := c.Tax["lrbxxList"]; !ok {
		return false
	}
	var kphz []map[string]string
	if err := json.Unmarshal(c.Invoice["kphzxxList"], &kphz); err != nil || len(kphz) == 0 {
		return false
	}
	return true
}

func hasInvoiceSummary(raw string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	var kphz []map[string]string
	if err := json.Unmarshal(c.Invoice["kphzxxList"], &kphz); err != nil {
		return false
	}
	return len(kphz) > 0
}

// hasSalesData 校验销项条目已完成字段映射（含 xlsx 契约名 ssyf，而非上游原名 belongMonth）。
func hasSalesData(raw string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	var kphz []map[string]string
	if err := json.Unmarshal(c.Invoice["kphzxxList"], &kphz); err != nil {
		return false
	}
	for _, row := range kphz {
		if row["ssyf"] == "2024-03" && row["kpje"] != "" {
			return !strings.Contains(raw, "belongMonth")
		}
	}
	return false
}

// hasCTaxTaxData 校验综合源税务段已完成契约映射：sbsjList 有条目，利润表已拍平。
func hasCTaxTaxData(raw string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	var sbsj []map[string]string
	if err := json.Unmarshal(c.Tax["sbsjList"], &sbsj); err != nil || len(sbsj) == 0 {
		return false
	}
	if !strings.Contains(string(c.Tax["sbsjList"]), "sfzl") {
		return false
	}
	var lrb []map[string]string
	if err := json.Unmarshal(c.Tax["lrbxxList"], &lrb); err != nil {
		return false
	}
	return strings.Contains(string(c.Tax["lrbxxList"]), "xmmc") &&
		!strings.Contains(string(c.Tax["lrbxxList"]), `"data"`)
}

func scopeOf(raw, dim string) bool {
	c := parseContract(raw)
	if c == nil {
		return false
	}
	return c.DataScope[dim]
}
