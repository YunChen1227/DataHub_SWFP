// Package report 生成出账/对账 Excel：
//
//   - 下游账单：某客户（或全部客户）区间内的查得请求，按实得维度分票 / 税 / 票税，
//     并标出「近 12 个月金额全为 0」（不向下游计费）的条目。
//   - 上游对账：某上游公司区间内的全部实际调用，按该公司本次查得的维度分类，
//     另列查无/失败调用。
//
// 分类按实得维度 (audit_log.data_scope)，而不是 fee_standard：后者在主体年度计费
// 之后只保留「本次真正收费的类目」，免费期命中会变成 none 或单项，按它分类会把
// 查得记录漏出账单。历史行缺 data_scope 时回落 fee_standard。
package report

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/datahub/relay/internal/common/xlsx"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/port"
	"github.com/datahub/relay/internal/domain/zerorule"
)

// MaxRangeDays 是单次导出的最长区间（天）。
const MaxRangeDays = 366

// MaxRows 是单次导出的最大明细行数，超过请缩小区间。
const MaxRows = 300000

// ErrTooLarge：明细行数超过 MaxRows。
var ErrTooLarge = errors.New("导出数据量过大")

var beijing = time.FixedZone("CST", 8*3600)

// Vendor 是一家上游公司；Provider 对应 upstream_call.provider（配置里的 kind）。
type Vendor struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

var Vendors = []Vendor{
	{ID: "zhengtong", Name: "证通", Provider: "entcredit"},
	{ID: "kaiyingyun", Name: "凯盈云", Provider: "salesdata"},
	{ID: "huizhong", Name: "惠众", Provider: "ctax"},
	{ID: "shuiyou", Name: "税友", Provider: "sy"},
}

func VendorByID(id string) (Vendor, bool) {
	for _, v := range Vendors {
		if v.ID == id {
			return v, true
		}
	}
	return Vendor{}, false
}

func providerName(p string) string {
	for _, v := range Vendors {
		if v.Provider == p {
			return v.Name
		}
	}
	return p
}

// ParseRange 把北京时间日期 [from, to]（YYYY-MM-DD，含 to 当天）转成 [From, To)。
func ParseRange(from, to string) (time.Time, time.Time, error) {
	f, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(from), beijing)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("起始日期格式应为 YYYY-MM-DD")
	}
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(to), beijing)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("截止日期格式应为 YYYY-MM-DD")
	}
	if t.Before(f) {
		return time.Time{}, time.Time{}, fmt.Errorf("截止日期不能早于起始日期")
	}
	t = t.AddDate(0, 0, 1)
	if t.Sub(f) > MaxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("单次导出区间不能超过 %d 天", MaxRangeDays)
	}
	return f, t, nil
}

type Service struct {
	route string
	repo  port.ReportRepository
	now   func() time.Time
}

func New(route string, repo port.ReportRepository) *Service {
	return &Service{route: route, repo: repo, now: time.Now}
}

// 分类：票 / 税 / 票税；历史行无维度信息时为 unknown。
const (
	catInvoice = "invoice"
	catTax     = "tax"
	catBoth    = "both"
	catUnknown = "unknown"
)

var catLabel = map[string]string{catInvoice: "票", catTax: "税", catBoth: "票税", catUnknown: "维度未知"}

func categoryOf(dataScope, feeStandard string) string {
	switch dataScope {
	case "invoice+tax":
		return catBoth
	case "invoice":
		return catInvoice
	case "tax":
		return catTax
	}
	switch feeStandard {
	case string(model.FeeBoth):
		return catBoth
	case string(model.FeeInvoice):
		return catInvoice
	case string(model.FeeTax):
		return catTax
	}
	return catUnknown
}

func catOf(d model.DimSet) string {
	switch {
	case d.Both():
		return catBoth
	case d.Invoice:
		return catInvoice
	case d.Tax:
		return catTax
	}
	return ""
}

// judgement 是一条记录在其维度上的计费建议。
type judgement struct {
	verdict string // valid / zero / partial / nopayload / undecided
	advice  string
}

const (
	jValid     = "valid"
	jZero      = "zero"
	jInvZero   = "inv_zero" // 票税：仅发票为 0
	jTaxZero   = "tax_zero" // 票税：仅税务为 0
	jNoPayload = "nopayload"
	jUndecided = "undecided"
)

