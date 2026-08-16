package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

const testCreditCode = "92500233MA60R5KW8M"

// contractOut mirrors the 契约输出结构 for assertions.
type contractOut struct {
	Invoice      map[string]map[string]json.RawMessage `json:"发票数据聚合"`
	Tax          map[string]map[string]json.RawMessage `json:"税务数据聚合"`
	SourceStatus map[string]string                     `json:"sourceStatus"`
	DataScope    map[string]bool                       `json:"dataScope"`
	FeeStandard  string                                `json:"feeStandard"`
}

// entInvoiceData 构造一份证通发票段解码明细（含 xlsx 外字段 yxhpje，应被白名单剔除）。
func entInvoiceData() string {
	return `{"nsrjbxx":{"nsrsbh":"` + testCreditCode + `","nsrmc":"某某公司","kyrq":"2018-01-01"},
	"nsrfpxx":{"kphzxxList":[{"ssyf":"2026-05","kpqj":"2026-05-31","nsrsbh":"` + testCreditCode + `",
	"ljkpcs":"3","kpje":"100.00","ljse":"13.00","yxhpje":"9","dykptslp":"null"}],
	"syhzxxList":[{"ssyf":"2025-05","nsrsbh":"` + testCreditCode + `","xfmc":"某销方","ljkpjebhs":"172.28"}]}}`
}

func entTaxData() string {
	return `{"nsrjbxx":{"nsrsbh":"` + testCreditCode + `","nsrmc":"某某公司"},
	"nsrswxx":{"sbsjList":[{"sssjq":"2026-01-01","nsrsbh":"` + testCreditCode + `","ynse":1.71,"sbqx":"2026-03-16"}],
	"zsbxxList":[{"sssjq":"2025-01-01","zsxm":"财务报表","sjje":0,"jkzt":"无需扣款"}]}}`
}

// salesData 构造一份源5 (销项数据) 合并明细（summaryIndicators 应被整体丢弃）。
func salesData() string {
	return `{"salesInvoice":[{"belongMonth":202403,"invoiceAmtMonth":1234.56,"taxAmtMonth":160.5,
	"invoiceCntMonth":8,"invoiceHighAmtMonth":500,"allInvoiceHighAmtMonth":600,
	"redInvoiceAmtMonth":-10,"redTaxAmtMonth":-1.3,"redInvoiceCntMonth":1,
	"nullifiedInvoiceAmtMonth":0,"nullifiedInvoiceCntMonth":0,"nullTaxAmtMonth":0,
	"invoiceDayMonth":5,"blueInvoiceDayMonth":4,"latestInvoiceDate":"20240328","noTradeRcordDay":3}],
	"summaryIndicators":{"inputL1ySaleActualAmt":99999},
	"monthlyDownstreamInfo":[{"belongMonth":202403,"buyerName":"某购方","buyerTaxpayerIdNum":"91500000XXXX",
	"tradeAmtRankMonth":1,"tradeAmtMonth":800,"taxAmtMonth":104,"invoiceCntMonth":2,
	"invoiceCntPctMonth":0.25,"tradeAmtPctMonth":0.648,"redInvoiceAmtMonth":0,"redInvoiceCntMonth":0,
	"redTaxAmtMonth":0,"nullifiedInvoiceAmtMonth":0,"nullifiedInvoiceCntMonth":0,"nullTaxAmtMonth":0}]}`
}

// contractPorts 编排五个上游桩的返回值，用于构造不同寻源场景。
type contractPorts struct {
	invoicePort fakePort // 证通发票聚合（一个逻辑源的两次互补调用共用同一桩）
	taxPort     fakePort
	salesPort   fakePort
}

func okData(data string) fakePort {
	return fakePort{res: &model.UpstreamResult{Code: "001", Msg: "成功", UID: "ORD-1", LogID: "ORD-1", Range: data}}
}

func defaultContractPorts() contractPorts {
	return contractPorts{
		invoicePort: okData(entInvoiceData()),
		taxPort:     okData(entTaxData()),
		salesPort:   okData(salesData()),
	}
}

