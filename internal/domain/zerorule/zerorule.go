// Package zerorule 判定一次查询的数据是否「近 12 个月金额全为 0」——对账口径里这类
// 数据不向下游计费，但寻源/计费链路并不过滤它，故在原始数据落库时单独算出来，
// 供出账/对账报表标注与计数。
//
// 各上游的段名、字段名都不一样，判定直接读各源的原始业务数据（解密/解包后、
// 契约映射前），按源取字段：
//
//	源                    发票：金额字段 (月份字段)            税务：金额字段 (月份字段)
//	证通发票 invoice1/2   nsrfpxx.kphzxxList[].kpje (ssyf)      -
//	证通税务 tax1/2       -                                    nsrswxx.sbsjList[].ysxssr/ynse (sssjz)
//	                                                           nsrswxx.zsbxxList[].sjje (sssjz)
//	凯盈云销项 sales      salesInvoice[].invoiceAmtMonth         -
//	                      (belongMonth, yyyyMM)
//	惠众税票C ctax        kphzxx[].kpje (ssyf)                  sbsj[].ysxssr/ynse (sssjz)、zsbxx[].sjje (sssjz)
//	税友 sy               kphzxxList[].kpje (kpyf, 含税)        sbxxList[].ysx/ynse (skssqz)、zsxxList[].ybtse (skssqz)
//
// 规则 v1：
//   - 窗口：所属月份落在 [请求月-12, 请求月]（北京时间）内的条目；月份缺失或无法
//     识别的条目不计入。
//   - 金额字段缺失、为空或无法解析按 0 计。
//   - 发票为 0：窗口内开票金额合计 <= 0。
//   - 税务为 0：窗口内每一项税务金额（应税收入、应纳税额、实缴金额）各自的合计
//     全部 <= 0。展示用的税务金额 = 各项合计中正数部分之和，故它 <= 0 与判定等价。
package zerorule

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// Version 是当前判定规则版本，随判定结果一起落库。
const Version = "v1"

var beijing = time.FixedZone("CST", 8*3600)

// Amounts 是一个源（或一次请求合并后）在窗口内的金额。Known=false 表示该源没有
// 已登记的取数规则（未知段名），金额无从判定。
type Amounts struct {
	Known   bool
	Invoice *big.Rat // 近 12 个月开票金额合计
	Tax     *big.Rat // 近 12 个月各项税务金额合计的正数部分之和
}

// Verdict 是落库的判定结果。各维度为 nil 表示该维度本次没查得（无从判定）。
type Verdict struct {
	InvAmt  *big.Rat
	TaxAmt  *big.Rat
	InvZero *bool
	TaxZero *bool
}

// field 描述一个列表段里的金额字段与月份字段（月份按顺序回落）。
type field struct {
	path   []string // 从 data 根到列表的路径
	amount []string // 同一类目的金额字段（各自独立求和）
	month  []string
}

var invoiceRules = map[string][]field{
	"invoice1": {{path: []string{"nsrfpxx", "kphzxxList"}, amount: []string{"kpje"}, month: []string{"ssyf", "kpqj"}}},
	"invoice2": {{path: []string{"nsrfpxx", "kphzxxList"}, amount: []string{"kpje"}, month: []string{"ssyf", "kpqj"}}},
	"sales":    {{path: []string{"salesInvoice"}, amount: []string{"invoiceAmtMonth"}, month: []string{"belongMonth"}}},
	"ctax":     {{path: []string{"kphzxx"}, amount: []string{"kpje"}, month: []string{"ssyf", "kpqj"}}},
	"sy":       {{path: []string{"kphzxxList"}, amount: []string{"kpje"}, month: []string{"kpyf", "kpqj"}}},
}

var taxRules = map[string][]field{
	"tax1": entTaxRules,
	"tax2": entTaxRules,
	"ctax": {
		{path: []string{"sbsj"}, amount: []string{"ysxssr", "ynse"}, month: []string{"sssjz", "sssjq", "sbrq"}},
		{path: []string{"zsbxx"}, amount: []string{"sjje"}, month: []string{"sssjz", "sssjq", "rkrq"}},
	},
	"sy": {
		{path: []string{"sbxxList"}, amount: []string{"ysx", "ynse"}, month: []string{"skssqz", "skssqq", "sbrq"}},
		{path: []string{"zsxxList"}, amount: []string{"ybtse"}, month: []string{"skssqz", "skssqq", "rkrq"}},
	},
}

var entTaxRules = []field{
	{path: []string{"nsrswxx", "sbsjList"}, amount: []string{"ysxssr", "ynse"}, month: []string{"sssjz", "sssjq", "sbrq"}},
	{path: []string{"nsrswxx", "zsbxxList"}, amount: []string{"sjje"}, month: []string{"sssjz", "sssjq", "rkrq"}},
}

