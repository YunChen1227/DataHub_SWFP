package model

import "time"

// 本文件承载「按维度寻源 + 按实得内容计费 + 按源对账」这条新流程的核心类型
// (设计_多源计费与上游对账.md 的落地形态)。三个概念一定要分清：
//
//   - Want   下游这次要什么（发票 / 税务 / 两者）
//   - Provides 某个源能提供什么
//   - Got    本次实际查得了什么 ← 唯一的计费依据
//
// 计费只看 Got：请求了两项但只查得发票，就按【单发票】收费。

// Dimension 是一次查询的数据维度。
type Dimension string

const (
	DimInvoice Dimension = "invoice" // 发票数据
	DimTax     Dimension = "tax"     // 税务数据
)

// DataType 是下游请求维度的入参取值 (QueryCommand.DataType)。
const (
	DataTypeInvoice = "invoice" // 仅发票
	DataTypeTax     = "tax"     // 仅税务
	DataTypeBoth    = "both"    // 发票 + 税务（缺省，兼容未带该入参的老下游）
)

// DimSet 是一组数据维度。只有两个维度，用两个 bool 而非 map/bitmask——零值即空集，
// 可直接比较，JSON 序列化后下游可读。
type DimSet struct {
	Invoice bool `json:"invoice"`
	Tax     bool `json:"tax"`
}

// AllDims 是全维度集（发票 + 税务）。
func AllDims() DimSet { return DimSet{Invoice: true, Tax: true} }

// NewDimSet builds a set from an explicit dimension list.
func NewDimSet(dims ...Dimension) DimSet {
	var d DimSet
	for _, x := range dims {
		switch x {
		case DimInvoice:
			d.Invoice = true
		case DimTax:
			d.Tax = true
		}
	}
	return d
}

// DimSetOf maps a DataType 入参 to its dimension set；未知/空值按 both 处理
// （老下游不带 dataType 时行为与改造前一致：发票和税务都查）。
func DimSetOf(dataType string) DimSet {
	switch dataType {
	case DataTypeInvoice:
		return DimSet{Invoice: true}
	case DataTypeTax:
		return DimSet{Tax: true}
	default:
		return AllDims()
	}
}

func (d DimSet) Has(x Dimension) bool {
	switch x {
	case DimInvoice:
		return d.Invoice
	case DimTax:
		return d.Tax
	}
	return false
}

func (d DimSet) Empty() bool { return !d.Invoice && !d.Tax }

// Both reports whether the set holds 发票 and 税务 at the same time.
func (d DimSet) Both() bool { return d.Invoice && d.Tax }

func (d DimSet) Union(o DimSet) DimSet {
	return DimSet{Invoice: d.Invoice || o.Invoice, Tax: d.Tax || o.Tax}
}

// Intersect 返回 d 与 o 的交集（用于「这个源能提供的维度里，有几个是我这次要的」）。
func (d DimSet) Intersect(o DimSet) DimSet {
	return DimSet{Invoice: d.Invoice && o.Invoice, Tax: d.Tax && o.Tax}
}

// Covers 报告 d 是否已覆盖 want 的全部维度（寻源循环的终止条件）。
func (d DimSet) Covers(want DimSet) bool {
	return (!want.Invoice || d.Invoice) && (!want.Tax || d.Tax)
}

// Missing 返回 want 里 d 尚未覆盖的维度（缺项补齐的输入）。
func (d DimSet) Missing(want DimSet) DimSet {
	return DimSet{Invoice: want.Invoice && !d.Invoice, Tax: want.Tax && !d.Tax}
}

// String 是落库/日志用的稳定短名：invoice+tax / invoice / tax / none。
func (d DimSet) String() string {
	switch {
	case d.Invoice && d.Tax:
		return "invoice+tax"
	case d.Invoice:
		return "invoice"
	case d.Tax:
		return "tax"
	default:
		return "none"
	}
}

// FeeStandard 是本次请求的最终收费标准，由【实际查得内容】决定。
type FeeStandard string

const (
	FeeBoth    FeeStandard = "both"    // 实得发票 + 税务
	FeeInvoice FeeStandard = "invoice" // 实得仅发票
	FeeTax     FeeStandard = "tax"     // 实得仅税务
	FeeNone    FeeStandard = "none"    // 皆无 → 不计费
)

// StandardOf 把实际查得维度映射为收费标准。
func StandardOf(got DimSet) FeeStandard {
	switch {
	case got.Invoice && got.Tax:
		return FeeBoth
	case got.Invoice:
		return FeeInvoice
	case got.Tax:
		return FeeTax
	default:
		return FeeNone
	}
}

// FeeRates 是三档收费标准的单价，单位「分」的整数——金额一律不用浮点，避免
// 累计对账时的舍入漂移。挂在 license 上即客户合同价；某档为 0 时由全局缺省
// (config billing.rates) 兜底，见 OrDefault。
type FeeRates struct {
	BothFen    int64 `json:"bothFen"`
	InvoiceFen int64 `json:"invoiceFen"`
	TaxFen     int64 `json:"taxFen"`
}