// buildContract 组一个「证通发票源(源1+源2) / 证通税务源(源3+源4) / 销项数据源(源5)」
// 的串行寻源器 + 契约层。源5 与证通发票源同供发票维度，优先级更低，故仅在证通发票源
// 未拿到数据时才会被调用（命中即停）。
func buildContract(t *testing.T, p contractPorts) *SwfpContract {
	t.Helper()
	s, err := NewSourcer([]Source{
		{Name: "ent_invoice", Provider: "entcredit", Provides: model.DimSet{Invoice: true}, Priority: 1,
			Calls: []Call{
				{Label: "invoice1", Dims: model.DimSet{Invoice: true}, Port: p.invoicePort},
				{Label: "invoice2", Dims: model.DimSet{Invoice: true}, Port: p.invoicePort},
			}},
		{Name: "ent_tax", Provider: "entcredit", Provides: model.DimSet{Tax: true}, Priority: 1,
			Calls: []Call{
				{Label: "tax1", Dims: model.DimSet{Tax: true}, Port: p.taxPort},
				{Label: "tax2", Dims: model.DimSet{Tax: true}, Port: p.taxPort},
			}},
		{Name: "sales", Provider: "salesdata", Provides: model.DimSet{Invoice: true}, Priority: 9, Optional: true,
			Calls: []Call{{Label: "sales", Dims: model.DimSet{Invoice: true}, Port: p.salesPort}}},
	}, 0)
	if err != nil {
		t.Fatalf("NewSourcer: %v", err)
	}
	return NewSwfpContract(s)
}

func queryContract(t *testing.T, c *SwfpContract, scope string) (*model.UpstreamResult, contractOut) {
	t.Helper()
	res, out, err := tryQueryContract(t, c, scope)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return res, out
}

func tryQueryContract(t *testing.T, c *SwfpContract, scope string) (*model.UpstreamResult, contractOut, error) {
	t.Helper()
	res, err := c.Query(context.Background(), &model.UpstreamRequest{
		CreditCode: testCreditCode, Scope: scope, Reqid: "r1", Want: model.AllDims(),
	})
	if err != nil {
		return res, contractOut{}, err
	}
	var out contractOut
	if res.Range != "" {
		if err := json.Unmarshal([]byte(res.Range), &out); err != nil {
			t.Fatalf("契约 range 不是合法 JSON: %v\n%s", err, res.Range)
		}
	}
	return res, out, nil
}

