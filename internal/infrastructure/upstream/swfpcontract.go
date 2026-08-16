package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
)

// swfp 契约层 (docs/税票分析接口文档.xlsx)：swfp 路由是首个有「明确下游返回值
// 文档」的路由——不再把聚合器的原始分段结果透传下游，而是按 xlsx 定义的两段结构
// （发票数据聚合 / 税务数据聚合）整理输出，且**只输出 xlsx 内定义的字段**（严格
// 白名单；xlsx 定义但上游缺失的字段输出空串）。
//
// 输出结构（result.range 反序列化后）：
//
//	{
//	  "发票数据聚合": { "nsrjbxx": {"源1": {...}, "源2": {...}},
//	                    "kphzxxList": {"源1": [...], "源2": [...], "源5": [...]}, ... },
//	  "税务数据聚合": { "nsrjbxx": {"源3": {...}, "源4": {...}}, "lrbxxList": {...}, ... },
//	  "sourceStatus": { "源1": "ok", "源2": "ok", "源5": "skipped" },
//	  "dataScope":    { "发票": true, "税务": false },
//	  "feeStandard":  "invoice"
//	}
//
// 每个 xlsx 顶层字段的值按数据源分组（源1..源5，对下游隐匿真实上游），各源数据
// 不合并不去重，冲突由下游自行采信。sourceStatus 标记各源本次的状态
// (ok=查得 / empty=查无 / error=失败 / skipped=未调用——串行寻源命中即停，被更
// 高优先级源短路掉的源即为 skipped)。
//
// dataScope/feeStandard 是本次【实际查得的维度】与据此判定的收费标准：请求两项而
// 只查得发票时业务码仍为 001，但按【单发票】计费，下游凭这两个字段自查即可。
//
// 计费判定 (001/999/002) 仍由寻源器 (sourcing.go) 完成，本层只改写 range 内容，
// 不触碰上游调用与归一逻辑。
type SwfpContract struct {
	inner port.UpstreamPort
}

// NewSwfpContract wraps the swfp Aggregator with the xlsx 契约映射层。
func NewSwfpContract(inner port.UpstreamPort) *SwfpContract {
	return &SwfpContract{inner: inner}
}

// swfpSourceAlias 把聚合段名 label 映射为对下游脱敏的源编号。
var swfpSourceAlias = map[string]string{
	"invoice1": "源1", // 发票数据聚合-part1
	"invoice2": "源2", // 发票数据聚合-part2
	"tax1":     "源3", // 税务数据聚合-part1
	"tax2":     "源4", // 税务数据聚合-part2
	"sales":    "源5", // 销项数据（月度汇总，可选源）
}

// SourceAlias 返回段名对下游的脱敏编号；未登记的段名原样返回（兜底不丢数据）。
// 寻源器 (sourcing.go) 与本契约层共用这一份映射，避免两处漂移。
func SourceAlias(label string) string {
	if a := swfpSourceAlias[label]; a != "" {
		return a
	}
	return label
}

// ---- xlsx 字段白名单（docs/税票分析接口文档.xlsx，逐字段核对）----

// nsrjbxx 纳税人基本信息（两个 sheet 相同）。
var swfpNsrjbxxFields = []string{
	"nsrsbh", "nsrmc", "hybmdl", "hymcdl", "cybm", "cymc", "yqjydz",
	"szdjsswjgdm", "szdjsswjgmc", "qydyckpsj", "sjjyys", "kyrq", "nsrzt",
	"zzsnsrlx", "nsrxypdnd", "nsrxypddj", "djzclxDm", "djzclxmc",
	"hybmxl", "hymcxl", "zjyckprq", "dqwkpts",
}

