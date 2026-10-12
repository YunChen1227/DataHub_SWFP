package upstream

import "github.com/datahub/relay/internal/domain/model"

// 源7 (税友 sy，【税票数据查询-包年】接口文档) → xlsx 契约映射。
//
// 包年接口的段名、字段名与 xlsx 都不同，按「口径一致才映射」逐字段对照：
// 名称对得上但口径不同（含税/不含税、是否剔废、统计区间不同）的字段一律留空，
// 包年独有的字段（yxhpje、wsje、现金流指标等）不透出。代码类字段转成 xlsx 的
// 码值，转不了的留空。
//
// 纳税人识别号：各条目取 djxh（文档注明 ==纳税人识别号），缺失时依次回落
// data.nsrsbh、data.djxh、查询主体。

// qyxxList[0] → nsrjbxx（nsrsbh / nsrzt / zzsnsrlx 另行处理）。
// minKprq 按地区是「近 24 个月最早」或「历史最早」开票日期，与 qydyckpsj 口径
// 不一致，不映射。
var syNsrjbxxMap = map[string]string{
	"nsrmc":     "nsrmc",
	"hybmdl":    "dlhyDm",
	"hymcdl":    "dlhymc",
	"yqjydz":    "scjydz",
	"nsrxypddj": "nsrxydjId",
	"djzclxDm":  "djzclxDm",
	"djzclxmc":  "djzclxmc",
	"sjjyys":    "kprqMinMonth",
}

// nsrzt：包年只回中文状态（实际只有正常户）。
var syNsrztCodes = map[string]string{"正常": "03"}

// zzsnsrlx：zzsnsrlxId 为 1/2，nsrlx 文档写中文，两种写法都认。
var syZzsnsrlxCodes = map[string]string{
	"1": "2000010001", "一般纳税人": "2000010001",
	"2": "2000010002", "小规模纳税人": "2000010002",
}

// kphzxxList → kphzxxList。kpje/hpje/fpje 是含税口径，zddzje 不分蓝票/全部，
// lxwjyts 是「当月最晚开票日至月底」，kpsl 是开票数量而非次数，均不映射。
var syKphzMap = map[string]string{
	"kpqj":     "kpqj",
	"dykptslp": "dykptsLp",
	"dykptsqb": "dykptsQb",
	"hpsl":     "hpsl",
	"fpsl":     "fpsl",
	"zjybkpsj": "zjkpsj",
}

// syKphzxxList → syhzxxList。zcjeSum/wsjeSum 与 ljkpjebhs 的剔废口径未能确认，不映射。
var sySyhzMap = map[string]string{
	"kpqj":      "kpqj",
	"xfnsrsbh":  "xfDjxh",
	"xfmc":      "xfmc",
	"xfsl":      "xfCount",
	"syqyhydm":  "xfhyId",
	"ljkpcs":    "zcjeCnt",
	"kpcszb":    "zcjeCntRate",
	"kpjezb":    "zcjeSumRate",
	"kpjepmbhs": "zcjeSumRank",
}

// xyKphzxxList → xyhzxxList，口径同上游。
var syXyhzMap = map[string]string{
	"kpqj":      "kpqj",
	"gfnsrsbh":  "gfDjxh",
	"gfnsrmc":   "gfmc",
	"gfsl":      "gfCount",
	"xyqyhydm":  "gfhyId",
	"ljkpcs":    "zcjeCnt",
	"kpcszb":    "zcjeCntRate",
	"kpjezb":    "zcjeSumRate",
	"kpjepmbhs": "zcjeSumRank",
}

// khxsdqxxList → khxsdqList。kpjebhs 同样因剔废口径未确认不映射。
var syKhxsdqMap = map[string]string{
	"kpqj":        "kpqj",
	"gfdjsxzqydm": "gfdsswjgId",
	"gfdjssl":     "gfsCount",
	"jycs":        "zcjeCnt",
	"jycszb":      "zcjeCntRate",
	"jyjezb":      "zcjeSumRate",
	"jyjepm":      "zcjeSumRank",
}

