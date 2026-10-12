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

// 税友 sy（源7，惠众征信 /hzservice/sy/tax）：切到 c/tax 之前源6 实际在用的接口。
// 它与 c/tax 同属一个惠众账号（appId 9VTYC6YU），但按主体计费、价格高；对账单里已计过
// 费的 (企业, 维度) 再调不产生新费用，故只作为**白名单管控**的源接回（见 syallow.go）。
// 本客户端只管协议，是否允许调用由寻源器的白名单阶段 + GatedPort 两道闸决定。
//
// 请求  POST <baseURL>   application/json; charset=UTF-8
//
//	baseURL: 完整业务地址，如 https://api.huizhongcredit.com/hzservice/sy/tax
//	Header:  X-AppId / X-Token（缺失报 A20029）
//	Body:    {"identityId","type","authTimeBeg","authTimeEnd","authCode"}（缺任一报 A03001）
//
// 应答  {"code":"...","msg":"...","data":{...}}（或外包 result），data 结构见
// 【税票数据查询-包年】接口文档 表1：success/nsrsbh/djxh/... + 各 *List 业务段，
// 与 c/tax 的段名、字段名都不同，契约映射见 sycontract.go。
//
// 判定（与 ctax 的区别：**不做业务段为空的过滤**——白名单企业都已验证过能查得）：
//   - 成功码 (200/SYS200/…) 且 data 容器非空 → 查得，Got = 本次请求的维度
//   - 查无码 (404/SYS404/…)，或成功码但 data 为空/null，或 data.success=false → 查无 (999)
//   - 其余（203 授权信息校验失败、A03001、A20029 等）→ 该源失败
const (
	syServiceCode        = "sy"
	syDefaultAuthTimeBeg = "2020-01-01"
	syDateLayout         = "2006-01-02"
)

// data 节点里按维度归属的业务段（包年文档 表1）。qyxxList 两维共有，不参与裁剪。
var (
	syInvoiceSections = []string{"kphzxxList", "syKphzxxList", "xyKphzxxList", "khxsdqxxList"} // 表3/表4/表5/表6
	syTaxSections     = []string{"sbxxList", "zsxxList", "cwbbZbList"}                         // 表7/表8/表9
)

// SyConfig holds the sy endpoint + 凭证 + 授权信息。
type SyConfig struct {
	BaseURL     string // 完整业务地址（含 /hzservice/sy/tax），直接 POST
	AppID       string // 请求头 X-AppId
	Token       string // 请求头 X-Token
	AuthCode    string // body authCode：授权码（生产 gckj）
	AuthTimeBeg string // body authTimeBeg：授权时间窗下界（缺省 2020-01-01）
	AuthTimeEnd string // body authTimeEnd：授权时间窗上界（缺省当天）
}

// SyClient implements port.UpstreamPort for 税友 sy。
type SyClient struct {
	cfg  SyConfig
	http *http.Client
}

// NewSy builds a sy client。
func NewSy(cfg SyConfig, httpClient *http.Client) *SyClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &SyClient{cfg: cfg, http: httpClient}
}

type syRequest struct {
	IdentityID  string `json:"identityId"`
	Type        string `json:"type"`
	AuthTimeBeg string `json:"authTimeBeg"`
	AuthTimeEnd string `json:"authTimeEnd"`
	AuthCode    string `json:"authCode"`
}

// Query 调 sy 一次并归一。Want 必须由白名单阶段给定：为空时拒绝，绝不按「两项都要」
// 兜底（那等于替客户多买一个维度）。
func (c *SyClient) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	want := req.Want
	if want.Empty() {
		return nil, fmt.Errorf("sy 请求维度为空，拒绝调用")
	}

	res, err := c.call(ctx, req.CreditCode, ctaxTypeOf(want), req.Reqid)
	if err != nil {
		return nil, err
	}

	switch {
	case ctaxSuccessCodes[res.Code] && ctaxHasData(res.Data) && !syDataFailed(res.Data):
		return &model.UpstreamResult{
			Code: "001", Msg: "成功", UID: res.OrderNo, LogID: res.OrderNo, Reqid: req.Reqid,
			Range: string(syPrune(res.Data, want)), Got: want,
		}, nil
	case ctaxEmptyCodes[res.Code] || ctaxSuccessCodes[res.Code]:
		return &model.UpstreamResult{Code: "999", Msg: "查无结果", UID: res.OrderNo, LogID: res.OrderNo, Reqid: req.Reqid}, nil
	default:
		return nil, busiErr(res.Code, fmt.Sprintf("%s 失败: %s", syServiceCode, res.Msg), res.OrderNo, res.OrderNo)
	}
}

func (c *SyClient) call(ctx context.Context, identity, queryType, reqid string) (*ctaxResponse, error) {
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		return nil, fmt.Errorf("sy baseURL 未配置")
	}
	if c.cfg.AppID == "" || c.cfg.Token == "" {
		return nil, fmt.Errorf("sy 凭证不完整: appId/token 必填")
	}

	begin := c.cfg.AuthTimeBeg
	if begin == "" {
		begin = syDefaultAuthTimeBeg
	}
	end := c.cfg.AuthTimeEnd
	if end == "" {
		end = time.Now().Format(syDateLayout)
	}

	payload, err := json.Marshal(syRequest{
		IdentityID:  identity,
		Type:        queryType,
		AuthTimeBeg: begin,
		AuthTimeEnd: end,
		AuthCode:    c.cfg.AuthCode,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal sy request: %w", err)
	}

	fullURL := strings.TrimRight(c.cfg.BaseURL, "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build sy request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	httpReq.Header.Set(ctaxHeaderAppID, c.cfg.AppID)
	httpReq.Header.Set(ctaxHeaderToken, c.cfg.Token)

	slog.Debug("sy request", "url", fullURL, "type", queryType, "reqid", reqid)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sy call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read sy body: %w", err)
	}
	slog.Debug("sy response", "status", resp.StatusCode, "len", len(raw), "reqid", reqid)

	res, err := decodeCTaxBody(raw)
	if err != nil {
		return nil, fmt.Errorf("%w (http %d)", err, resp.StatusCode)
	}
	return res, nil
}

// syPrune 删掉 data 里不属于本次请求维度的业务段：没向 sy 要（也不计费）的维度，
// 即便上游多回了也不能透给下游。非对象形态原样返回。
func syPrune(data json.RawMessage, want model.DimSet) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	if !want.Invoice {
		for _, k := range syInvoiceSections {
			delete(m, k)
		}
	}
	if !want.Tax {
		for _, k := range syTaxSections {
			delete(m, k)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// syDataFailed 报告 data.success 是否明确为 false（数据成功标志，包年文档 表1）。
// 字段缺失或非布尔时不据此判查无，仍按 200 + 非空 data 算查得。
func syDataFailed(data json.RawMessage) bool {
	var flag struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(data, &flag); err != nil || flag.Success == nil {
		return false
	}
	return !*flag.Success
}

// Requery: sy 无复查接口，返回 Reachable=false 让台账保持 PENDING 由对账兜底。
func (c *SyClient) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	return &model.RequeryResult{Reachable: false}, nil
}
