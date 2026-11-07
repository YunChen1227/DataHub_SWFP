package model

import "time"

// ReportFilter 限定一次出账/对账导出的数据范围：[From, To) 绝对区间。
type ReportFilter struct {
	Version  string   // 路由作用域
	AppKeys  []string // 下游账单：客户的 appKey（空 = 全部客户）
	Provider string   // 上游对账：upstream_call.provider
	From     time.Time
	To       time.Time
}

// CoverageProof 是某主体某类目当前（或当次计费）免费期的佐证，来自 billing_coverage。
type CoverageProof struct {
	ChargedAt       time.Time // 本轮计费时刻 = 窗口起点
	ExpiresAt       time.Time // 免费期截止（不含）
	ChargeRequestID string    // 本轮计费那次的 request_id
	ChargeReqid     string
}

// DownstreamRow 是下游账单的一条有效请求（通过鉴权、参数校验、实际发起查询：
// audit_log.credit_code <> ''）。含查得、查无与上游失败；HasPayload=false 即上线前
// 的历史请求，零数据判定无从得出。
type DownstreamRow struct {
	CreatedAt    time.Time
	RequestID    string
	Reqid        string
	AppKey       string
	CustomerName string
	CreditCode   string
	CompanyName  string // 取自返回给下游的 nsrjbxx.nsrmc
	ReqScope     string
	DataScope    string
	FeeStandard  string
	ChargedScope string
	ChargeState  string
	FoundData    bool
	BusiCode     int
	Providers    string // 本次查得的上游 provider，逗号分隔
	HasPayload   bool
	ZeroVerdict
	InvCoverage CoverageProof // 发票类目免费期（无窗口时为零值）
	TaxCoverage CoverageProof // 税务类目免费期
}

// UpstreamCallRow 是上游对账的一次子源调用（upstream_call 为主，LEFT JOIN
// upstream_payload）。
type UpstreamCallRow struct {
	CreatedAt    time.Time
	RequestID    string
	Reqid        string
	AppKey       string
	CustomerName string
	CreditCode   string
	Label        string
	SourceName   string
	Provider     string
	Status       string
	Code         string
	Msg          string
	UID          string
	LogID        string
	DimInvoice   bool
	DimTax       bool
	HasPayload   bool
	ZeroVerdict
}

// UpstreamHistoryRow 是某上游公司在 To 之前对一批税号的全部 ok 调用，用于推算
// 「上游公司 + 税号 + 类目」的一年计费窗口（不分下游客户）。
type UpstreamHistoryRow struct {
	CreatedAt  time.Time
	RequestID  string
	CreditCode string
	Label      string
	DimInvoice bool
	DimTax     bool
	HasPayload bool
	ZeroVerdict
}