// Of 返回某档标准的单价。FeeNone 恒为 0（查无不向下游计费）。
func (r FeeRates) Of(s FeeStandard) int64 {
	switch s {
	case FeeBoth:
		return r.BothFen
	case FeeInvoice:
		return r.InvoiceFen
	case FeeTax:
		return r.TaxFen
	default:
		return 0
	}
}

// OrDefault 用 def 填补本 license 未单独定价（0）的档位。
func (r FeeRates) OrDefault(def FeeRates) FeeRates {
	if r.BothFen == 0 {
		r.BothFen = def.BothFen
	}
	if r.InvoiceFen == 0 {
		r.InvoiceFen = def.InvoiceFen
	}
	if r.TaxFen == 0 {
		r.TaxFen = def.TaxFen
	}
	return r
}

// 逐源调用状态。前三个与聚合段 status 同名同义（契约层的 sourceStatus 直接透出），
// skipped 是串行寻源引入的第四态：该源本次根本没被调用。
const (
	CallOK      = "ok"      // 查得
	CallEmpty   = "empty"   // 查无
	CallError   = "error"   // 该源调用失败
	CallSkipped = "skipped" // 未调用（更高优先级源已满足 / 本次已请求过 / 超时延预算）
)

// SourceCall 是一次下游请求里对某个上游子源的调用记录（每源一条，未调用的源同样
// 出一条 skipped）。它是「上游成本对账」与「寻源轨迹」的原子记录：一次 swfp 查询
// 会产生多条，各自带自己的上游订单号/请求号，不再像聚合器那样归并成一对丢掉其余。
type SourceCall struct {
	Seq       int    `json:"seq"`      // 本次请求内的调用顺序 (1 起)；skipped 为 0
	Source    string `json:"source"`   // 逻辑源名：寻源优先级列表的单位，也是去重的键
	Label     string `json:"label"`    // 契约段名 invoice1/invoice2/tax1/tax2/sales
	Alias     string `json:"alias"`    // 对下游脱敏编号 源1..源5
	Provider  string `json:"provider"` // 上游 kind: entcredit/salesdata
	Dims      DimSet `json:"dims"`     // 该次调用覆盖的维度
	Status    string `json:"status"`   // ok | empty | error | skipped
	Code      string `json:"code"`     // 上游业务码原值
	Msg       string `json:"msg"`
	UID       string `json:"uid"`       // 该源的上游订单号
	LogID     string `json:"logId"`     // 该源的上游请求号
	CostFen   int64  `json:"costFen"`   // 该次调用产生的上游成本（分）
	LatencyMs int64  `json:"latencyMs"` //
	Reason    string `json:"reason"`    // status=skipped 时的原因
}

// UpstreamCallRecord 是 SourceCall 的持久化形态：加上与台账同构的关联键
// (app_key, version, reqid) 与用于 join 审计的 request_id。
type UpstreamCallRecord struct {
	SourceCall
	AppKey    string    `json:"appKey"`
	Version   string    `json:"version"`
	Reqid     string    `json:"reqid"`
	RequestID string    `json:"requestId"`
	Billable  bool      `json:"billable"` // 该源是否构成本次计费依据 (status=ok)
	CreatedAt time.Time `json:"createdAt"`
}

// UpstreamCallFilter narrows a 逐源明细 query (后台按 requestId 下钻)。
type UpstreamCallFilter struct {
	Version   string
	RequestID string
	Reqid     string
	AppKey    string
	Limit     int
}

// SourceSummary 汇总一次请求的逐源结果，供台账速查「这笔钱是哪个源挣的、花了多少」
// 而不必回头 join 明细表。
type SourceSummary struct {
	Total   int    // 参与寻源的源数（含 skipped）
	OK      int    // 查得的源数
	Err     int    // 失败的源数
	Called  int    // 实际调用的源数（ok+empty+error）
	CostFen int64  // 上游总成本（分）
	Code    string // 代表源的上游业务码（优先第一个 ok）
	UID     string // 代表源的上游订单号
	LogID   string // 代表源的上游请求号
}

// SummarizeSources 计算 SourceSummary。代表值取「第一个 status=ok 的源」，
// 其次「第一个带上游标识的源」——保证台账那三列在有上游应答时永不为空。
func SummarizeSources(calls []SourceCall) SourceSummary {
	var s SourceSummary
	s.Total = len(calls)
	repOK := false
	for _, c := range calls {
		s.CostFen += c.CostFen
		switch c.Status {
		case CallOK:
			s.OK++
			s.Called++
			if !repOK {
				repOK = true
				s.Code, s.UID, s.LogID = c.Code, c.UID, c.LogID
			}
		case CallError:
			s.Err++
			s.Called++
		case CallEmpty:
			s.Called++
		}
		if !repOK && s.UID == "" && (c.UID != "" || c.LogID != "") {
			s.Code, s.UID, s.LogID = c.Code, c.UID, c.LogID
		}
	}
	return s
}