// sheet1 发票数据聚合的各列表 item 字段。
var swfpInvoiceLists = map[string][]string{
	"syhzxxList": {"ssyf", "nsrsbh", "kpqj", "xfnsrsbh", "xfmc", "xfsl",
		"ljkpcs", "ljkpjebhs", "ljse", "kpcszb", "kpjezb", "kpjepmbhs",
		"syqyhydm", "syqyhymc", "hpjebhs", "hpsl", "hpse", "fpjebhs", "fpsl", "fpse"},
	"xyhzxxList": {"ssyf", "nsrsbh", "kpqj", "gfnsrsbh", "gfnsrmc", "gfsl",
		"ljkpcs", "ljkpjebhs", "ljse", "kpcszb", "kpjezb", "kpjepmbhs",
		"xyqyhydm", "xyqyhymc", "hpjebhs", "hpsl", "hpse", "fpjebhs", "fpsl", "fpse"},
	"spxxList": {"ssyf", "nsrsbh", "hwhlwbm", "hwhlwmc", "sl", "spzsl",
		"spzje", "spzse", "gxfxdhwhlwbmzls", "sphlwzslzb", "sphlwzjezb", "jyjezbpm"},
	"khxsdqList": {"ssyf", "nsrsbh", "kpqj", "gfdjssl", "jycs", "jycszb",
		"kpjebhs", "jyjezb", "jyjepm", "gfdjssjjgdm", "gfdjsxzqydm", "gfdjsxzqymc", "ljse"},
	"kphzxxList": {"ssyf", "kpqj", "nsrsbh", "ljkpcs", "kpje", "ljse",
		"hpsl", "hpje", "hpse", "fpsl", "fpje", "fpse",
		"dzzgkpjejlp", "dzzgkpjehhfp", "dykptsqb", "dykptslp", "zjybkpsj", "dqlxwjyjlts"},
}

// sheet2 税务数据聚合的各列表 item 字段。
var swfpTaxLists = map[string][]string{
	"lrbxxList":   {"nsrsbh", "sbrq", "sssjq", "sssjz", "xmmc", "bnljje", "bys", "sqje", "mc", "zlbsxlmc"},
	"sbsjList":    {"nsrsbh", "sbrq", "sfzl", "sssjq", "sssjz", "qbxssr", "ysxssr", "ynse", "yjse", "ybtse", "jmse", "sbqx"},
	"zcfzbxxList": {"nsrsbh", "sbrq", "sssjq", "sssjz", "cwbblxdm", "zlbsxlmc", "ewbxh", "zcxmmc", "qmyezc", "ncyezc", "fzhsyzqyxmmc", "qmyeqy", "ncyeqy"},
	"zsbxxList":   {"nsrsbh", "sssjq", "sssjz", "jkjzrq", "jkfsrq", "jkzt", "zsxm", "skzl", "jsje", "sl", "sjje", "rkrq"},
}

// 源5 (销项数据) → xlsx 字段名映射。语义口径与 xlsx 定义不完全一致的字段照样映射，
// 差异在对外手册中按源标注（如源5 金额为不含税/剔废口径），见手册 3.1.5。
//
// salesInvoice (月度开票汇总) → kphzxxList。
var swfpSalesKphzMap = map[string]string{
	"ljkpcs":       "invoiceCntMonth",          // 累计开票次数（源5：剔废、红为负）
	"kpje":         "invoiceAmtMonth",          // 累计开票金额（源5：不含税、剔废）
	"ljse":         "taxAmtMonth",              // 累计税额（源5：剔废、红为负）
	"hpsl":         "redInvoiceCntMonth",       // 红票数量
	"hpje":         "redInvoiceAmtMonth",       // 红票金额（源5：不含税）
	"hpse":         "redTaxAmtMonth",           // 红票税额
	"fpsl":         "nullifiedInvoiceCntMonth", // 废票数量
	"fpje":         "nullifiedInvoiceAmtMonth", // 废票金额（源5：不含税）
	"fpse":         "nullTaxAmtMonth",          // 废票税额
	"dzzgkpjejlp":  "invoiceHighAmtMonth",      // 单张最高开票金额（仅蓝票）
	"dzzgkpjehhfp": "allInvoiceHighAmtMonth",   // 单张最高开票金额（含红废票）
	"dykptsqb":     "invoiceDayMonth",          // 当月开票天数（含红废票）
	"dykptslp":     "blueInvoiceDayMonth",      // 当月开票天数（仅蓝票）
	"dqlxwjyjlts":  "noTradeRcordDay",          // 当前连续无交易记录天数
}

// monthlyDownstreamInfo (月度下游企业, 购方 Top3) → xyhzxxList。
var swfpSalesXyhzMap = map[string]string{
	"gfnsrsbh":  "buyerTaxpayerIdNum",
	"gfnsrmc":   "buyerName",
	"ljkpcs":    "invoiceCntMonth",
	"ljkpjebhs": "tradeAmtMonth",       // 累计开票金额（不含税）
	"ljse":      "taxAmtMonth",         // 累计税额
	"kpcszb":    "invoiceCntPctMonth",  // 开票次数占比（源5：3 位小数）
	"kpjezb":    "tradeAmtPctMonth",    // 开票金额占比（源5：3 位小数）
	"kpjepmbhs": "tradeAmtRankMonth",   // 开票金额排名（源5：仅 Top3）
	"hpjebhs":   "redInvoiceAmtMonth",
	"hpsl":      "redInvoiceCntMonth",
	"hpse":      "redTaxAmtMonth",
	"fpjebhs":   "nullifiedInvoiceAmtMonth",
	"fpsl":      "nullifiedInvoiceCntMonth",
	"fpse":      "nullTaxAmtMonth",
}

