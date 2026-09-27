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
	ID             int64  `json:"id"`
	RequestID      string `json:"requestId"`
	Version        string `json:"version"` // 路由名 (x1/v9/v8/zlf/blk)，区分共享 license 的 v8/v9
	AppKey         string `json:"appKey"`
	TradeNo        string `json:"tradeNo"`
	Reqid          string `json:"reqid"`
	ClientIP       string `json:"clientIp"`
	CalledUpstream bool   `json:"calledUpstream"` // 是否成功调用上游
	FoundData      bool   `json:"foundData"`      // 是否查得数据 (busiCode 10)
	BusiCode       int    `json:"busiCode"`
	BusiMsg        string `json:"busiMsg"`
	UpstreamCode   string `json:"upstreamCode"`
	UpstreamUID    string `json:"upstreamUid"`
	UpstreamLogID  string `json:"upstreamLogId"`
	// Billed 的语义自「主体年度计费」起改为**本次是否真的收了钱**
	// (ChargeState == CHARGED)。它原先等于 FoundData（都来自 busiCode 10，完全重复）。
	// FoundData 保持「是否查得数据」不变，于是同一行里 FoundData=true 且 Billed=false
	// 就是一次免费期命中——这是「计费与查得统计彻底分开」的落地形态。
	Billed bool `json:"billed"`
	// 新寻源/计费口径：请求了哪些维度、实际查得哪些维度、按哪档标准收多少、
	// 上游花了多少（逐源明细在 upstream_call，按 requestId 下钻）。
	ReqScope        string `json:"reqScope"`    // invoice+tax / invoice / tax
	DataScope       string `json:"dataScope"`   // 实际查得维度
	FeeStandard     string `json:"feeStandard"` // both/invoice/tax/none
	AmountFen       int64  `json:"amountFen"`
	UpstreamCostFen int64  `json:"upstreamCostFen"`
	// 主体年度计费：本次查的企业、为哪些类目收了钱、哪些类目命中免费期、计费标识。
	CreditCode   string      `json:"creditCode"`
	ChargedScope string      `json:"chargedScope"`
	CoveredScope string      `json:"coveredScope"`
	ChargeState  ChargeState `json:"chargeState"`
	LatencyMs    int64       `json:"latencyMs"`
	NameMask     string      `json:"nameMask"`
	IDCardMask   string      `json:"idCardMask"`
	MobileMask   string      `json:"mobileMask"`
	ErrMsg       string      `json:"errMsg"`
	CreatedAt    time.Time   `json:"createdAt"`
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

// StatsGranularity 是用量统计的时间桶粒度 (按年/月/日)。
type StatsGranularity string

const (
	GranularityDay   StatsGranularity = "day"
	GranularityMonth StatsGranularity = "month"
	GranularityYear  StatsGranularity = "year"
)

// StatsFilter narrows a usage-stats aggregation over the audit log (§16.4)。
// 统计按 (用户 appKey, 时间桶) 分组：每个用户各自的时间趋势 (请求次数/成功次数)。
type StatsFilter struct {
	Version     string // 路由作用域 (由 admin.Service 注入)
	AppKeys     []string
	Granularity StatsGranularity // 默认 day
	From        time.Time        // 可选区间 [From, To)
	To          time.Time
	Limit       int
}

// UsageStat is one (user, time-bucket) aggregate of request/success counts,
// powering the admin 「按年/月/日」统计面板 (§16.4)。时间桶按北京时间 (+08:00)
// 归档，Bucket 为其可读标签 (YYYY / YYYY-MM / YYYY-MM-DD)。
//
// 口径取自 audit_log（一次请求一行），与逐源明细 upstream_call（一次请求 N 行）
// 分开聚合：两表 join 后再 count 会把请求数按源的行数放大，故「哪个源查成功」
// 走 SourceStats 单独下钻，见 sourcing.go。
type UsageStat struct {
	Bucket  string `json:"bucket"`  // 时间桶标签 (北京时间)
	AppKey  string `json:"appKey"`  // 用户 uuid
	Name    string `json:"name"`    // 用户名称/备注
	Total   int64  `json:"total"`   // 请求次数 (审计行总数，含鉴权/参数失败)
	Success int64  `json:"success"` // 成功次数 (查得数据 found_data)
}
