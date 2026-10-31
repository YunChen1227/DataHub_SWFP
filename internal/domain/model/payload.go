package model

import (
	"encoding/json"
	"time"
)

// RequestPayload 是一次请求的原始数据留存（request_payload 表）：下游入参、返回给
// 下游的数据、以及零数据判定结果。Sources 是本次查得/部分返回的各上游原始数据，
// 与本行一起写入（upstream_payload 表）。
//
// 金额为保留两位小数的元字符串；判定结果为 nil 表示该维度本次没查得（无从判定）。
type RequestPayload struct {
	RequestID     string             `json:"requestId"`
	AppKey        string             `json:"appKey"`
	Version       string             `json:"version"`
	Reqid         string             `json:"reqid"`
	CreditCode    string             `json:"creditCode"`
	RequestBody   json.RawMessage    `json:"requestBody,omitempty"`
	ResponseRange json.RawMessage    `json:"responseRange,omitempty"`
	ZeroVerdict                      // 请求级：合并各查得源
	CreatedAt     time.Time          `json:"createdAt"`
	Sources       []*UpstreamPayload `json:"sources,omitempty"`
}

// UpstreamPayload 是一个上游源本次返回的业务数据（解密/解包后、契约映射前）。
type UpstreamPayload struct {
	RequestID   string          `json:"requestId"`
	AppKey      string          `json:"appKey"`
	Version     string          `json:"version"`
	Reqid       string          `json:"reqid"`
	SourceLabel string          `json:"sourceLabel"`
	SourceName  string          `json:"sourceName"`
	Provider    string          `json:"provider"`
	Status      string          `json:"status"`
	RawData     json.RawMessage `json:"rawData,omitempty"`
	ZeroVerdict
	CreatedAt time.Time `json:"createdAt"`
}

// ZeroVerdict 是「近 12 个月金额是否全为 0」的落库形态（规则见 zerorule 包）。
type ZeroVerdict struct {
	InvAmt12m   *string `json:"invAmt12m,omitempty"`
	TaxAmt12m   *string `json:"taxAmt12m,omitempty"`
	InvZero     *bool   `json:"invZero,omitempty"`
	TaxZero     *bool   `json:"taxZero,omitempty"`
	RuleVersion string  `json:"ruleVersion"`
}