// sbxxList → sbsjList。ysx 在增值税/企税下含义不同，jsyj 与应税销售收入口径
// 未能确认，qbxssr/ysxssr 不映射。
var sySbsjMap = map[string]string{
	"sbrq":  "sbrq",
	"sfzl":  "zsxmmc",
	"sssjq": "skssqq",
	"sssjz": "skssqz",
	"sbqx":  "sbqx",
	"jmse":  "jmse",
	"ynse":  "ynse",
	"yjse":  "yjse",
	"ybtse": "ybtse",
}

// zsxxList → zsbxxList。该段的 ybtse 文档明确为实缴金额。
var syZsbxxMap = map[string]string{
	"sssjq":  "skssqq",
	"sssjz":  "skssqz",
	"jkjzrq": "jkqx",
	"jkfsrq": "yzfsrq",
	"jkzt":   "skzt",
	"zsxm":   "zsxmmc",
	"skzl":   "skzlmc",
	"sl":     "sl1",
	"jsje":   "jsyj",
	"sjje":   "ybtse",
	"rkrq":   "rkrq",
}

// cwbbZbList 每期一条宽表，按指标拆成 xlsx 的项目行；现金流指标在 xlsx 里没有
// 对应列表，不透出。
type syIndicator struct{ key, name string }

var syLrbIndicators = []syIndicator{
	{"yysr", "营业收入"}, {"yycb", "营业成本"}, {"lrze", "利润总额"}, {"jlr", "净利润"},
	{"cwfy", "财务费用"}, {"glfy", "管理费用"}, {"xsfy", "销售费用"}, {"lxfy", "利息费用"},
	{"lxsr", "利息收入"}, {"yffy", "研发费用"}, {"zcjzss", "资产减值损失"},
	{"zyywsr", "主营业务收入"}, {"hzss", "坏账损失"},
}

var syZcIndicators = []syIndicator{
	{"hbzj", "货币资金"}, {"yszk", "应收账款"}, {"ch", "存货"}, {"gdzc", "固定资产"},
	{"wxzc", "无形资产"}, {"ldzchj", "流动资产合计"}, {"zchj", "资产合计"},
}

var syQyIndicators = []syIndicator{
	{"dqjk", "短期借款"}, {"yfzk", "应付账款"}, {"cqjk", "长期借款"}, {"yszky", "预收账款"},
	{"ldfzhj", "流动负债合计"}, {"fzhjq", "负债合计"}, {"sszbhgb", "实收资本（或股本）"},
	{"wfplr", "未分配利润"}, {"ybfxzb", "一般风险准备"}, {"syzqyhgdqyhj", "所有者权益（或股东权益）合计"},
}