func judge(cat string, hasPayload bool, v model.ZeroVerdict) judgement {
	if !hasPayload {
		return judgement{jNoPayload, "待人工（历史无原始数据）"}
	}
	inv, tax := v.InvZero, v.TaxZero
	switch cat {
	case catInvoice:
		if inv == nil {
			return judgement{jUndecided, "待人工（无法判定）"}
		}
		if *inv {
			return judgement{jZero, "不计费（近12月发票金额为0）"}
		}
	case catTax:
		if tax == nil {
			return judgement{jUndecided, "待人工（无法判定）"}
		}
		if *tax {
			return judgement{jZero, "不计费（近12月税务金额为0）"}
		}
	case catBoth:
		if inv == nil || tax == nil {
			return judgement{jUndecided, "待人工（无法判定）"}
		}
		switch {
		case *inv && *tax:
			return judgement{jZero, "不计费（发票、税务近12月金额均为0）"}
		case *inv:
			return judgement{jInvZero, "建议按单税计（发票近12月金额为0）"}
		case *tax:
			return judgement{jTaxZero, "建议按单票计（税务近12月金额为0）"}
		}
	default:
		return judgement{jUndecided, "待人工（维度未知）"}
	}
	return judgement{jValid, "计费"}
}

// tally 是一个分类的汇总计数。
type tally struct {
	found, valid, zero, invZero, taxZero, noPayload, undecided, free int
}

func (t *tally) add(j judgement, free bool) {
	t.found++
	switch j.verdict {
	case jValid:
		t.valid++
	case jZero:
		t.zero++
	case jInvZero:
		t.invZero++
	case jTaxZero:
		t.taxZero++
	case jNoPayload:
		t.noPayload++
	default:
		t.undecided++
	}
	if free {
		t.free++
	}
}

// ---------------- 下游账单 ----------------

var downstreamHeaders = []string{"时间", "requestId", "reqid", "客户", "税号", "企业名称",
	"请求维度", "实得维度", "查得上游", "近12月开票金额", "发票为0", "近12月税务金额", "税务为0",
	"建议", "计费标识", "计费系统收费标准"}

var downstreamWidths = []float64{20, 34, 24, 24, 22, 28, 12, 12, 14, 16, 9, 16, 9, 34, 12, 16}

// Downstream 生成下游账单。customer 是展示用的客户名（空 = 全部客户）。
func (s *Service) Downstream(ctx context.Context, customer string, appKeys []string, from, to time.Time) (*xlsx.Workbook, error) {
	rows, err := s.repo.DownstreamRows(ctx, model.ReportFilter{Version: s.route, AppKeys: appKeys, From: from, To: to})
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxRows {
		return nil, fmt.Errorf("%w：明细 %d 行超过上限 %d，请缩小时间范围", ErrTooLarge, len(rows), MaxRows)
	}
	tallies := map[string]*tally{catInvoice: {}, catTax: {}, catBoth: {}, catUnknown: {}}
	details := map[string][][]any{}
	for _, r := range rows {
		cat := categoryOf(r.DataScope, r.FeeStandard)
		j := judge(cat, r.HasPayload, r.ZeroVerdict)
		tallies[cat].add(j, r.ChargeState == string(model.ChargeFree))
		details[cat] = append(details[cat], []any{
			fmtTime(r.CreatedAt), r.RequestID, r.Reqid, r.CustomerName, r.CreditCode, r.CompanyName,
			r.ReqScope, r.DataScope, providerNames(r.Providers),
			amount(r.InvAmt12m), yesNo(r.InvZero), amount(r.TaxAmt12m), yesNo(r.TaxZero),
			j.advice, chargeLabel(r.ChargeState), r.FeeStandard,
		})
	}

	if customer == "" {
		customer = "全部客户"
	}
	wb := &xlsx.Workbook{}
	sum := [][]any{
		{"客户", customer},
		{"区间（北京时间）", rangeLabel(from, to)},
		{"导出时间", fmtTime(s.now())},
		{"零数据规则", "近12个月（请求月及之前12个月）发票开票金额合计 <= 0 视为发票为0；各项税务金额合计全部 <= 0 视为税务为0；规则版本 " + zerorule.Version},
		{"分类口径", "按实际查得维度：请求票税但只查得票，计入「票」"},
		{},
	}
	hdr := map[int]bool{len(sum): true}
	sum = append(sum, []any{"类型", "查得次数", "建议计费次数", "零数据不计费", "票税-建议按单税计", "票税-建议按单票计",
		"历史无原始数据（待人工）", "无法判定（待人工）", "其中免费期命中（参考）"})
	for _, cat := range []string{catInvoice, catTax, catBoth} {
		t := tallies[cat]
		sum = append(sum, []any{catLabel[cat], t.found, t.valid, t.zero, t.invZero, t.taxZero, t.noPayload, t.undecided, t.free})
	}
	if t := tallies[catUnknown]; t.found > 0 {
		sum = append(sum, []any{"维度未知（历史数据）", t.found, 0, 0, 0, 0, t.noPayload, t.undecided, t.free})
	}
	sh := wb.Add("汇总", nil, sum, []float64{22, 14, 14, 14, 18, 18, 22, 18, 20})
	sh.HeaderRows = hdr

	wb.Add("票明细", downstreamHeaders, details[catInvoice], downstreamWidths)
	wb.Add("税明细", downstreamHeaders, details[catTax], downstreamWidths)
	wb.Add("票税明细", downstreamHeaders, details[catBoth], downstreamWidths)
	if len(details[catUnknown]) > 0 {
		wb.Add("维度未知明细", downstreamHeaders, details[catUnknown], downstreamWidths)
	}
	return wb, nil
}

