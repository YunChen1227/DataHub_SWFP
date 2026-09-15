package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/datahub/relay/internal/domain/model"
)

const testCreditCode = "92500233MA60R5KW8M"

// contractOut mirrors the flattened 契约输出结构 for assertions.
type contractOut struct {
	Invoice   map[string]json.RawMessage `json:"发票数据聚合"`
	Tax       map[string]json.RawMessage `json:"税务数据聚合"`
	DataScope map[string]bool            `json:"dataScope"`
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

// ctaxData 构造一份源6 (税票数据查询C) 明细：发票段 + 税务段各一，其中利润表/
// 资产负债表带一层嵌套 data（表8/表10 的项目行），契约层需与父级拍平；
// yxhpje 是 xlsx 之外的字段，应被白名单剔除。
func ctaxData() string {
	return `{"nsrjbxx":{"nsrsbh":"` + testCreditCode + `","nsrmc":"某某公司","hybmdl":"651"},
	"kphzxx":[{"ssyf":"2026-05","kpqj":"2026-05-31","nsrsbh":"` + testCreditCode + `",
	  "ljkpcs":"12","kpje":"888.00","ljse":"115.44","yxhpje":"-10.00"}],
	"lrbxx":[{"nsrsbh":"` + testCreditCode + `","sbrq":"2026-04","sssjq":"2026-01-01","sssjz":"2026-03-31",
	  "data":[{"xmmc":"营业收入","sqje":"9000.00","bys":"3000.00","bnljje":"10000.00"},
	          {"xmmc":"营业成本","sqje":"6000.00","bys":"2000.00","bnljje":"7000.00"}]}],
	"zcfzbxx":[{"nsrsbh":"` + testCreditCode + `","sbrq":"2026-04","cwbblxdm":"101","zlbsxlmc":"资产负债表",
	  "data":[{"ewbxh":"1","zcxmmc":"货币资金","qmyezc":"5000.00","ncyezc":"4000.00"}]}]}`
}

// contractPorts 编排各上游桩的返回值，用于构造不同寻源场景。
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

// buildContract 组一个「证通发票源 / 证通税务源 / 销项数据源」的串行寻源器 + 契约层。
// 销项与证通发票源同供发票维度，优先级更低，故仅在证通发票源未拿到数据时才会被
// 调用（命中即停）。
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

func assertNoSourceLeak(t *testing.T, rangeJSON string) {
	t.Helper()
	for _, banned := range []string{"源1", "源2", "源3", "源4", "源5", "源6", "sourceStatus", "feeStandard"} {
		if strings.Contains(rangeJSON, banned) {
			t.Fatalf("下游契约泄漏 %q: %s", banned, rangeJSON)
		}
	}
}

func decodeObj(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode object: %v\n%s", err, raw)
	}
	return m
}

func decodeRows(t *testing.T, raw json.RawMessage) []map[string]string {
	t.Helper()
	var rows []map[string]string
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode rows: %v\n%s", err, raw)
	}
	return rows
}

// TestSwfpContractAllOK 两项皆查得：xlsx 两段扁平结构、白名单过滤，以及 dataScope。
// 命中即停（销项被短路）由 sourcing_test 覆盖，本层只断言下游看不到源信息。
func TestSwfpContractAllOK(t *testing.T) {
	res, out := queryContract(t, buildContract(t, defaultContractPorts()), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)
	if !out.DataScope["发票"] || !out.DataScope["税务"] {
		t.Fatalf("两项皆得应为 dataScope 全真, got %v", out.DataScope)
	}

	jb := decodeObj(t, out.Invoice["nsrjbxx"])
	if jb["nsrmc"] != "某某公司" || jb["hybmdl"] != "" {
		t.Fatalf("nsrjbxx 白名单/补空不符: %v", jb)
	}
	if len(jb) != len(swfpNsrjbxxFields) {
		t.Fatalf("nsrjbxx 字段数=%d, want %d (xlsx 全字段)", len(jb), len(swfpNsrjbxxFields))
	}

	kphz := decodeRows(t, out.Invoice["kphzxxList"])
	if len(kphz) == 0 {
		t.Fatalf("kphzxxList 应有数据")
	}
	if _, leaked := kphz[0]["yxhpje"]; leaked {
		t.Fatalf("xlsx 外字段 yxhpje 泄漏: %v", kphz[0])
	}
	if kphz[0]["ljkpcs"] != "3" {
		t.Fatalf("kphzxxList 数据不符: %v", kphz[0])
	}
	// 销项未被调用：不应出现销项映射后的月份 2024-03。
	for _, row := range kphz {
		if row["ssyf"] == "2024-03" {
			t.Fatalf("销项未被调用，不应出现其开票汇总: %v", row)
		}
	}

	zsb := decodeRows(t, out.Tax["zsbxxList"])
	if len(zsb) == 0 || zsb[0]["sjje"] != "0" || zsb[0]["jkjzrq"] != "" {
		t.Fatalf("zsbxxList 转换不符: %v", zsb)
	}

	if strings.Contains(res.Range, "summaryIndicators") || strings.Contains(res.Range, "inputL1ySaleActualAmt") {
		t.Fatalf("xlsx 外内容 summaryIndicators 泄漏")
	}
}