// fillSySection 把源7 的明细分写进两个契约段。data 已由 syPrune 删掉未请求维度的
// 业务段，故 nsrjbxx 写进带回了业务段（段存在即可，sy 不做空段过滤）的维度；
// 两维的段都缺时按本次实得维度 got 写。
func fillSySection(invoice, tax map[string]map[string]any, alias string, data map[string]any, creditCode string, got model.DimSet) {
	nsrsbh := firstNonEmpty(str(data["nsrsbh"]), str(data["djxh"]), creditCode)

	invPresent := false
	for _, s := range []struct {
		section, list string
		mapping       map[string]string
	}{
		{"kphzxxList", "kphzxxList", syKphzMap},
		{"syKphzxxList", "syhzxxList", sySyhzMap},
		{"xyKphzxxList", "xyhzxxList", syXyhzMap},
		{"khxsdqxxList", "khxsdqList", syKhxsdqMap},
	} {
		items, ok := data[s.section].([]any)
		if !ok {
			continue
		}
		invoice[s.list][alias] = syRows(items, swfpInvoiceLists[s.list], s.mapping, nsrsbh, true)
		invPresent = true
	}

	taxPresent := false
	if items, ok := data["sbxxList"].([]any); ok {
		tax["sbsjList"][alias] = syRows(items, swfpTaxLists["sbsjList"], sySbsjMap, nsrsbh, false)
		taxPresent = true
	}
	if items, ok := data["zsxxList"].([]any); ok {
		tax["zsbxxList"][alias] = syRows(items, swfpTaxLists["zsbxxList"], syZsbxxMap, nsrsbh, false)
		taxPresent = true
	}
	if items, ok := data["cwbbZbList"].([]any); ok {
		lrb, zcfzb := syCwbbRows(items, nsrsbh)
		tax["lrbxxList"][alias] = lrb
		tax["zcfzbxxList"][alias] = zcfzb
		taxPresent = true
	}

	qyxx, _ := data["qyxxList"].([]any)
	if len(qyxx) == 0 {
		return
	}
	m, ok := qyxx[0].(map[string]any)
	if !ok {
		return
	}
	row := emptyRow(swfpNsrjbxxFields)
	for xlsxField, syField := range syNsrjbxxMap {
		if v, ok := m[syField]; ok {
			row[xlsxField] = str(v)
		}
	}
	row["nsrsbh"] = firstNonEmpty(str(m["nsrsbh"]), str(m["djxh"]), nsrsbh)
	row["nsrzt"] = syNsrztCodes[str(m["nsrztmc"])]
	row["zzsnsrlx"] = firstNonEmpty(syZzsnsrlxCodes[str(m["zzsnsrlxId"])], syZzsnsrlxCodes[str(m["nsrlx"])])

	toInvoice, toTax := invPresent, taxPresent
	if !toInvoice && !toTax {
		toInvoice, toTax = got.Invoice, got.Tax
	}
	if toInvoice {
		invoice["nsrjbxx"][alias] = row
	}
	if toTax {
		tax["nsrjbxx"][alias] = row
	}
}

// syRows 按映射表把一个业务段转成契约条目；monthly 段另把 kpyf (yyyyMM) 归一为 ssyf。
func syRows(items []any, fields []string, mapping map[string]string, nsrsbh string, monthly bool) []map[string]string {
	out := make([]map[string]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		row := emptyRow(fields)
		for xlsxField, syField := range mapping {
			if v, ok := m[syField]; ok {
				row[xlsxField] = str(v)
			}
		}
		row["nsrsbh"] = firstNonEmpty(str(m["djxh"]), nsrsbh)
		if monthly {
			row["ssyf"] = salesMonth(str(m["kpyf"]))
		}
		out = append(out, row)
	}
	return out
}

// syCwbbRows 把 cwbbZbList 的每期宽表拆成利润表 / 资产负债表项目行。上游未给
// （缺失或 null）的指标不出行。
func syCwbbRows(items []any, nsrsbh string) (lrb, zcfzb []map[string]string) {
	lrb = make([]map[string]string, 0)
	zcfzb = make([]map[string]string, 0)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		head := func(fields []string) map[string]string {
			row := emptyRow(fields)
			row["nsrsbh"] = nsrsbh
			row["sssjq"] = str(m["bbsqq"])
			row["sssjz"] = str(m["bbsqz"])
			return row
		}
		for _, ind := range syLrbIndicators {
			v, ok := m[ind.key]
			if !ok || v == nil {
				continue
			}
			row := head(swfpTaxLists["lrbxxList"])
			row["xmmc"] = ind.name
			row["bnljje"] = str(v)
			lrb = append(lrb, row)
		}
		for _, side := range []struct {
			indicators      []syIndicator
			nameKey, valKey string
		}{
			{syZcIndicators, "zcxmmc", "qmyezc"},
			{syQyIndicators, "fzhsyzqyxmmc", "qmyeqy"},
		} {
			for _, ind := range side.indicators {
				v, ok := m[ind.key]
				if !ok || v == nil {
					continue
				}
				row := head(swfpTaxLists["zcfzbxxList"])
				row["cwbblxdm"] = str(m["zcfzbZlbslxDm"])
				row[side.nameKey] = ind.name
				row[side.valKey] = str(v)
				zcfzb = append(zcfzb, row)
			}
		}
	}
	return lrb, zcfzb
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
