package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// 税票数据查询C 上游 (惠众征信)：本仓第一个**综合源**——一次调用即可同时给出税务与
// 发票两个维度，由入参 type 选择 (1 仅税务 / 2 仅发票 / 3 两者)。
//
// ⚠ 协议以**真实服务器为准**，与 docs/【税票数据查询c】接口文档.docx V1.0 多处不符
// （2026-09-13 用测试环境凭证联调所得，文档 V1.0 的信封/字段/返回码几乎全部过时）：
//
//	                文档 V1.0            真实服务器（本实现依据）
//	路径            /c/tax（相对）        baseURL 即完整地址，直接 POST
//	鉴权            无                    请求头 X-AppId + X-Token（缺失报 A20029）
//	业务入参        {param:{identity,     顶层平铺 {identityId, type, authTimeBeg,
//	                 type}}               authTimeEnd, authCode}（缺任一报 A03001）
//	返回码          SYS200/SYS404/500     数字/字母码，如 200/203/A03001；带 orderNo
//
// 请求  POST <baseURL>   application/json; charset=UTF-8
//
//	Header: X-AppId / X-Token
//	Body:   {"identityId","type","authTimeBeg","authTimeEnd","authCode"}
//
// 应答  {"code":"...","msg":"...","orderNo":"...","data":{...}}
//
// authCode 与 authTime 是上游要求的**授权信息**（企业授权数据查询的凭证与时间窗），
// 下游 querySrmxSWFP 不带这些参数，故由本源的配置提供：authCode 是环境级凭证
// (test/gckj)，authTime 窗口默认取「2020-01-01 ~ 当天」。
//
// 返回码（2026-09-13 测试环境实测，与 SYS200/… 文档口径部分吻合、部分不符）：
//   - SYS200 / 200 有 data      → 查得（本实现以「有业务段」而非仅看码判成功，防码不准）
//   - SYS404「未查得数据」        → 查无（归一 999，寻源器静默回落其余源）
//   - 203「授权信息校验失败」     → 该源失败（error）。这是配置/授权问题（authCode 错、
//     授权时间窗不合法、企业未授权等），**故意不当查无**：当成查无会把「凭证配错、
//     源6 形同虚设」悄悄掩盖掉；报 error 才能在审计里暴露出来。实测同一税号，窗口过
//     窄/过近会回 203，取足够宽的默认窗（2020-01-01~当天）则回 SYS404，故默认取宽窗。
const (
	ctaxServiceCode = "cTax" // 文档 §3 服务代码（备查；真实报文里不出现）

	// type 入参 (文档 §3，真实服务器沿用)。
	ctaxTypeTax     = "1" // 仅税务
	ctaxTypeInvoice = "2" // 仅发票
	ctaxTypeBoth    = "3" // 税务 + 发票

	// authTimeBeg 缺省起始日（授权时间窗下界）。授权窗口只是查询过滤，取一个足够早
	// 的日期即「尽量多给」；authTimeEnd 缺省取当天。两者都可由配置覆盖。
	ctaxDefaultAuthTimeBeg = "2020-01-01"
	ctaxDateLayout         = "2006-01-02"

	// 真实服务器的鉴权头（联调实测：缺失报 A20029「请检查X-AppId，X-Token」）。
	ctaxHeaderAppID = "X-AppId"
	ctaxHeaderToken = "X-Token"
)

// data 节点里按维度归属的业务段 (文档 表1)。**nsrjbxx 不在其中**：纳税人基本信息
// 是两个维度共有的，只回了它不足以证明"查得了某个维度"，把它算进实得维度会让
// 一次没有任何业务数据的应答也被计费。
var (
	ctaxInvoiceSections = []string{"kphzxx", "spxx", "xyhzxx", "khxsdq", "syhzxx"} // 表4/表6/表11/表12/表13
	ctaxTaxSections     = []string{"sbsj", "zsbxx", "lrbxx", "zcfzbxx"}            // 表2/表3/表7/表9
)

// CTaxConfig holds the 税票数据查询C endpoint + 凭证 + 授权信息。
// BaseURL 是上游给出的**完整**业务地址（含 /hzservice/sy/tax），直接 POST，不再拼路径。
type CTaxConfig struct {
	BaseURL     string // 完整业务地址（生产/测试各一）
	AppID       string // 请求头 X-AppId
	Token       string // 请求头 X-Token
	AuthCode    string // body authCode：授权码（环境级凭证 test/gckj）
	AuthTimeBeg string // body authTimeBeg：授权时间窗下界（缺省 2020-01-01）
	AuthTimeEnd string // body authTimeEnd：授权时间窗上界（缺省当天）
}