// TestSwfpContractSalesFallback 证通发票源查无时，销项作为发票维度的后备被调用，
// 其字段映射（日期归一、xlsx 契约名、缺失补空）在此校验。
func TestSwfpContractSalesFallback(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{res: &model.UpstreamResult{Code: "999", Msg: "查无", UID: "ORD-EMPTY", LogID: "ORD-EMPTY"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)

	kphz := decodeRows(t, out.Invoice["kphzxxList"])
	if len(kphz) == 0 {
		t.Fatalf("回落销项后 kphzxxList 应有数据")
	}
	row := kphz[0]
	if row["ssyf"] != "2024-03" || row["kpqj"] != "2024-03-31" || row["zjybkpsj"] != "2024-03-28" {
		t.Fatalf("销项日期归一不符: %v", row)
	}
	if row["kpje"] != "1234.56" || row["nsrsbh"] != testCreditCode || row["hpje"] != "-10" {
		t.Fatalf("销项字段映射不符: %v", row)
	}

	xyhz := decodeRows(t, out.Invoice["xyhzxxList"])
	if len(xyhz) == 0 || xyhz[0]["gfnsrmc"] != "某购方" || xyhz[0]["gfsl"] != "" || xyhz[0]["kpjezb"] != "0.648" {
		t.Fatalf("xyhzxxList 映射不符: %v", xyhz)
	}
	if strings.Contains(res.Range, "summaryIndicators") || strings.Contains(res.Range, "belongMonth") {
		t.Fatalf("xlsx 外内容或上游原字段名泄漏")
	}
}

// TestSwfpContractScopeBasic scope=basic：可选源（销项）不调用，即便证通发票源查无
// 也不回落——basic 是"只用基础源"的口径。
func TestSwfpContractScopeBasic(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{res: &model.UpstreamResult{Code: "999", Msg: "查无", UID: "ORD-EMPTY", LogID: "ORD-EMPTY"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeBasic)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)
	kphz := decodeRows(t, out.Invoice["kphzxxList"])
	for _, row := range kphz {
		if row["ssyf"] == "2024-03" {
			t.Fatalf("scope=basic 不应有销项数据: %v", row)
		}
	}
	if out.DataScope["发票"] || !out.DataScope["税务"] {
		t.Fatalf("仅查得税务时 dataScope 不符: %v", out.DataScope)
	}
}

// TestSwfpContractPartialFailure 发票维度全部失败、税务查得：业务码仍是 001，
// 失败详情不泄漏给下游，扁平税务数据仍在。
func TestSwfpContractPartialFailure(t *testing.T) {
	p := defaultContractPorts()
	p.invoicePort = fakePort{err: &model.UpstreamError{Code: "0002", Msg: "请求超时"}}
	p.salesPort = fakePort{err: &model.UpstreamError{Code: "0002", Msg: "请求超时"}}
	res, out := queryContract(t, buildContract(t, p), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)
	if out.DataScope["发票"] || !out.DataScope["税务"] {
		t.Fatalf("仅查得税务时 dataScope 不符: %v", out.DataScope)
	}
	if strings.Contains(res.Range, "请求超时") || strings.Contains(res.Range, "0002") {
		t.Fatalf("失败详情不应透出下游: %s", res.Range)
	}
	zsb := decodeRows(t, out.Tax["zsbxxList"])
	if len(zsb) == 0 {
		t.Fatalf("成功维度数据缺失")
	}
}

// buildCTaxContract 组一个「源6 综合源 + 证通两个逻辑源」的寻源器 + 契约层。
func buildCTaxContract(t *testing.T, ctaxPort fakePort) *SwfpContract {
	t.Helper()
	p := defaultContractPorts()
	s, err := NewSourcer([]Source{
		{Name: "ctax", Provider: "ctax", Provides: model.AllDims(), Priority: 1,
			Calls: []Call{{Label: "ctax", Dims: model.AllDims(), Port: ctaxPort}}},
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
	}, 0)
	if err != nil {
		t.Fatalf("NewSourcer: %v", err)
	}
	return NewSwfpContract(s)
}

// TestSwfpContractCTax 综合源一次拿全两维：数据按段写入扁平契约，嵌套利润表/
// 资产负债表与父级拍平成一维列表；下游看不到源编号。
func TestSwfpContractCTax(t *testing.T) {
	ctaxPort := fakePort{res: &model.UpstreamResult{
		Code: "001", Msg: "成功", Range: ctaxData(), Got: model.AllDims(),
	}}
	res, out := queryContract(t, buildCTaxContract(t, ctaxPort), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("want 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)
	if !out.DataScope["发票"] || !out.DataScope["税务"] {
		t.Fatalf("两项皆得 dataScope 不符: %v", out.DataScope)
	}

	kphz := decodeRows(t, out.Invoice["kphzxxList"])
	if len(kphz) == 0 || kphz[0]["kpje"] != "888.00" || kphz[0]["hpje"] != "" {
		t.Fatalf("kphzxxList 映射/补空不符: %v", kphz)
	}
	if _, leaked := kphz[0]["yxhpje"]; leaked {
		t.Fatalf("xlsx 外字段 yxhpje 泄漏: %v", kphz[0])
	}

	lrb := decodeRows(t, out.Tax["lrbxxList"])
	if len(lrb) != 2 {
		t.Fatalf("利润表应按项目行拍平成 2 条, got %d: %v", len(lrb), lrb)
	}
	if lrb[0]["xmmc"] != "营业收入" || lrb[0]["sssjz"] != "2026-03-31" || lrb[0]["bnljje"] != "10000.00" {
		t.Fatalf("利润表拍平后应同时含父级与项目行字段: %v", lrb[0])
	}
	if _, leaked := lrb[0]["data"]; leaked {
		t.Fatalf("嵌套 data 节点不应作为契约字段透出: %v", lrb[0])
	}

	zcfzb := decodeRows(t, out.Tax["zcfzbxxList"])
	if len(zcfzb) == 0 || zcfzb[0]["zcxmmc"] != "货币资金" || zcfzb[0]["cwbblxdm"] != "101" || zcfzb[0]["qmyeqy"] != "" {
		t.Fatalf("资产负债表拍平/补空不符: %v", zcfzb)
	}

	for name, raw := range map[string]json.RawMessage{"发票": out.Invoice["nsrjbxx"], "税务": out.Tax["nsrjbxx"]} {
		jb := decodeObj(t, raw)
		if jb["nsrmc"] != "某某公司" || jb["hybmdl"] != "651" {
			t.Fatalf("%s段 nsrjbxx 不符: %v", name, jb)
		}
	}
}

// TestSwfpContractCTaxTaxOnlyGap 综合源只拿回税务时：发票维度回落证通，综合源的
// 基本信息不落进发票段（发票段 nsrjbxx 来自证通，不含 hybmdl=651）。
func TestSwfpContractCTaxTaxOnlyGap(t *testing.T) {
	taxOnly := `{"nsrjbxx":{"nsrsbh":"` + testCreditCode + `","nsrmc":"某某公司","hybmdl":"651"},
	"sbsj":[{"nsrsbh":"` + testCreditCode + `","sbrq":"2026-04-15","sfzl":"增值税","ynse":"1300.00"}]}`
	ctaxPort := fakePort{res: &model.UpstreamResult{
		Code: "001", Msg: "成功", Range: taxOnly, Got: model.DimSet{Tax: true},
	}}
	res, out := queryContract(t, buildCTaxContract(t, ctaxPort), model.ScopeAll)
	if res.Code != "001" {
		t.Fatalf("补齐后应 001, got %s", res.Code)
	}
	assertNoSourceLeak(t, res.Range)
	if !out.DataScope["发票"] || !out.DataScope["税务"] {
		t.Fatalf("补齐后 dataScope 应两维皆得: %v", out.DataScope)
	}

	sbsj := decodeRows(t, out.Tax["sbsjList"])
	if len(sbsj) == 0 || sbsj[0]["sfzl"] != "增值税" || sbsj[0]["sssjq"] != "" {
		t.Fatalf("sbsjList 映射/补空不符: %v", sbsj)
	}
	invJB := decodeObj(t, out.Invoice["nsrjbxx"])
	if invJB["hybmdl"] == "651" {
		t.Fatalf("综合源未贡献发票段时，其 nsrjbxx 不应落进发票段: %v", invJB)
	}
	if invJB["nsrmc"] != "某某公司" {
		t.Fatalf("发票段 nsrjbxx 应来自证通回落: %v", invJB)
	}
	taxJB := decodeObj(t, out.Tax["nsrjbxx"])
	if taxJB["hybmdl"] != "651" {
		t.Fatalf("税务段 nsrjbxx 应来自综合源: %v", taxJB)
	}
}
