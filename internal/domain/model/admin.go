package model

import "time"

// AdminUser is an internal operator account for the admin console (DESIGN §16.1).
type AdminUser struct {
	ID           int64
	Username     string
	PasswordHash string // 加盐哈希; 生产应换 bcrypt/argon2
	Role         string // ADMIN（本期单一角色）
	CreatedAt    time.Time
}

// UserDetail is the admin-facing aggregate view of a普通用户 (license + 成功
// 查得数), used by the user management screens (DESIGN §16.2). v0.6 起取消额度
// 限制，仅保留 ServiceUsed (累计成功查得数); v0.7 起新增手机号、密钥创建时间、
// 授权过期日期，并移除每用户 IP 白名单 (IP 准入交由阿里云 ECS 安全组)。
type UserDetail struct {
	LicenseID       string    `json:"licenseId"`
	AppKey          string    `json:"appKey"`
	Name            string    `json:"name"`
	Mobile          string    `json:"mobile"` // 联系手机号 (前端脱敏展示)
	Status          string    `json:"status"`
	ClientUUID      string    `json:"clientUuid"`
	ServiceUsed     int64     `json:"serviceUsed"`     // 累计成功查得数 (当前路由作用域)
	TotalCalls      int64     `json:"totalCalls"`      // 累计调用上游次数 (当前路由作用域)
	Rates           FeeRates  `json:"rates"`           // 三档合同价（分）；0 = 走全局缺省
	SecretCreatedAt time.Time `json:"secretCreatedAt"` // 当前密钥创建/轮换时间
	ValidTo         time.Time `json:"validTo"`         // 授权过期日期
	CreatedAt       time.Time `json:"createdAt"`
}

// AuditRecord is the rich per-request audit log (DESIGN §16.3 / §16.5). It is
// append-only and keyed by requestId for cross-referencing the billing ledger
// and the [requestId]-prefixed logs (§9).
type AuditRecord struct {
	ID             int64     `json:"id"`
	RequestID      string    `json:"requestId"`
	Version        string    `json:"version"` // 路由名 (x1/v9/v8/zlf/blk)，区分共享 license 的 v8/v9
	AppKey         string    `json:"appKey"`
	TradeNo        string    `json:"tradeNo"`
	Reqid          string    `json:"reqid"`
	ClientIP       string    `json:"clientIp"`
	CalledUpstream bool      `json:"calledUpstream"` // 是否成功调用上游
	FoundData      bool      `json:"foundData"`      // 是否查得数据 (busiCode 10)
	BusiCode       int       `json:"busiCode"`
	BusiMsg        string    `json:"busiMsg"`
	UpstreamCode   string    `json:"upstreamCode"`
	UpstreamUID    string    `json:"upstreamUid"`
	UpstreamLogID  string    `json:"upstreamLogId"`
	Billed         bool      `json:"billed"` // 是否查得数据（计入成功查得数）
	// 新寻源/计费口径：请求了哪些维度、实际查得哪些维度、按哪档标准收多少、
	// 上游花了多少（逐源明细在 upstream_call，按 requestId 下钻）。
	ReqScope        string `json:"reqScope"`    // invoice+tax / invoice / tax
	DataScope       string `json:"dataScope"`   // 实际查得维度
	FeeStandard     string `json:"feeStandard"` // both/invoice/tax/none
	AmountFen       int64  `json:"amountFen"`
	UpstreamCostFen int64  `json:"upstreamCostFen"`
	LatencyMs       int64  `json:"latencyMs"`
	NameMask       string    `json:"nameMask"`
	IDCardMask     string    `json:"idCardMask"`
	MobileMask     string    `json:"mobileMask"`
	ErrMsg         string    `json:"errMsg"`
	CreatedAt      time.Time `json:"createdAt"`
}

// AuditFilter narrows an audit query (DESIGN §16.3). AppKeys (任一匹配) 支持按
// uuid/名称/手机号检索时先解析出的多个 appKey；AppKey 仍保留精确匹配入口。
type AuditFilter struct {
	Version  string // 路由作用域 (由 admin.Service 注入，区分共享 license 的 v8/v9)
	AppKey   string
	AppKeys  []string
	BusiCode *int
	Limit    int
	Offset   int
}