// salesFieldAliases 收录源5 文档 (销项数据接口文档V1.0.docx) 自相矛盾的字段拼写：
// 报文示例与字段表对同一字段给了两种写法，上游实际用哪种未经联调确认，两种都认。
// key = 报文示例的写法（映射表里用的），value = 字段表的写法。
var salesFieldAliases = map[string]string{
	"invoiceDayMonth":    "invoceDayMonth",     // §4.1 字段表少一个 i
	"buyerTaxpayerIdNum": "buyerTaxPayerIdNum", // §4.2 字段表 P 大写
}

// salesValue 取源5 条目的字段值，字段缺失时再试文档给出的另一种拼写。
func salesValue(m map[string]any, field string) (any, bool) {
	if v, ok := m[field]; ok {
		return v, true
	}
	if alt, ok := salesFieldAliases[field]; ok {
		if v, ok := m[alt]; ok {
			return v, true
		}
	}
	return nil, false
}

// Query delegates to the aggregator then rewrites range into the xlsx 契约结构。
func (c *SwfpContract) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	res, err := c.inner.Query(ctx, req)
	if err != nil || res == nil {
		return res, err
	}
	// 只有携带分段结果的 001/002 需要改写；999 查无 range 恒空，原样透出。
	if (res.Code != "001" && res.Code != "002") || res.Range == "" {
		return res, nil
	}
	mapped, mErr := mapSwfpRange(res.Range, req.CreditCode, res.Got)
	if mErr != nil {
		// 契约映射失败属我方内部错误：不改判定结论，保底透出原始分段（好过丢数据），
		// 并在错误里留痕（该情况仅在聚合器输出结构异常时出现）。
		return res, fmt.Errorf("swfp 契约映射失败: %w", mErr)
	}
	out := *res
	out.Range = mapped
	return &out, nil
}

// Requery passes through (聚合器多源复查逻辑不变)。
func (c *SwfpContract) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	return c.inner.Requery(ctx, reqid)
}

