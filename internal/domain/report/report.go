// Package report 生成出账/对账 Excel：
//
//   - 下游账单：按「客户 + 税号」聚合。同一税号无论查几次只计 1 个；近 12 个月金额
//     为 0 不计费；以主体年度免费期 (billing_coverage) 作为「本月不收费」的佐证。
//   - 上游对账：按「上游公司 + 税号 + 类目」推算一年窗口（不分下游客户），第一次有
//     金额的 ok 调用计费并开窗，窗口内不计费。
//
// 汇总恒等式：查得税号 = 金额为0 + 本月收费 + 免费期内 + 待人工（无原始数据）。
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

// 分类：票 / 税 / 票税。
const (
	catInvoice = "invoice"
	catTax     = "tax"
	catBoth    = "both"
)

var catLabel = map[string]string{catInvoice: "票", catTax: "税", catBoth: "票税"}

func categoryOf(dataScope, feeStandard, reqScope string) string {
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
	switch reqScope {
	case "invoice+tax":
		return catBoth
	case "invoice":
		return catInvoice
	case "tax":
		return catTax
	}
	return ""
}

// 税号结论。
const (
	verdictBillable = "billable" // 本月收费
	verdictZero     = "zero"     // 查得但金额为 0
	verdictFree     = "free"     // 查得有金额，命中免费期
	verdictMiss     = "miss"     // 未查得
	verdictManual   = "manual"   // 查得但无原始数据，待人工
)

var verdictLabel = map[string]string{
	verdictBillable: "本月收费",
	verdictZero:     "查得不计费（金额为0）",
	verdictFree:     "免费期内（前期已收费）",
	verdictMiss:     "未查得",
	verdictManual:   "待人工（无原始数据）",
}

// ---------------- 下游账单 ----------------

// subjectAgg 是「客户 + 税号」的聚合结果。
type subjectAgg struct {
	customer, credit, company string
	reqScope, dataScope       string
	queries                   int
	times                     []time.Time
	requestIDs                []string
	found, hasPayload         bool
	hasAmount, allZero        bool // 仅在 found 时有意义
	billable                  bool
	invAmt, taxAmt            *string
	invZero, taxZero          *bool
	chargedCat                string // 本月收费类目
	providers                 string
	invCov, taxCov            model.CoverageProof
	reqs                      []*model.DownstreamRow
}

func (a *subjectAgg) verdict() string {
	switch {
	case !a.found:
		return verdictMiss
	case !a.hasPayload:
		return verdictManual
	case a.billable:
		return verdictBillable
	case a.allZero:
		return verdictZero
	default:
		return verdictFree
	}
}

func (a *subjectAgg) add(r *model.DownstreamRow) {
	a.queries++
	a.times = append(a.times, r.CreatedAt)
	a.requestIDs = append(a.requestIDs, r.RequestID)
	a.reqs = append(a.reqs, r)
	if a.company == "" {
		a.company = r.CompanyName
	}
	if a.reqScope == "" {
		a.reqScope = r.ReqScope
	}
	if r.FoundData {
		a.found = true
		if a.dataScope == "" {
			a.dataScope = r.DataScope
		} else if r.DataScope != "" && r.DataScope != a.dataScope {
			// 多次查得维度不同时取并集展示
			a.dataScope = mergeScope(a.dataScope, r.DataScope)
		}
		if r.HasPayload {
			a.hasPayload = true
			mergeAmounts(a, r)
		}
		if chargedNonZero(r) {
			a.billable = true
			a.chargedCat = mergeScope(a.chargedCat, r.ChargedScope)
		}
	}
	if r.Providers != "" {
		a.providers = mergeCSV(a.providers, r.Providers)
	}
	if r.InvCoverage.ChargeRequestID != "" {
		a.invCov = r.InvCoverage
	}
	if r.TaxCoverage.ChargeRequestID != "" {
		a.taxCov = r.TaxCoverage
	}
}