// ---------------- 上游对账 ----------------

var upstreamHeaders = []string{"时间", "requestId", "reqid", "客户", "税号", "实得维度", "查得子源",
	"上游订单号", "上游请求号", "近12月开票金额", "发票为0", "近12月税务金额", "税务为0", "建议"}

var upstreamWidths = []float64{20, 34, 24, 24, 22, 12, 20, 30, 30, 16, 9, 16, 9, 34}

var failHeaders = []string{"时间", "requestId", "reqid", "客户", "税号", "子源", "调用维度", "结果",
	"上游code", "上游订单号", "上游请求号", "说明"}

// reqAgg 把一次请求里该公司的各子源调用合并成一条对账记录。
type reqAgg struct {
	first             *model.UpstreamCallRow
	got               model.DimSet
	labels, uids, lgs []string
	hasPayload        bool
	inv, tax          *big.Rat
	invUnknown        bool
	taxUnknown        bool
}

// Upstream 生成某上游公司的对账单。
func (s *Service) Upstream(ctx context.Context, v Vendor, from, to time.Time) (*xlsx.Workbook, error) {
	calls, err := s.repo.UpstreamCalls(ctx, model.ReportFilter{Version: s.route, Provider: v.Provider, From: from, To: to})
	if err != nil {
		return nil, err
	}
	if len(calls) > MaxRows {
		return nil, fmt.Errorf("%w：明细 %d 行超过上限 %d，请缩小时间范围", ErrTooLarge, len(calls), MaxRows)
	}

	type labelStat struct{ calls, ok, empty, fail int }
	byLabel := map[string]*labelStat{}
	labelSource := map[string]string{}
	var order []string
	aggs := map[string]*reqAgg{}
	var fails [][]any
	for _, c := range calls {
		st := byLabel[c.Label]
		if st == nil {
			st = &labelStat{}
			byLabel[c.Label] = st
			labelSource[c.Label] = c.SourceName
		}
		st.calls++
		switch c.Status {
		case model.CallOK:
			st.ok++
		case model.CallEmpty:
			st.empty++
		default:
			st.fail++
		}
		if c.Status != model.CallOK {
			fails = append(fails, []any{fmtTime(c.CreatedAt), c.RequestID, c.Reqid, c.CustomerName, c.CreditCode,
				c.Label, model.DimSet{Invoice: c.DimInvoice, Tax: c.DimTax}.String(), statusLabel(c.Status),
				c.Code, c.UID, c.LogID, c.Msg})
			continue
		}
		a := aggs[c.RequestID]
		if a == nil {
			a = &reqAgg{first: c, hasPayload: true, inv: new(big.Rat), tax: new(big.Rat)}
			aggs[c.RequestID] = a
			order = append(order, c.RequestID)
		}
		a.got = a.got.Union(model.DimSet{Invoice: c.DimInvoice, Tax: c.DimTax})
		a.labels = append(a.labels, c.Label)
		a.uids = appendNonEmpty(a.uids, c.UID)
		a.lgs = appendNonEmpty(a.lgs, c.LogID)
		a.hasPayload = a.hasPayload && c.HasPayload
		if c.DimInvoice {
			addAmount(a.inv, c.InvAmt12m, c.InvZero, &a.invUnknown)
		}
		if c.DimTax {
			addAmount(a.tax, c.TaxAmt12m, c.TaxZero, &a.taxUnknown)
		}
	}

	tallies := map[string]*tally{catInvoice: {}, catTax: {}, catBoth: {}}
	details := map[string][][]any{}
	for _, id := range order {
		a := aggs[id]
		cat := catOf(a.got)
		if cat == "" {
			continue
		}
		var zv model.ZeroVerdict
		if a.got.Invoice && !a.invUnknown {
			zv.InvAmt12m, zv.InvZero = ratText(a.inv), boolPtr(a.inv.Sign() <= 0)
		}
		if a.got.Tax && !a.taxUnknown {
			zv.TaxAmt12m, zv.TaxZero = ratText(a.tax), boolPtr(a.tax.Sign() <= 0)
		}
		j := judge(cat, a.hasPayload, zv)
		tallies[cat].add(j, false)
		c := a.first
		details[cat] = append(details[cat], []any{
			fmtTime(c.CreatedAt), c.RequestID, c.Reqid, c.CustomerName, c.CreditCode, a.got.String(),
			strings.Join(a.labels, ","), strings.Join(a.uids, ","), strings.Join(a.lgs, ","),
			amount(zv.InvAmt12m), yesNo(zv.InvZero), amount(zv.TaxAmt12m), yesNo(zv.TaxZero), j.advice,
		})
	}

	wb := &xlsx.Workbook{}
	sum := [][]any{
		{"上游", v.Name + "（" + v.Provider + "）"},
		{"区间（北京时间）", rangeLabel(from, to)},
		{"导出时间", fmtTime(s.now())},
		{"零数据规则", "按该上游本次返回的原始数据判定；规则同下游账单，版本 " + zerorule.Version},
		{"分类口径", "同一请求内该上游各子源查得维度合并后分票 / 税 / 票税；查无与失败的调用见「查无失败调用」"},
		{},
	}
	hdr := map[int]bool{len(sum): true}
	sum = append(sum, []any{"类型", "查得请求数", "数据有效", "零数据", "票税-仅发票为0", "票税-仅税务为0",
		"历史无原始数据（待人工）", "无法判定（待人工）"})
	for _, cat := range []string{catInvoice, catTax, catBoth} {
		t := tallies[cat]
		sum = append(sum, []any{catLabel[cat], t.found, t.valid, t.zero, t.invZero, t.taxZero, t.noPayload, t.undecided})
	}
	sum = append(sum, []any{})
	hdr[len(sum)] = true
	sum = append(sum, []any{"子源", "逻辑源", "调用次数", "查得", "查无", "失败"})
	labels := make([]string, 0, len(byLabel))
	for l := range byLabel {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	var tot labelStat
	for _, l := range labels {
		st := byLabel[l]
		sum = append(sum, []any{l, labelSource[l], st.calls, st.ok, st.empty, st.fail})
		tot.calls += st.calls
		tot.ok += st.ok
		tot.empty += st.empty
		tot.fail += st.fail
	}
	sum = append(sum, []any{"合计", "", tot.calls, tot.ok, tot.empty, tot.fail})
	sh := wb.Add("汇总", nil, sum, []float64{22, 16, 14, 12, 16, 16, 22, 18})
	sh.HeaderRows = hdr

	wb.Add("票明细", upstreamHeaders, details[catInvoice], upstreamWidths)
	wb.Add("税明细", upstreamHeaders, details[catTax], upstreamWidths)
	wb.Add("票税明细", upstreamHeaders, details[catBoth], upstreamWidths)
	wb.Add("查无失败调用", failHeaders, fails, []float64{20, 34, 24, 24, 22, 12, 12, 8, 14, 30, 30, 50})
	return wb, nil
}

// addAmount 累加一个子源的金额；任一子源无从判定，则整条该维度无从判定。
func addAmount(sum *big.Rat, amt *string, zero *bool, unknown *bool) {
	if amt == nil || zero == nil {
		*unknown = true
		return
	}
	if r, ok := new(big.Rat).SetString(*amt); ok {
		sum.Add(sum, r)
	} else {
		*unknown = true
	}
}

// ---------------- 格式化 ----------------

func fmtTime(t time.Time) string { return t.In(beijing).Format("2006-01-02 15:04:05") }

func rangeLabel(from, to time.Time) string {
	return from.In(beijing).Format("2006-01-02") + " 至 " + to.Add(-time.Second).In(beijing).Format("2006-01-02")
}

func amount(s *string) any {
	if s == nil {
		return ""
	}
	f, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		return *s
	}
	return f
}

func yesNo(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "是"
	default:
		return "否"
	}
}

func chargeLabel(s string) string {
	switch model.ChargeState(s) {
	case model.ChargeCharged:
		return "已计费"
	case model.ChargeFree:
		return "免费期"
	case model.ChargeNoCharge:
		return "不计费"
	case model.ChargeDeferred:
		return "待补记"
	}
	return s
}

func statusLabel(s string) string {
	switch s {
	case model.CallOK:
		return "查得"
	case model.CallEmpty:
		return "查无"
	case model.CallError:
		return "失败"
	}
	return s
}

func providerNames(csv string) string {
	if csv == "" {
		return ""
	}
	parts := strings.Split(csv, ",")
	for i, p := range parts {
		parts[i] = providerName(p)
	}
	return strings.Join(parts, ",")
}

func appendNonEmpty(xs []string, v string) []string {
	if v == "" {
		return xs
	}
	return append(xs, v)
}

func ratText(r *big.Rat) *string {
	s := r.FloatString(2)
	return &s
}

func boolPtr(b bool) *bool { return &b }