// mapSwfpRange 把寻源器的分段 JSON ({label:{status,data,error}}) 改写为 xlsx 契约
// 结构。creditCode 用于补齐源5 条目里的 nsrsbh (纳税人识别号 = 查询主体)；got 是
// 本次实际查得的维度，据此向下游明示 dataScope 与 feeStandard（部分查得时业务码
// 仍是 001，客户凭这两个字段自查本次按哪档标准收费）。
func mapSwfpRange(rangeJSON, creditCode string, got model.DimSet) (string, error) {
	var sections map[string]aggSection
	dec := json.NewDecoder(bytes.NewReader([]byte(rangeJSON)))
	if err := dec.Decode(&sections); err != nil {
		return "", fmt.Errorf("解析聚合分段: %w", err)
	}

	invoice := newSwfpSegment(append([]string{"nsrjbxx"}, keysOf(swfpInvoiceLists)...))
	tax := newSwfpSegment(append([]string{"nsrjbxx"}, keysOf(swfpTaxLists)...))
	status := map[string]string{}

	for label, sec := range sections {
		alias := SourceAlias(label)
		status[alias] = sec.Status
		if sec.Status != model.CallOK || len(sec.Data) == 0 {
			continue
		}
		data, err := decodeNumberMap(sec.Data)
		if err != nil {
			return "", fmt.Errorf("解析 %s 段明细: %w", label, err)
		}
		switch label {
		case "invoice1", "invoice2":
			fillEntSection(invoice, alias, data, "nsrfpxx", swfpInvoiceLists)
		case "tax1", "tax2":
			fillEntSection(tax, alias, data, "nsrswxx", swfpTaxLists)
		case "sales":
			fillSalesSection(invoice, alias, data, creditCode)
		default:
			// 未知段：无契约映射依据，只标状态不透数据（严格白名单）。
		}
	}

	out := map[string]any{
		"发票数据聚合": invoice,
		"税务数据聚合": tax,
		"sourceStatus": status,
		"dataScope": map[string]bool{
			"发票": got.Invoice,
			"税务": got.Tax,
		},
		"feeStandard": string(model.StandardOf(got)),
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// newSwfpSegment 预置一个契约段：每个 xlsx 顶层字段恒存在（值为 源N→数据 的分组
// 对象，无源贡献时为空对象），保证下游拿到确定形状。
func newSwfpSegment(fields []string) map[string]map[string]any {
	seg := make(map[string]map[string]any, len(fields))
	for _, f := range fields {
		seg[f] = map[string]any{}
	}
	return seg
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fillEntSection 把一个证通子源 (发票/税务聚合) 的明细写进契约段：nsrjbxx 对象 +
// wrapper (nsrfpxx/nsrswxx) 下的各列表，全部按 xlsx 白名单过滤、缺失字段补空串。
func fillEntSection(seg map[string]map[string]any, alias string, data map[string]any, wrapper string, lists map[string][]string) {
	if jb, ok := data["nsrjbxx"].(map[string]any); ok {
		seg["nsrjbxx"][alias] = pickFields(jb, swfpNsrjbxxFields)
	}
	inner, _ := data[wrapper].(map[string]any)
	for listName, fields := range lists {
		items, ok := inner[listName].([]any)
		if !ok {
			continue
		}
		outItems := make([]map[string]string, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			outItems = append(outItems, pickFields(m, fields))
		}
		seg[listName][alias] = outItems
	}
}

// fillSalesSection 把源5 (销项数据月度汇总) 映射进发票数据聚合段：
// salesInvoice → kphzxxList、monthlyDownstreamInfo → xyhzxxList。字段名转为 xlsx
// 契约名，日期/月份统一为 xlsx 格式；xlsx 定义而源5 未提供的字段输出空串。
// summaryIndicators 等 xlsx 之外的内容一律不透出。
func fillSalesSection(seg map[string]map[string]any, alias string, data map[string]any, creditCode string) {
	if items, ok := data["salesInvoice"].([]any); ok {
		out := make([]map[string]string, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			row := emptyRow(swfpInvoiceLists["kphzxxList"])
			month := salesMonth(str(m["belongMonth"]))
			row["ssyf"] = month
			row["kpqj"] = monthLastDay(month)
			row["nsrsbh"] = creditCode
			for xlsxField, salesField := range swfpSalesKphzMap {
				if v, ok := salesValue(m, salesField); ok {
					row[xlsxField] = str(v)
				}
			}
			row["zjybkpsj"] = salesDate(str(m["latestInvoiceDate"]))
			out = append(out, row)
		}
		seg["kphzxxList"][alias] = out
	}
	if items, ok := data["monthlyDownstreamInfo"].([]any); ok {
		out := make([]map[string]string, 0, len(items))
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			row := emptyRow(swfpInvoiceLists["xyhzxxList"])
			month := salesMonth(str(m["belongMonth"]))
			row["ssyf"] = month
			row["kpqj"] = monthLastDay(month)
			row["nsrsbh"] = creditCode
			for xlsxField, salesField := range swfpSalesXyhzMap {
				if v, ok := salesValue(m, salesField); ok {
					row[xlsxField] = str(v)
				}
			}
			out = append(out, row)
		}
		seg["xyhzxxList"][alias] = out
	}
}

// pickFields 按白名单抽取字段并全部转为字符串；缺失字段输出空串（Q6 口径）。
func pickFields(m map[string]any, fields []string) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		if v, ok := m[f]; ok {
			out[f] = str(v)
		} else {
			out[f] = ""
		}
	}
	return out
}

// emptyRow 预置一条全空的契约条目（xlsx 全字段，值空串）。
func emptyRow(fields []string) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f] = ""
	}
	return out
}

// decodeNumberMap 用 json.Number 解析对象（金额最长 22 位整数，不能走 float64）。
func decodeNumberMap(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// str 把任意 JSON 标量转成字符串（对齐 xlsx 全 string 类型）；nil → 空串。
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// salesMonth 把源5 的所属月份 (yyyyMM) 归一为 xlsx 的 yyyy-MM；已带连字符或无法
// 识别时原样返回。
func salesMonth(s string) string {
	if len(s) == 6 {
		if _, err := time.Parse("200601", s); err == nil {
			return s[:4] + "-" + s[4:]
		}
	}
	return s
}

// salesDate 把源5 的日期 (yyyyMMdd) 归一为 xlsx 的 yyyy-MM-dd；无法识别时原样返回。
func salesDate(s string) string {
	if len(s) == 8 {
		if _, err := time.Parse("20060102", s); err == nil {
			return s[:4] + "-" + s[4:6] + "-" + s[6:]
		}
	}
	return s
}

// monthLastDay 计算 yyyy-MM 的当月最后一天 (yyyy-MM-dd，xlsx kpqj 口径，闰年安全)。
func monthLastDay(month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 1, -1).Format("2006-01-02")
}