func mergeScope(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	return model.DimSet{
		Invoice: strings.Contains(a, "invoice") || strings.Contains(b, "invoice"),
		Tax:     strings.Contains(a, "tax") || strings.Contains(b, "tax"),
	}.String()
}

func mergeCSV(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, s := range strings.Split(a+","+b, ",") {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func mergeAmounts(a *subjectAgg, r *model.DownstreamRow) {
	// 只要有一次有金额就算有金额；只有全部查得结果都为 0 才算金额为 0。
	gotInv := strings.Contains(r.DataScope, "invoice")
	gotTax := strings.Contains(r.DataScope, "tax")
	if gotInv && r.InvZero != nil {
		if !*r.InvZero {
			a.hasAmount = true
			a.invZero = boolPtr(false)
		} else if a.invZero == nil {
			a.invZero = boolPtr(true)
		}
		if r.InvAmt12m != nil {
			a.invAmt = pickAmt(a.invAmt, r.InvAmt12m)
		}
	}
	if gotTax && r.TaxZero != nil {
		if !*r.TaxZero {
			a.hasAmount = true
			a.taxZero = boolPtr(false)
		} else if a.taxZero == nil {
			a.taxZero = boolPtr(true)
		}
		if r.TaxAmt12m != nil {
			a.taxAmt = pickAmt(a.taxAmt, r.TaxAmt12m)
		}
	}
	// 无从判定（nil）按有金额处理，避免误判为零。
	if (gotInv && r.InvZero == nil) || (gotTax && r.TaxZero == nil) {
		a.hasAmount = true
	}
	a.allZero = a.found && a.hasPayload && !a.hasAmount
}

func pickAmt(cur, neu *string) *string {
	if cur == nil {
		return neu
	}
	c, ok1 := new(big.Rat).SetString(*cur)
	n, ok2 := new(big.Rat).SetString(*neu)
	if !ok1 {
		return neu
	}
	if !ok2 {
		return cur
	}
	if n.Cmp(c) > 0 {
		return neu
	}
	return cur
}

// chargedNonZero：本月 CHARGED 且收费类目金额大于 0（无 payload 不算收费）。
func chargedNonZero(r *model.DownstreamRow) bool {
	if r.ChargeState != string(model.ChargeCharged) || !r.HasPayload {
		return false
	}
	scope := r.ChargedScope
	if scope == "" || scope == "none" {
		return false
	}
	wantInv := strings.Contains(scope, "invoice")
	wantTax := strings.Contains(scope, "tax")
	if wantInv {
		if r.InvZero == nil || !*r.InvZero {
			return true
		}
	}
	if wantTax {
		if r.TaxZero == nil || !*r.TaxZero {
			return true
		}
	}
	return false
}

var subjectHeaders = []string{
	"税号", "企业名称", "客户", "请求维度", "实得维度", "请求次数",
	"请求时间", "首次请求", "末次请求", "查得情况",
	"近12月开票金额", "发票为0", "近12月税务金额", "税务为0",
	"结论", "本月收费类目", "查得上游",
	"发票计费时间", "发票免费期至", "发票计费requestId",
	"税务计费时间", "税务免费期至", "税务计费requestId",
}

var subjectWidths = []float64{
	22, 28, 24, 12, 12, 10,
	40, 20, 20, 10,
	16, 10, 16, 10,
	22, 12, 14,
	20, 20, 34,
	20, 20, 34,
}

func subjectRow(a *subjectAgg) []any {
	sort.Slice(a.times, func(i, j int) bool { return a.times[i].Before(a.times[j]) })
	times := make([]string, len(a.times))
	for i, t := range a.times {
		times[i] = fmtTime(t)
	}
	found := "未查得"
	if a.found {
		found = "查得"
	}
	v := a.verdict()
	return []any{
		a.credit, a.company, a.customer, a.reqScope, a.dataScope, a.queries,
		strings.Join(times, "；"), firstLast(a.times, true), firstLast(a.times, false), found,
		amount(a.invAmt), yesNo(a.invZero), amount(a.taxAmt), yesNo(a.taxZero),
		verdictLabel[v], a.chargedCat, providerNames(a.providers),
		proofTime(a.invCov.ChargedAt), proofTime(a.invCov.ExpiresAt), a.invCov.ChargeRequestID,
		proofTime(a.taxCov.ChargedAt), proofTime(a.taxCov.ExpiresAt), a.taxCov.ChargeRequestID,
	}
}

func firstLast(ts []time.Time, first bool) string {
	if len(ts) == 0 {
		return ""
	}
	if first {
		return fmtTime(ts[0])
	}
	return fmtTime(ts[len(ts)-1])
}

func proofTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtTime(t)
}

var requestHeaders = []string{
	"税号", "企业名称", "客户", "时间", "requestId", "reqid",
	"请求维度", "实得维度", "查得", "近12月开票金额", "发票为0", "近12月税务金额", "税务为0",
	"计费标识", "收费类目", "计费系统收费标准", "查得上游",
}

var requestWidths = []float64{
	22, 28, 24, 20, 34, 24,
	12, 12, 8, 16, 10, 16, 10,
	12, 12, 16, 14,
}

// Downstream 生成下游账单。customer 是展示用的客户名（空 = 全部客户）。
func (s *Service) Downstream(ctx context.Context, customer string, appKeys []string, from, to time.Time) (*xlsx.Workbook, error) {
	rows, err := s.repo.DownstreamRows(ctx, model.ReportFilter{Version: s.route, AppKeys: appKeys, From: from, To: to})
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxRows {
		return nil, fmt.Errorf("%w：明细 %d 行超过上限 %d，请缩小时间范围", ErrTooLarge, len(rows), MaxRows)
	}

	aggs := map[string]*subjectAgg{}
	var order []string
	for _, r := range rows {
		key := r.CustomerName + "\x00" + r.CreditCode
		a := aggs[key]
		if a == nil {
			a = &subjectAgg{customer: r.CustomerName, credit: r.CreditCode}
			aggs[key] = a
			order = append(order, key)
		}
		a.add(r)
	}

	byVerdict := map[string][]*subjectAgg{}
	billByCat := map[string]int{}
	var queried, found, miss, zero, billable, free, manual int
	for _, key := range order {
		a := aggs[key]
		v := a.verdict()
		byVerdict[v] = append(byVerdict[v], a)
		queried++
		switch v {
		case verdictMiss:
			miss++
		case verdictManual:
			found++
			manual++
		case verdictZero:
			found++
			zero++
		case verdictBillable:
			found++
			billable++
			cat := categoryOf(a.chargedCat, "", a.reqScope)
			if cat == "" {
				cat = categoryOf(a.dataScope, "", a.reqScope)
			}
			billByCat[cat]++
		case verdictFree:
			found++
			free++
		}
	}

	if customer == "" {
		customer = "全部客户"
	}
	wb := &xlsx.Workbook{}
	sum := [][]any{
		{"客户", customer},
		{"区间（北京时间）", rangeLabel(from, to)},
		{"导出时间", fmtTime(s.now())},
		{"计数单位", "税号。同一个税号无论请求几次，只计 1 个。"},
		{"查询了的税号", "通过鉴权与参数校验、实际发起查询的请求（含查无与上游失败）。"},
		{"查得计费", "本月有 charge_state=CHARGED 且收费类目近 12 个月金额 > 0 的税号。"},
		{"零数据规则", "近 12 个月金额合计 <= 0 不计费、不开免费期；规则版本 " + zerorule.Version},
		{"免费期佐证", "billing_coverage：同一客户 + 同一税号 + 同一类目，首次有金额的查得计费一次，一年内免费。"},
		{},
	}
	hdr := map[int]bool{len(sum): true}
	sum = append(sum, []any{"指标", "税号数"})
	sum = append(sum,
		[]any{"查询了的税号总数", queried},
		[]any{"查得了的税号总数", found},
		[]any{"未查得的税号总数", miss},
		[]any{"查得成功但金额为0的税号总数", zero},
		[]any{"本月需要收费的税号总数", billable},
		[]any{"免费期内（前期已收费）的税号数", free},
		[]any{"无原始数据（待人工）的税号数", manual},
	)
	sum = append(sum, []any{})
	hdr[len(sum)] = true
	sum = append(sum, []any{"本月收费按类目", "税号数"})
	for _, cat := range []string{catInvoice, catTax, catBoth} {
		sum = append(sum, []any{catLabel[cat], billByCat[cat]})
	}
	sum = append(sum, []any{})
	sum = append(sum, []any{"恒等式", "查得 = 金额为0 + 本月收费 + 免费期内 + 待人工",
		fmt.Sprintf("%d = %d + %d + %d + %d", found, zero, billable, free, manual)})
	sh := wb.Add("汇总", nil, sum, []float64{36, 18, 40})
	sh.HeaderRows = hdr

	addSubjects := func(name, verdict string) {
		list := byVerdict[verdict]
		rows := make([][]any, 0, len(list))
		for _, a := range list {
			rows = append(rows, subjectRow(a))
		}
		wb.Add(name, subjectHeaders, rows, subjectWidths)
	}
	addSubjects("本月收费", verdictBillable)
	addSubjects("金额为0", verdictZero)
	addSubjects("未查得", verdictMiss)
	addSubjects("免费期内", verdictFree)
	addSubjects("数据库无记录（待人工）", verdictManual)

	allSubjects := make([][]any, 0, len(order))
	for _, key := range order {
		allSubjects = append(allSubjects, subjectRow(aggs[key]))
	}
	wb.Add("税号明细", subjectHeaders, allSubjects, subjectWidths)

	reqRows := make([][]any, 0, len(rows))
	sorted := append([]*model.DownstreamRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreditCode != sorted[j].CreditCode {
			return sorted[i].CreditCode < sorted[j].CreditCode
		}
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	for _, r := range sorted {
		found := "否"
		if r.FoundData {
			found = "是"
		}
		reqRows = append(reqRows, []any{
			r.CreditCode, r.CompanyName, r.CustomerName, fmtTime(r.CreatedAt), r.RequestID, r.Reqid,
			r.ReqScope, r.DataScope, found, amount(r.InvAmt12m), yesNo(r.InvZero), amount(r.TaxAmt12m), yesNo(r.TaxZero),
			chargeLabel(r.ChargeState), r.ChargedScope, r.FeeStandard, providerNames(r.Providers),
		})
	}
	wb.Add("请求明细", requestHeaders, reqRows, requestWidths)
	return wb, nil
}

// ---------------- 上游对账 ----------------

// upSubject 是「税号 + 类目」的上游聚合。
type upSubject struct {
	credit, company, cat string
	queries              int
	times                []time.Time
	labels, uids, lgs    []string
	found, hasPayload    bool
	hasAmount, allZero   bool
	billable, free       bool // 计费点是否落在本月 / 窗口内
	invAmt, taxAmt       *string
	invZero, taxZero     *bool
	chargeAt             time.Time
	windowTo             time.Time
	chargeRequestID      string
}

var upstreamSubjectHeaders = []string{
	"税号", "企业名称", "类目", "请求次数", "请求时间", "首次请求", "末次请求",
	"查得情况", "近12月开票金额", "发票为0", "近12月税务金额", "税务为0",
	"结论", "计费时间", "免费期至", "计费requestId", "查得子源", "上游订单号", "上游请求号",
}

var upstreamSubjectWidths = []float64{
	22, 28, 10, 10, 40, 20, 20,
	10, 16, 10, 16, 10,
	22, 20, 20, 34, 20, 30, 30,
}

func (a *upSubject) verdict() string {
	switch {
	case !a.found:
		return verdictMiss
	case !a.hasPayload:
		return verdictManual
	case a.billable:
		return verdictBillable
	case a.allZero:
		return verdictZero
	default:
		return verdictFree
	}
}

func (a *upSubject) row() []any {
	sort.Slice(a.times, func(i, j int) bool { return a.times[i].Before(a.times[j]) })
	times := make([]string, len(a.times))
	for i, t := range a.times {
		times[i] = fmtTime(t)
	}
	found := "未查得"
	if a.found {
		found = "查得"
	}
	return []any{
		a.credit, a.company, catLabel[a.cat], a.queries, strings.Join(times, "；"),
		firstLast(a.times, true), firstLast(a.times, false), found,
		amount(a.invAmt), yesNo(a.invZero), amount(a.taxAmt), yesNo(a.taxZero),
		verdictLabel[a.verdict()], proofTime(a.chargeAt), proofTime(a.windowTo), a.chargeRequestID,
		strings.Join(uniq(a.labels), ","), strings.Join(uniq(a.uids), ","), strings.Join(uniq(a.lgs), ","),
	}
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

	codes := map[string]bool{}
	type labelStat struct{ calls, ok, empty, fail int }
	byLabel := map[string]*labelStat{}
	labelSource := map[string]string{}
	periodOK := map[string][]*model.UpstreamCallRow{} // credit|cat -> ok calls in period
	var fails []*model.UpstreamCallRow
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
		if c.CreditCode != "" {
			codes[c.CreditCode] = true
		}
		if c.Status != model.CallOK {
			fails = append(fails, c)
			continue
		}
		for _, cat := range catsOfCall(c) {
			key := c.CreditCode + "\x00" + cat
			periodOK[key] = append(periodOK[key], c)
		}
	}
	creditList := make([]string, 0, len(codes))
	for c := range codes {
		creditList = append(creditList, c)
	}
	sort.Strings(creditList)

	hist, err := s.repo.UpstreamHistory(ctx, model.ReportFilter{Version: s.route, Provider: v.Provider, To: to}, creditList)
	if err != nil {
		return nil, err
	}

	// 按「税号 + 类目」推算一年窗口：第一次有金额的 ok 调用计费并开窗一年。
	type win struct {
		chargeAt, expiresAt time.Time
		chargeRequestID     string
	}
	windows := map[string]*win{} // credit|cat
	for _, h := range hist {
		for _, cat := range catsOfHist(h) {
			key := h.CreditCode + "\x00" + cat
			if !histHasAmount(h, cat) {
				continue // 金额为 0 或无从判定但无金额字段：0 不开窗；无从判定下面单独处理
			}
			w := windows[key]
			if w == nil || !h.CreatedAt.Before(w.expiresAt) {
				windows[key] = &win{chargeAt: h.CreatedAt, expiresAt: h.CreatedAt.AddDate(1, 0, 0), chargeRequestID: h.RequestID}
			}
		}
	}
	// 无从判定（无 payload / 零标记 nil）的历史 ok：按有金额处理，开窗。
	for _, h := range hist {
		for _, cat := range catsOfHist(h) {
			key := h.CreditCode + "\x00" + cat
			if windows[key] != nil {
				continue
			}
			if histUnknown(h, cat) {
				windows[key] = &win{chargeAt: h.CreatedAt, expiresAt: h.CreatedAt.AddDate(1, 0, 0), chargeRequestID: h.RequestID}
			}
		}
	}

	aggs := map[string]*upSubject{}
	var order []string
	ensure := func(credit, cat string) *upSubject {
		key := credit + "\x00" + cat
		a := aggs[key]
		if a == nil {
			a = &upSubject{credit: credit, cat: cat}
			aggs[key] = a
			order = append(order, key)
		}
		return a
	}
	for key, list := range periodOK {
		parts := strings.SplitN(key, "\x00", 2)
		a := ensure(parts[0], parts[1])
		a.found = true
		for _, c := range list {
			a.queries++
			a.times = append(a.times, c.CreatedAt)
			a.labels = append(a.labels, c.Label)
			a.uids = appendNonEmpty(a.uids, c.UID)
			a.lgs = appendNonEmpty(a.lgs, c.LogID)
			if c.HasPayload {
				a.hasPayload = true
				applyUpAmount(a, c)
			}
		}
		a.allZero = a.found && a.hasPayload && !a.hasAmount
		if w := windows[key]; w != nil {
			a.chargeAt, a.windowTo, a.chargeRequestID = w.chargeAt, w.expiresAt, w.chargeRequestID
			if !w.chargeAt.Before(from) && w.chargeAt.Before(to) {
				a.billable = true
			} else if w.chargeAt.Before(from) && from.Before(w.expiresAt) {
				a.free = true
			}
		} else if a.hasAmount {
			a.billable = true
		}
	}
	// 本月只有查无/失败、没有任何 ok 的税号×类目，计入未查得。
	for _, c := range fails {
		if c.CreditCode == "" {
			continue
		}
		cats := catsOfCall(c)
		if len(cats) == 0 {
			continue
		}
		for _, cat := range cats {
			key := c.CreditCode + "\x00" + cat
			if _, ok := periodOK[key]; ok {
				continue
			}
			a := ensure(c.CreditCode, cat)
			a.queries++
			a.times = append(a.times, c.CreatedAt)
		}
	}

	byVerdict := map[string][]*upSubject{}
	var queried, foundN, miss, zero, billable, free, manual int
	billByCat := map[string]int{}
	for _, key := range order {
		a := aggs[key]
		v := a.verdict()
		// 未查得：本月只有失败/查无
		if !a.found {
			v = verdictMiss
		}
		byVerdict[v] = append(byVerdict[v], a)
		queried++
		switch v {
		case verdictMiss:
			miss++
		case verdictManual:
			foundN++
			manual++
		case verdictZero:
			foundN++
			zero++
		case verdictBillable:
			foundN++
			billable++
			billByCat[a.cat]++
		case verdictFree:
			foundN++
			free++
		}
	}

	wb := &xlsx.Workbook{}
	sum := [][]any{
		{"上游", v.Name + "（" + v.Provider + "）"},
		{"区间（北京时间）", rangeLabel(from, to)},
		{"导出时间", fmtTime(s.now())},
		{"计数单位", "税号 + 类目（票、税各算一次）。同一税号无论调用几次，同类目只计 1 个。"},
		{"计费窗口", "不分下游客户；第一次有金额的 ok 调用计费并开窗一年，窗口内不计费。金额为 0 不开窗。"},
		{"零数据规则", "近 12 个月金额合计 <= 0 不计费；规则版本 " + zerorule.Version},
		{},
	}
	hdr := map[int]bool{len(sum): true}
	sum = append(sum, []any{"指标", "税号×类目数"})
	sum = append(sum,
		[]any{"查询了的税号（×类目）总数", queried},
		[]any{"查得了的总数", foundN},
		[]any{"未查得的总数", miss},
		[]any{"查得成功但金额为0的总数", zero},
		[]any{"本月需要收费的总数", billable},
		[]any{"免费期内（前期已收费）", free},
		[]any{"无原始数据（待人工）", manual},
	)
	sum = append(sum, []any{})
	hdr[len(sum)] = true
	sum = append(sum, []any{"本月收费按类目", "数量"})
	for _, cat := range []string{catInvoice, catTax} {
		sum = append(sum, []any{catLabel[cat], billByCat[cat]})
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
	sh := wb.Add("汇总", nil, sum, []float64{36, 18, 14, 12, 12, 12})
	sh.HeaderRows = hdr

	addUp := func(name, verdict string) {
		list := byVerdict[verdict]
		rows := make([][]any, 0, len(list))
		for _, a := range list {
			rows = append(rows, a.row())
		}
		wb.Add(name, upstreamSubjectHeaders, rows, upstreamSubjectWidths)
	}
	addUp("本月收费", verdictBillable)
	addUp("金额为0", verdictZero)
	addUp("未查得", verdictMiss)
	addUp("免费期内", verdictFree)
	addUp("数据库无记录（待人工）", verdictManual)

	all := make([][]any, 0, len(order))
	for _, key := range order {
		all = append(all, aggs[key].row())
	}
	wb.Add("税号明细", upstreamSubjectHeaders, all, upstreamSubjectWidths)

	failHeaders := []string{"税号", "时间", "requestId", "reqid", "客户", "子源", "调用维度", "结果",
		"上游code", "上游订单号", "上游请求号", "说明"}
	failRows := make([][]any, 0, len(fails))
	sort.SliceStable(fails, func(i, j int) bool {
		if fails[i].CreditCode != fails[j].CreditCode {
			return fails[i].CreditCode < fails[j].CreditCode
		}
		return fails[i].CreatedAt.Before(fails[j].CreatedAt)
	})
	for _, c := range fails {
		failRows = append(failRows, []any{c.CreditCode, fmtTime(c.CreatedAt), c.RequestID, c.Reqid, c.CustomerName,
			c.Label, model.DimSet{Invoice: c.DimInvoice, Tax: c.DimTax}.String(), statusLabel(c.Status),
			c.Code, c.UID, c.LogID, c.Msg})
	}
	wb.Add("查无失败调用", failHeaders, failRows, []float64{22, 20, 34, 24, 24, 12, 12, 8, 14, 30, 30, 50})
	return wb, nil
}

func catsOfCall(c *model.UpstreamCallRow) []string {
	var out []string
	if c.DimInvoice {
		out = append(out, catInvoice)
	}
	if c.DimTax {
		out = append(out, catTax)
	}
	return out
}

func catsOfHist(h *model.UpstreamHistoryRow) []string {
	var out []string
	if h.DimInvoice {
		out = append(out, catInvoice)
	}
	if h.DimTax {
		out = append(out, catTax)
	}
	return out
}

func histHasAmount(h *model.UpstreamHistoryRow, cat string) bool {
	if !h.HasPayload {
		return false
	}
	if cat == catInvoice {
		return h.InvZero != nil && !*h.InvZero
	}
	return h.TaxZero != nil && !*h.TaxZero
}

func histUnknown(h *model.UpstreamHistoryRow, cat string) bool {
	if !h.HasPayload {
		return true // 无原始数据：按有金额处理（与线上「无从判定照常计费」一致）
	}
	if cat == catInvoice {
		return h.InvZero == nil
	}
	return h.TaxZero == nil
}

func applyUpAmount(a *upSubject, c *model.UpstreamCallRow) {
	if c.DimInvoice {
		if c.InvZero != nil {
			if !*c.InvZero {
				a.hasAmount = true
				a.invZero = boolPtr(false)
			} else if a.invZero == nil {
				a.invZero = boolPtr(true)
			}
		} else {
			a.hasAmount = true
		}
		if c.InvAmt12m != nil {
			a.invAmt = pickAmt(a.invAmt, c.InvAmt12m)
		}
	}
	if c.DimTax {
		if c.TaxZero != nil {
			if !*c.TaxZero {
				a.hasAmount = true
				a.taxZero = boolPtr(false)
			} else if a.taxZero == nil {
				a.taxZero = boolPtr(true)
			}
		} else {
			a.hasAmount = true
		}
		if c.TaxAmt12m != nil {
			a.taxAmt = pickAmt(a.taxAmt, c.TaxAmt12m)
		}
	}
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
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
	case model.ChargeZero:
		return "金额为0"
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

func boolPtr(b bool) *bool { return &b }