// TestSwfpContractAllOK 两项皆查得：xlsx 两段结构、按源分组、白名单过滤，
// 以及 dataScope/feeStandard 的对外明示。源5 与源1/源2 同供发票维度且优先级更低，
// 证通发票源已命中，故本例源5 为 skipped（命中即停省下的那笔钱）。
func TestSwfpContractAllOK(t *testing.T) {
	res, out := queryContract(t, buildContract(t, defaultContractPorts()), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	for _, s := range []string{"源1", "源2", "源3", "源4"} {
		if out.SourceStatus[s] != "ok" {
			t.Fatalf("sourceStatus[%s]=%q, want ok (all=%v)", s, out.SourceStatus[s], out.SourceStatus)
		}
	}
	if out.SourceStatus["源5"] != model.CallSkipped {
		t.Fatalf("证通发票源命中后源5 应为 skipped, got %q", out.SourceStatus["源5"])
	}
	if !out.DataScope["发票"] || !out.DataScope["税务"] || out.FeeStandard != string(model.FeeBoth) {
		t.Fatalf("两项皆得应为 dataScope 全真 + feeStandard=both, got %v/%q", out.DataScope, out.FeeStandard)
	}

	// 发票段 nsrjbxx：源1/源2 存在，xlsx 全字段补空（如 hybmdl 上游缺失 → ""）。
	var jb map[string]string
	if err := json.Unmarshal(out.Invoice["nsrjbxx"]["源1"], &jb); err != nil {
		t.Fatalf("nsrjbxx.源1: %v", err)
	}
	if jb["nsrmc"] != "某某公司" || jb["hybmdl"] != "" {
		t.Fatalf("nsrjbxx 白名单/补空不符: %v", jb)
	}
	if len(jb) != len(swfpNsrjbxxFields) {
		t.Fatalf("nsrjbxx 字段数=%d, want %d (xlsx 全字段)", len(jb), len(swfpNsrjbxxFields))
	}

	// kphzxxList：源1/源2 来自证通（yxhpje 被剔除），源5 来自销项映射。
	var kphz1 []map[string]string
	if err := json.Unmarshal(out.Invoice["kphzxxList"]["源1"], &kphz1); err != nil {
		t.Fatalf("kphzxxList.源1: %v", err)
	}
	if _, leaked := kphz1[0]["yxhpje"]; leaked {
		t.Fatalf("xlsx 外字段 yxhpje 泄漏: %v", kphz1[0])
	}
	if kphz1[0]["ljkpcs"] != "3" {
		t.Fatalf("kphzxxList.源1 数据不符: %v", kphz1[0])
	}

	if _, present := out.Invoice["kphzxxList"]["源5"]; present {
		t.Fatalf("源5 未被调用，不应出现在数据段里")
	}

	// 税务段：源3/源4；zsbxxList 数字转字符串、缺失字段补空。
	var zsb []map[string]string
	if err := json.Unmarshal(out.Tax["zsbxxList"]["源3"], &zsb); err != nil {
		t.Fatalf("zsbxxList.源3: %v", err)
	}
	if zsb[0]["sjje"] != "0" || zsb[0]["jkjzrq"] != "" {
		t.Fatalf("zsbxxList 转换不符: %v", zsb[0])
	}

	// summaryIndicators (xlsx 之外) 不得出现在任何地方。
	if strings.Contains(res.Range, "summaryIndicators") || strings.Contains(res.Range, "inputL1ySaleActualAmt") {
		t.Fatalf("xlsx 外内容 summaryIndicators 泄漏")
	}
}

// TestSwfpContractSalesFallback 证通发票源查无时，源5 作为发票维度的后备被调用，
// 其字段映射（日期归一、xlsx 契约名、缺失补空）在此校验。
func TestSwfpContractSalesFallback(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{res: &model.UpstreamResult{Code: "999", Msg: "查无", UID: "ORD-EMPTY", LogID: "ORD-EMPTY"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	if out.SourceStatus["源1"] != model.CallEmpty || out.SourceStatus["源5"] != model.CallOK {
		t.Fatalf("证通发票源查无应回落源5: %v", out.SourceStatus)
	}

	var kphz5 []map[string]string
	if err := json.Unmarshal(out.Invoice["kphzxxList"]["源5"], &kphz5); err != nil {
		t.Fatalf("kphzxxList.源5: %v", err)
	}
	row := kphz5[0]
	if row["ssyf"] != "2024-03" || row["kpqj"] != "2024-03-31" || row["zjybkpsj"] != "2024-03-28" {
		t.Fatalf("源5 日期归一不符: %v", row)
	}
	if row["kpje"] != "1234.56" || row["nsrsbh"] != testCreditCode || row["hpje"] != "-10" {
		t.Fatalf("源5 字段映射不符: %v", row)
	}

	// xyhzxxList.源5：购方映射 + 缺失字段 (gfsl) 补空。
	var xyhz5 []map[string]string
	if err := json.Unmarshal(out.Invoice["xyhzxxList"]["源5"], &xyhz5); err != nil {
		t.Fatalf("xyhzxxList.源5: %v", err)
	}
	if xyhz5[0]["gfnsrmc"] != "某购方" || xyhz5[0]["gfsl"] != "" || xyhz5[0]["kpjezb"] != "0.648" {
		t.Fatalf("xyhzxxList.源5 映射不符: %v", xyhz5[0])
	}
	if strings.Contains(res.Range, "summaryIndicators") {
		t.Fatalf("xlsx 外内容 summaryIndicators 泄漏")
	}
}

// TestSwfpContractScopeBasic scope=basic：可选源（源5）不调用，即便证通发票源查无
// 也不回落——basic 是"只用基础源"的口径。
func TestSwfpContractScopeBasic(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{res: &model.UpstreamResult{Code: "999", Msg: "查无", UID: "ORD-EMPTY", LogID: "ORD-EMPTY"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeBasic)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	if out.SourceStatus["源5"] != model.CallSkipped {
		t.Fatalf("scope=basic 源5 应为 skipped: %v", out.SourceStatus)
	}
	if _, present := out.Invoice["kphzxxList"]["源5"]; present {
		t.Fatalf("scope=basic 不应有源5 数据")
	}
	// 只拿到税务：按【单税务】计费。
	if out.FeeStandard != string(model.FeeTax) || out.DataScope["发票"] {
		t.Fatalf("仅查得税务应按 tax 档计费, got %q/%v", out.FeeStandard, out.DataScope)
	}
}

// TestSwfpContractPartialFailure 发票维度全部失败、税务查得：业务码仍是 001，但按
// 【单税务】计费，且失败详情不泄漏给下游。
func TestSwfpContractPartialFailure(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{err: &model.UpstreamError{Code: "0002", Msg: "请求超时"}}
	p.salesPort = fakePort{err: &model.UpstreamError{Code: "0002", Msg: "请求超时"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	if out.SourceStatus["源1"] != model.CallError || out.SourceStatus["源3"] != model.CallOK {
		t.Fatalf("sourceStatus 不符: %v", out.SourceStatus)
	}
	if out.FeeStandard != string(model.FeeTax) {
		t.Fatalf("仅查得税务应按 tax 档计费, got %q", out.FeeStandard)
	}
	if strings.Contains(res.Range, "请求超时") || strings.Contains(res.Range, "0002") {
		t.Fatalf("失败详情不应透出下游: %s", res.Range)
	}
	if _, present := out.Tax["zsbxxList"]["源3"]; !present {
		t.Fatalf("成功源数据缺失")
	}
}
