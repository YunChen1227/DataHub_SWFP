// Package model holds the framework-agnostic core types shared across all
// layers (DESIGN §2/§5/§11). It depends on nothing but the standard library so
// it never participates in import cycles.
package model

import "fmt"

// QueryCommand is the parsed SWFP client request body. 必填 creditCode（统一社会
// 信用代码，对齐上游证通 entcreditapi args.creditCode）；可选 dataType 声明本次
// 要哪些维度（invoice/tax/both，缺省 both）；可选 scope 控制是否调用可选源。
type QueryCommand struct {
	CreditCode string `json:"creditCode"`
	DataType   string `json:"dataType"`
	Scope      string `json:"scope"`
}

// SignedRequest carries the request envelope material needed for MD5 signature
// verification (接口文档-经济能力.doc 网关 appKey/appSecret / DESIGN §8.1).
// BodyParams are the non-empty business params (string) used to recompute the
// signature; appKey/sign/encryptionType do not participate in signing.
type SignedRequest struct {
	AppKey         string
	Sign           string
	EncryptionType int
	BodyParams     map[string]string
}

// LicenseView is the authenticated client identity + status (DESIGN §7.1).
// IP 准入自 v0.7 起移交阿里云 ECS 安全组，网关不再做 IP 白名单。
type LicenseView struct {
	LicenseID  string
	AppKey     string
	ClientUUID string
	Status     string // ACTIVE / SUSPENDED / EXPIRED
	// Rates 是该客户的合同价（三档，单位分）；某档为 0 时由全局缺省兜底。
	Rates FeeRates
}

// Active reports whether the license may call the service.
func (l *LicenseView) Active() bool { return l != nil && l.Status == "ACTIVE" }

// UpstreamRequest carries the parameters the upstream client needs. Want 是本次
// 请求的维度（寻源引擎据此选优先级列表；空集按全维度处理）。
type UpstreamRequest struct {
	CreditCode string
	Scope      string
	Want       DimSet
	Reqid      string
}

// Scope 取值（swfp 调用范围）。
const (
	ScopeAll   = "all"   // 全部数据源（含可选源），缺省
	ScopeBasic = "basic" // 仅基础数据源（跳过 optional 子源）
)

// UpstreamResult is the normalized upstream response (DESIGN §6). 唯一上游伽马把原生
// 响应归一化为此形态; Code 统一为 ("001" 查得 / "999" 查无) so billing + downstream body 统一。
type UpstreamResult struct {
	Code   string // "001" 查得 / "999" 查无
	Msg    string
	UID    string // 上游流水号 (伽马 seqNo)
	Reqid  string
	Range  string // 收入模型评分
	Verify string // 上游签名 (伽马为空)
	LogID  string

	// 以下三项由串行寻源引擎 (upstream.Sourcer) 填充，单源直通路由留零值。
	Got     DimSet       // 本次实际查得的维度 = 计费依据
	Sources []SourceCall // 逐源寻源轨迹（含未调用的源）
	CostFen int64        // 本次上游总成本（分）
}

// RequeryResult is the outcome of an idempotent re-query (DESIGN §7.3).
// Reachable=false means the upstream could not be reached此刻; the ledger stays
// PENDING for the reconciliation job to settle.
type RequeryResult struct {
	Reachable bool
	Result    *UpstreamResult // nil when upstream confirms "未执行/未扣费"
}

