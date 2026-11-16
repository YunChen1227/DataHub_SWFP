package model

import "time"

// UpstreamAllowEntry 是全局上游白名单的一条：(税号, 上游) → 允许的票/税维度。
// Provider 取值与 upstream.Provider* 常量一致：entcredit / ctax / sy / salesdata。
type UpstreamAllowEntry struct {
	CreditCode string    `json:"creditCode"`
	Provider   string    `json:"provider"`
	Tax        bool      `json:"tax"`
	Invoice    bool      `json:"invoice"`
	CallTime   time.Time `json:"callTime,omitempty"`
	SourceFile string    `json:"sourceFile,omitempty"`
}

// Dims 返回本条目允许的维度。
func (e UpstreamAllowEntry) Dims() DimSet {
	return DimSet{Tax: e.Tax, Invoice: e.Invoice}
}

// UpstreamAllowStats 是白名单的分类计数（管理页 / 启动日志）。
type UpstreamAllowStats struct {
	Total        int            `json:"total"`
	TaxOnly      int            `json:"taxOnly"`
	InvoiceOnly  int            `json:"invoiceOnly"`
	Both         int            `json:"both"`
	ByProvider   map[string]int `json:"byProvider"`
	UniqueCodes  int            `json:"uniqueCodes"`
}

// UpstreamAllowlistView 是管理接口返回体。
type UpstreamAllowlistView struct {
	Enabled bool               `json:"enabled"`
	Stats   UpstreamAllowStats `json:"stats"`
}
