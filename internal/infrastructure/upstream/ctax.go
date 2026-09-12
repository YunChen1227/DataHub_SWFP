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

	"github.com/datahub/relay/internal/domain/model"
)

// 税票数据查询C 上游 (docs/【税票数据查询c】接口文档.docx V1.0)：本仓第一个
// **综合源**——一次调用即可同时给出税务与发票两个维度，由入参 type 选择
// (1 仅税务 / 2 仅发票 / 3 两者)。
//
// 协议按该文档实现，**不得**照搬其余源的形态（证通 entcredit 是 HMAC-SHA256 + 表单
// 提交 + 产品码；源5 凯盈云是 AES + 加密信封 + 接口名走 URL 路径）：
//
//	请求  POST <baseURL>/c/tax   application/json; charset=UTF-8
//	      {"param": {"identity": "<统一社会信用代码>", "type": "3"}}
//	应答  {"result": {"code": "SYS200", "msg": "...", "data": {...}}}
//
// 文档 §2.1「接口说明」在 V1.0 里是空白的：没有给出接口域名、鉴权/加签/加密方式，
// 也没有报文示例，仅有「入参 param」「出参 result」两张字段表。故信封字段名取这两
// 张表的表头，且**应答的两种形态都兼容**（外层带 result 节 / 直接就是 result 本体）
// ——没有示例可依时兼容两种好过挑一种赌（源5 文档自相矛盾的字段拼写同此处理）。
// 联调若报鉴权失败，以上游服务器的报错为准在此补鉴权字段，不要凭猜想预先加。
const (
	ctaxServiceCode = "cTax"   // 文档 §3 服务代码（备查；裸 JSON 信封里不出现）
	ctaxPath        = "/c/tax" // 文档 §3 接口地址（相对 baseURL）

	// 文档 §3 入参表 type 取值。
	ctaxTypeTax     = "1" // 仅税务
	ctaxTypeInvoice = "2" // 仅发票
	ctaxTypeBoth    = "3" // 税务 + 发票

	// 文档 §4 系统返回码。
	ctaxCodeOK    = "SYS200" // 查询成功
	ctaxCodeEmpty = "SYS404" // 查无数据
	ctaxCodeError = "500"    // 异常
)

// data 节点里按维度归属的业务段 (文档 表1)。**nsrjbxx 不在其中**：纳税人基本信息
// 是两个维度共有的，只回了它不足以证明"查得了某个维度"，把它算进实得维度会让
// 一次没有任何业务数据的应答也被计费。
var (
	ctaxInvoiceSections = []string{"kphzxx", "spxx", "xyhzxx", "khxsdq", "syhzxx"} // 表4/表6/表11/表12/表13
	ctaxTaxSections     = []string{"sbsj", "zsbxx", "lrbxx", "zcfzbxx"}            // 表2/表3/表7/表9
)

// CTaxConfig holds the 税票数据查询C endpoint。BaseURL 只含 scheme+host(:port)
// 与可能的前缀，调用时追加 /c/tax。文档未定义任何鉴权凭证，故此处也没有。
type CTaxConfig struct {
	BaseURL string
}

// CTaxClient implements port.UpstreamPort for 税票数据查询C。
// 归一：查得 → "001"（Range = data 原样，Got = 本次实得维度）；查无 → "999"；
// 其余返回码 → *model.UpstreamError（该源失败，不计费，走复查/对账）。
//
// Got 是本源存在的**关键**：作为综合源，它一次调用可能只拿到其中一个维度（如只有
// 申报/征收数据、没有开票数据），而计费只看实得维度——若沿用配置里静态的
// provides=both，请求两项只拿到税务时会按【发票+税务】档收费，即向客户收没给到的
// 数据的钱。寻源器优先采信这里回填的 Got（见 sourcing.go 的 invoke）。
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

// ctaxEnvelope 是请求报文 (文档 §3「入参 param」)。
type ctaxEnvelope struct {
	Param ctaxParam `json:"param"`
}

// ctaxParam 是业务入参：identity 为社会信用代码，type 为查询类型 (1/2/3)，两者必填。
type ctaxParam struct {
	Identity string `json:"identity"`
	Type     string `json:"type"`
}

// ctaxResult 是应答的 result 节 (文档 §3「出参 result」)。data 结构见文档 表1，
// 由契约层按 xlsx 白名单映射，本客户端不裁剪字段。
type ctaxResult struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// ctaxTypeOf 把本次请求维度映射为文档 §3 的 type 入参。只要要的那一维：多要一维
// 就多付一次上游的钱，而多出来的数据既不透出下游也不计费。
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

	// 上游应答无订单号/请求号字段（文档 出参表只有 code/msg/data），UID/LogID
	// 无可填——非漏填。
	switch res.Code {
	case ctaxCodeOK:
		got := ctaxGot(res.Data).Intersect(want)
		if got.Empty() {
			// SYS200 但本次请求的维度下没有任何业务段（如只回了 nsrjbxx）：
			// 与查无同口径，绝不能当查得计费。
			slog.Info("ctax 应答成功但无业务数据段", "reqid", req.Reqid, "want", want.String())
			return &model.UpstreamResult{Code: "999", Msg: "查无结果", Reqid: req.Reqid}, nil
		}
		return &model.UpstreamResult{
			Code: "001", Msg: "成功", Reqid: req.Reqid, Range: string(res.Data), Got: got,
		}, nil
	case ctaxCodeEmpty:
		return &model.UpstreamResult{Code: "999", Msg: "查无结果", Reqid: req.Reqid}, nil
	default:
		// 500 异常及后续扩展返回码：该源失败（已应答，带业务码）。
		return nil, busiErr(res.Code, fmt.Sprintf("%s 失败: %s", ctaxServiceCode, res.Msg), "", "")
	}
}

// call 发一次 c/tax 请求并解出 result 节。
func (c *CTaxClient) call(ctx context.Context, identity, queryType, reqid string) (*ctaxResult, error) {
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		return nil, fmt.Errorf("ctax baseURL 未配置")
	}
	payload, err := json.Marshal(ctaxEnvelope{Param: ctaxParam{Identity: identity, Type: queryType}})
	if err != nil {
		return nil, fmt.Errorf("marshal ctax param: %w", err)
	}

	fullURL := strings.TrimRight(c.cfg.BaseURL, "/") + ctaxPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build ctax request: %w", err)
	}
	// 文档 §1.3 + §2.2：POST + UTF-8。
	httpReq.Header.Set("Content-Type", "application/json; charset=UTF-8")

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

// decodeCTaxBody 解出 result 节。文档没有报文示例，故两种形态都认：
// 外层包一层 {"result":{...}}，或直接就是 result 本体 {"code":...}。
func decodeCTaxBody(raw []byte) (*ctaxResult, error) {
	var wrapped struct {
		Result *ctaxResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Result != nil && wrapped.Result.Code != "" {
		return wrapped.Result, nil
	}
	var flat ctaxResult
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("decode ctax result: %w", err)
	}
	if flat.Code == "" {
		return nil, fmt.Errorf("ctax 应答缺少 code")
	}
	return &flat, nil
}

// ctaxGot 判定应答的 data 里实际带回了哪些维度：某维度只要有一个业务段非空即算
// 查得。段存在但为 null/[]/{} 不算——上游对没有的数据常回空容器而非省略字段，
// 把空容器当查得会导致"计了费却没有数据"。
func ctaxGot(data json.RawMessage) model.DimSet {
	if len(data) == 0 {
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