// UpstreamError 表示上游"已应答但以业务码明确拒绝/失败"的错误（区别于网络不可达）。
// 它承载上游返回的可追查标识，供 orchestrator 写入审计——即便请求最终落 PENDING，
// 也能凭 UID(上游订单号) / LogID(上游请求号) 向上游对账、向上追查失败原因。
// 上游客户端在遇到"非成功业务码"时应返回本类型（而非裸 fmt.Errorf），字段尽量填全：
//   - Code：上游业务/状态码原值（如 "461"/"1002"/"SW0001"/"4"）
//   - Msg ：上游返回的错误消息
//   - UID ：上游订单号（对账用，如 OutBizNo/seqNo/respOrder/orderNo）
//   - LogID：上游请求/日志号（对账用，如 RequestId/reqno）
// 纯网络/传输失败（上游不可达、读超时）不用本类型——那时没有上游标识可填。
type UpstreamError struct {
	Code  string
	Msg   string
	UID   string
	LogID string
	Err   error // 可选底层原因

	// 全部源失败时同样要能拿到每源标识与已经产生的成本——钱已经花了，不能因为
	// 本次对下游不计费就在库里看不见（亏损单必须可对账）。
	Sources []SourceCall
	CostFen int64
}

func (e *UpstreamError) Error() string {
	s := fmt.Sprintf("上游业务失败 code=%s msg=%s", e.Code, e.Msg)
	if e.UID != "" {
		s += " uid=" + e.UID
	}
	if e.LogID != "" {
		s += " logId=" + e.LogID
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// BillingState is the ledger lifecycle state (DESIGN §7.3). There is no UNKNOWN
// terminal state — PENDING is always resolved by re-query or reconciliation.
type BillingState string

const (
	StatePending  BillingState = "PENDING"
	StateBilled   BillingState = "BILLED"
	StateUnbilled BillingState = "UNBILLED"
)

// BillingDecision is the verdict the billing engine produces.
//   - Resolved → 上游给出了确定结论（查得或查无）→ 台账 BILLED；否则 UNBILLED。
//   - Returned → upstream produced查得数据 (成功查得数 +1, = busiCode 10).
//
// The two are kept separate so the口径 can diverge by config (DESIGN §7.4):
// 999 查无结果 is Resolved=true, Returned=false.
// Standard/AmountFen 是本次的收费标准与应收金额，由【实际查得内容】(Result.Got)
// 决定；CostFen 是本次上游总成本，两者一起构成单笔毛利。
type BillingDecision struct {
	Resolved  bool
	Returned  bool
	Result    *UpstreamResult
	Standard  FeeStandard
	AmountFen int64
	CostFen   int64

	// 主体年度计费（billing.Service.ApplyCoverage 填充，见 subject.go）：Decide 先按
	// 实得维度给出毛口径的 Standard/AmountFen，随后 ApplyCoverage 把其中「还在免费期」
	// 的类目扣掉并重算金额。Charged 是真正产生应收的类目，Covered 是查得了但免费的
	// 类目，两者之并恒等于 Result.Got。未装配主体计费仓储时三者保持零值，
	// Standard/AmountFen 即退化为按次计费。
	Charged     DimSet
	Covered     DimSet
	ChargeState ChargeState
}

// Ledger is the append-only billing record (DESIGN §11.3). Version 标记产生该
// 台账的路由 (x1/v9/v8/zlf/blk)，使共享同一 license 的 v8/v9 在域库内幂等/统计相互独立。
type Ledger struct {
	ID             int64
	AppKey         string
	Version        string // 路由名 (= 调用的版本)，幂等键 (app_key, version, reqid) 的一部分
	TradeNo        string
	Reqid          string
	RequestID      string
	UpstreamCode   string
	BusiCode       int
	UpstreamUID    string
	UpstreamLogID  string
	State          BillingState
	CountedService bool

	// 结算时回填（LedgerSettlement）：本次按哪档标准收、收多少、上游花了多少。
	FeeStandard     FeeStandard
	AmountFen       int64
	UpstreamCostFen int64
	SourceTotal     int
	SourceOK        int
	SourceErr       int

	// 主体年度计费：本次查的是哪家企业、为哪些类目收了钱、哪些类目命中免费期，
	// 以及计费标识。有了这四列，台账的每一行都能自解释「本次为什么收/没收钱」，
	// 不必回头 join 免费期表。
	CreditCode   string
	ChargedScope string // invoice+tax / invoice / tax / none
	CoveredScope string
	ChargeState  ChargeState
}

// LedgerSettlement 是一次结算要回填进台账的全部字段。它替代了原来
// UpdateState(state, countedService) 的两个参数——新流程要落计费标准/金额/上游
// 成本，以及一直建了却从未写入的四个上游对账列 (设计_多源计费与上游对账 §2.2)。
type LedgerSettlement struct {
	State           BillingState
	CountedService  bool
	FeeStandard     FeeStandard
	AmountFen       int64
	UpstreamCostFen int64
	BusiCode        int
	UpstreamCode    string
	UpstreamUID     string
	UpstreamLogID   string
	SourceTotal     int
	SourceOK        int
	SourceErr       int

	// 主体年度计费的四列。与上游标识同理走「空值不覆盖」：复查/对账路径不带这些
	// 信息，不能把首次结算写下的值抹掉。
	CreditCode   string
	ChargedScope string
	CoveredScope string
	ChargeState  ChargeState
}

// ServiceQuotaView is the client-facing snapshot (DESIGN §5.2). 无额度限制，
// 按路由独立统计：Used = 累计成功查得数, Calls = 累计调用上游次数。
type ServiceQuotaView struct {
	Status string
	Used   int64 // 成功查得数据次数（累计，busiCode 10）
	Calls  int64 // 调用上游次数（累计，CalledUpstream）
	// Billing 是计费口径统计，与上面两个调用口径的数**互不换算**：免费期内的查询
	// 照常累加 Used/Calls 但不计费，所以 ChargedTotal 不等于 Calls 也不等于 Used。
	Billing BillingCounters
}

// QueryResponse is the unified client response envelope
// (接口文档-经济能力.doc §3.1.4): {head, body}. body 省略于 head 级错误。
type QueryResponse struct {
	Head ResponseHead `json:"head"`
	Body *QueryBody   `json:"body,omitempty"`
}

// ResponseHead is the gateway头部 (接口文档-经济能力.doc §3.1.4).
//   - ErrorCode "0" = 成功（含查得/查无）; 非 0 = 网关级错误。
//   - LogID = 全链路 requestId (§9); Time = 处理耗时 ms; Timestamp = 毫秒时间戳。
type ResponseHead struct {
	ErrorCode string `json:"errorCode"`
	LogID     string `json:"logId"`
	Time      int64  `json:"time"`
	ErrorMsg  string `json:"errorMsg"`
	Timestamp int64  `json:"timestamp"`
}

// QueryBody is the SWFP business response body.
type QueryBody struct {
	Code   string       `json:"code"`
	Msg    string       `json:"msg"`
	UID    string       `json:"uid"`
	Reqid  string       `json:"reqid"`
	Verify string       `json:"verify"`
	Result *RangeResult `json:"result,omitempty"`
}

// RangeResult is the result content: range carries the SWFP contract JSON payload.
type RangeResult struct {
	Range string `json:"range"`
}

// Versions is the canonical ordered list of service versions (routes). DataHub_SWFP
// 仅保留 swfp 路由：聚合税务+发票四产品码 + 可选销项数据 (企业维度 creditCode 入参，
// 见 upstream/entcredit.go / salesdata.go)。swfp 同时充当后台登录的控制面。
var Versions = []string{"swfp"}

// Domains is the canonical ordered list of license 域 (存储边界)。swfp 独占一套
// DB + Redis + license 表。
var Domains = []string{"swfp"}

// RouteDomain maps a route (version) to its license 域。
func RouteDomain(route string) string {
	return route
}

// DemoAppKey returns the per-域 dev demo license appKey（开发/测试专用；生产库
// 不播种 demo）。
func DemoAppKey(route string) string {
	switch RouteDomain(route) {
	case "swfp":
		return "y890swfp"
	default:
		return "demo-" + route
	}
}

// ValidVersion reports whether v is one of the supported service versions (routes).
func ValidVersion(v string) bool {
	for _, x := range Versions {
		if x == v {
			return true
		}
	}
	return false
}