// CTaxClient implements port.UpstreamPort for 税票数据查询C。
// 归一：查得 → "001"（Range = data 原样，Got = 本次实得维度）；查无 → "999"；
// 其余返回码（含 203 授权校验失败）→ *model.UpstreamError（该源失败，不计费，
// 带 orderNo 供复查/对账）。
//
// Got 是本源存在的**关键**：作为综合源，它一次调用可能只拿到其中一个维度（如只有
// 申报/征收数据、没有开票数据），而计费只看实得维度——若沿用配置里静态的
// provides=both，请求两项只拿到税务时会按【发票+税务】档收费，即向客户收没给到的
// 数据的钱。寻源器优先采信这里回填的 Got（见 sourcing.go 的 effectiveDims）。
type CTaxClient struct {
	cfg  CTaxConfig
	http *http.Client
}

// NewCTax builds a 税票数据查询C client。
func NewCTax(cfg CTaxConfig, httpClient *http.Client) *CTaxClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &CTaxClient{cfg: cfg, http: httpClient}
}

// ctaxRequest 是业务请求体（真实服务器要求的平铺形态，字段名以联调报错为准）。
type ctaxRequest struct {
	IdentityID  string `json:"identityId"`
	Type        string `json:"type"`
	AuthTimeBeg string `json:"authTimeBeg"`
	AuthTimeEnd string `json:"authTimeEnd"`
	AuthCode    string `json:"authCode"`
}

// ctaxResponse 是应答（真实服务器形态：平铺 + orderNo，小写字段）。data 结构见文档
// 表1，由契约层按 xlsx 白名单映射，本客户端不裁剪字段。
type ctaxResponse struct {
	Code    string          `json:"code"`
	Msg     string          `json:"msg"`
	OrderNo string          `json:"orderNo"`
	Data    json.RawMessage `json:"data"`
}

// 真实返回码（联调实测）。文档 V1.0 的 SYS200/SYS404/500 已过时，一并兼容以防不同
// 环境口径不一：成功以「有 data」为准（见 Query），查无与失败按码归类。
var (
	ctaxSuccessCodes = map[string]bool{"200": true, "0": true, "000000": true, "SYS200": true}
	ctaxEmptyCodes   = map[string]bool{"404": true, "A03002": true, "SYS404": true} // 查无（无授权数据/无结果）
)

// ctaxTypeOf 把本次请求维度映射为 type 入参。只要要的那一维：多要一维就多付一次
// 上游的钱，而多出来的数据既不透出下游也不计费。
func ctaxTypeOf(want model.DimSet) string {
	switch {
	case want.Both():
		return ctaxTypeBoth
	case want.Tax:
		return ctaxTypeTax
	default:
		return ctaxTypeInvoice
	}
}

// Query 调 c/tax 一次并归一。
func (c *CTaxClient) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	want := req.Want
	if want.Empty() {
		want = model.AllDims() // 老下游不带 dataType：按两项都要处理
	}

	res, err := c.call(ctx, req.CreditCode, ctaxTypeOf(want), req.Reqid)
	if err != nil {
		return nil, err
	}

	// orderNo 是上游订单号，作为 UID/LogID 落审计供对账（成功/失败都要带）。
	switch {
	case ctaxHasData(res.Data):
		got := ctaxGot(res.Data).Intersect(want)
		if got.Empty() {
			// 有 data 容器但本次请求维度下没有任何业务段（如只回了 nsrjbxx）：
			// 与查无同口径，绝不能当查得计费。
			slog.Info("ctax 应答有 data 但无业务数据段", "reqid", req.Reqid, "want", want.String(), "code", res.Code)
			return &model.UpstreamResult{Code: "999", Msg: "查无结果", UID: res.OrderNo, LogID: res.OrderNo, Reqid: req.Reqid}, nil
		}
		return &model.UpstreamResult{
			Code: "001", Msg: "成功", UID: res.OrderNo, LogID: res.OrderNo, Reqid: req.Reqid,
			Range: string(res.Data), Got: got,
		}, nil
	case ctaxEmptyCodes[res.Code] || (ctaxSuccessCodes[res.Code] && len(res.Data) == 0):
		// 明确的查无码，或成功码但无 data。
		return &model.UpstreamResult{Code: "999", Msg: "查无结果", UID: res.OrderNo, LogID: res.OrderNo, Reqid: req.Reqid}, nil
	default:
		// 其余（203 授权校验失败、A03001 参数缺失、A20029 鉴权失败等）：该源失败，
		// 带上游码与订单号可追查。
		return nil, busiErr(res.Code, fmt.Sprintf("%s 失败: %s", ctaxServiceCode, res.Msg), res.OrderNo, res.OrderNo)
	}
}

