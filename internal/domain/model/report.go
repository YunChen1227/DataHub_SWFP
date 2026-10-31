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

// DownstreamRow 是下游账单的一条查得请求（audit_log 为主，LEFT JOIN request_payload）。
// HasPayload=false 即上线前的历史请求，零数据判定无从得出。
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
	ChargeState  string
	Providers    string // 本次查得的上游 provider，逗号分隔
	HasPayload   bool
	ZeroVerdict
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