// Extract 从一个源的原始业务数据里取窗口内金额。at 是请求时间（窗口基准）。
func Extract(label string, raw []byte, at time.Time) Amounts {
	inv, invOK := invoiceRules[label]
	tax, taxOK := taxRules[label]
	out := Amounts{Known: invOK || taxOK, Invoice: new(big.Rat), Tax: new(big.Rat)}
	if !out.Known || len(raw) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if err := dec.Decode(&data); err != nil {
		return out
	}
	from, to := window(at)
	for _, f := range inv {
		for _, s := range sums(data, f, from, to) {
			out.Invoice.Add(out.Invoice, s)
		}
	}
	for _, f := range tax {
		for _, s := range sums(data, f, from, to) {
			if s.Sign() > 0 {
				out.Tax.Add(out.Tax, s)
			}
		}
	}
	return out
}

// Judge 把一个源的金额按其实得维度转成判定结果。
func Judge(a Amounts, got model.DimSet) Verdict {
	var v Verdict
	if !a.Known {
		return v
	}
	if got.Invoice {
		v.InvAmt, v.InvZero = a.Invoice, boolPtr(a.Invoice.Sign() <= 0)
	}
	if got.Tax {
		v.TaxAmt, v.TaxZero = a.Tax, boolPtr(a.Tax.Sign() <= 0)
	}
	return v
}

// Source 是参与请求级合并的一个查得源。
type Source struct {
	Amounts Amounts
	Got     model.DimSet
}

// Merge 计算请求级判定：每个维度把本次提供了该维度的各个源的金额相加。任一提供
// 该维度的源没有取数规则时，该维度无从判定（nil），宁可交人工也不误判为 0。
func Merge(sources []Source) Verdict {
	var v Verdict
	inv, tax := new(big.Rat), new(big.Rat)
	var invN, taxN int
	invUnknown, taxUnknown := false, false
	for _, s := range sources {
		if s.Got.Invoice {
			invN++
			invUnknown = invUnknown || !s.Amounts.Known
			if s.Amounts.Known {
				inv.Add(inv, s.Amounts.Invoice)
			}
		}
		if s.Got.Tax {
			taxN++
			taxUnknown = taxUnknown || !s.Amounts.Known
			if s.Amounts.Known {
				tax.Add(tax, s.Amounts.Tax)
			}
		}
	}
	if invN > 0 && !invUnknown {
		v.InvAmt, v.InvZero = inv, boolPtr(inv.Sign() <= 0)
	}
	if taxN > 0 && !taxUnknown {
		v.TaxAmt, v.TaxZero = tax, boolPtr(tax.Sign() <= 0)
	}
	return v
}

// window 返回 [请求月-12, 请求月] 的 yyyymm 闭区间（北京时间）。
func window(at time.Time) (from, to int) {
	t := at.In(beijing)
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, beijing).AddDate(0, -12, 0)
	return start.Year()*100 + int(start.Month()), t.Year()*100 + int(t.Month())
}

// sums 对一个列表段里窗口内的条目，按金额字段分别求和。
func sums(data map[string]any, f field, from, to int) []*big.Rat {
	out := make([]*big.Rat, len(f.amount))
	for i := range out {
		out[i] = new(big.Rat)
	}
	items, _ := lookup(data, f.path).([]any)
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		ym := 0
		for _, k := range f.month {
			if ym = yearMonth(m[k]); ym != 0 {
				break
			}
		}
		if ym == 0 || ym < from || ym > to {
			continue
		}
		for i, k := range f.amount {
			if r, ok := number(m[k]); ok {
				out[i].Add(out[i], r)
			}
		}
	}
	return out
}

func lookup(data map[string]any, path []string) any {
	var cur any = data
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// yearMonth 识别 yyyy-MM / yyyyMM / yyyy-MM-dd / yyyyMMdd（字符串或数字），返回 yyyymm；
// 无法识别返回 0。
func yearMonth(v any) int {
	s := strings.TrimSpace(scalar(v))
	digits := make([]byte, 0, 8)
	for i := 0; i < len(s) && len(digits) < 6; i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		case c == '-' || c == '/' || c == '.':
		default:
			return 0
		}
	}
	if len(digits) < 6 {
		return 0
	}
	ym := 0
	for _, c := range digits {
		ym = ym*10 + int(c-'0')
	}
	if m := ym % 100; m < 1 || m > 12 {
		return 0
	}
	return ym
}

func number(v any) (*big.Rat, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(scalar(v)), ",", "")
	if s == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func boolPtr(b bool) *bool { return &b }