// call 发一次请求并解出应答。
func (c *CTaxClient) call(ctx context.Context, identity, queryType, reqid string) (*ctaxResponse, error) {
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		return nil, fmt.Errorf("ctax baseURL 未配置")
	}
	if c.cfg.AppID == "" || c.cfg.Token == "" {
		// 鉴权头缺失上游必报 A20029，本地先拦下，避免发出注定失败的请求。
		return nil, fmt.Errorf("ctax 凭证不完整: appId/token 必填")
	}

	begin := c.cfg.AuthTimeBeg
	if begin == "" {
		begin = ctaxDefaultAuthTimeBeg
	}
	end := c.cfg.AuthTimeEnd
	if end == "" {
		end = time.Now().Format(ctaxDateLayout)
	}

	payload, err := json.Marshal(ctaxRequest{
		IdentityID:  identity,
		Type:        queryType,
		AuthTimeBeg: begin,
		AuthTimeEnd: end,
		AuthCode:    c.cfg.AuthCode,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ctax request: %w", err)
	}

	fullURL := strings.TrimRight(c.cfg.BaseURL, "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build ctax request: %w", err)
	}
	// 文档 §1.3 + §2.2：POST + UTF-8。鉴权走请求头（联调实测 X-AppId/X-Token）。
	httpReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	httpReq.Header.Set(ctaxHeaderAppID, c.cfg.AppID)
	httpReq.Header.Set(ctaxHeaderToken, c.cfg.Token)

	slog.Debug("ctax request", "url", fullURL, "type", queryType, "reqid", reqid)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ctax call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read ctax body: %w", err)
	}
	slog.Debug("ctax response", "status", resp.StatusCode, "len", len(raw), "reqid", reqid)

	res, err := decodeCTaxBody(raw)
	if err != nil {
		return nil, fmt.Errorf("%w (http %d)", err, resp.StatusCode)
	}
	return res, nil
}

// decodeCTaxBody 解出应答。真实服务器是平铺 {code,msg,orderNo,data}；文档旧版还写过
// 外层包一层 {"result":{...}} 的形态，一并兼容（挑一种赌等于赌联调失败）。
func decodeCTaxBody(raw []byte) (*ctaxResponse, error) {
	var flat ctaxResponse
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Code != "" {
		return &flat, nil
	}
	var wrapped struct {
		Result *ctaxResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Result != nil && wrapped.Result.Code != "" {
		return wrapped.Result, nil
	}
	return nil, fmt.Errorf("ctax 应答无法解析或缺少 code: %s", truncate(string(raw), 200))
}

// truncate 截断日志/错误里透出的报文，避免整包灌进错误串。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ctaxHasData 报告 data 节点是否带回了业务容器（非 null/空）。
func ctaxHasData(data json.RawMessage) bool {
	switch s := strings.TrimSpace(string(data)); s {
	case "", "null", "{}", "[]", `""`:
		return false
	default:
		return true
	}
}

// ctaxGot 判定应答的 data 里实际带回了哪些维度：某维度只要有一个业务段非空即算
// 查得。段存在但为 null/[]/{} 不算——上游对没有的数据常回空容器而非省略字段，
// 把空容器当查得会导致"计了费却没有数据"。
func ctaxGot(data json.RawMessage) model.DimSet {
	if !ctaxHasData(data) {
		return model.DimSet{}
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		return model.DimSet{}
	}
	has := func(names []string) bool {
		for _, n := range names {
			if ctaxSectionFilled(sections[n]) {
				return true
			}
		}
		return false
	}
	return model.DimSet{
		Invoice: has(ctaxInvoiceSections),
		Tax:     has(ctaxTaxSections),
	}
}

// ctaxSectionFilled 报告一个业务段是否带回了数据。
func ctaxSectionFilled(raw json.RawMessage) bool {
	switch s := strings.TrimSpace(string(raw)); s {
	case "", "null", "[]", "{}", `""`:
		return false
	default:
		return true
	}
}

// Requery: 文档未提供对账/复查接口，返回 Reachable=false 让台账保持 PENDING
// 由对账兜底（与其余上游一致）。
func (c *CTaxClient) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	_ = ctx
	_ = reqid
	return &model.RequeryResult{Reachable: false}, nil
}
