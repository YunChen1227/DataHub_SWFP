package upstream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/datahub/relay/internal/domain/model"
)

// 证通 entcreditapi 平台四个产品码 (docs/发票数据聚合查询-part1/2.pdf 与
// docs/税务数据聚合查询-part1/2.pdf)，共用同一端点
// /ectcispserver/api/entcreditapi/query。协议/鉴权见 docs/java-api-demo 官方示例：
// version/msgId/orgCode/accessKeyId/timestamp/args/signature 表单提交，
// HMAC-SHA256 签名 (SignedRequestsHelper.java)。
const (
	entCreditInvoice1 = "P0130081" // 发票数据聚合-part1
	entCreditInvoice2 = "P0130083" // 发票数据聚合-part2
	entCreditTax1     = "P0130082" // 税务数据聚合-part1
	entCreditTax2     = "P0130084" // 税务数据聚合-part2

	entCreditRequestURI = "/ectcispserver/api/entcreditapi/query"
	entCreditVersion    = "1.0"
	entCreditQueryMode  = "0"
)

// entCreditSectionKey maps 产品码 → range 合并 JSON 里的段名。
var entCreditSectionKey = map[string]string{
	entCreditInvoice1: "invoice1",
	entCreditInvoice2: "invoice2",
	entCreditTax1:     "tax1",
	entCreditTax2:     "tax2",
}

// entCreditDuplicates 记录与 part2 返回同一份数据的 part1 产品码（文档样例解码后逐字节
// 相同），值为保留调用的 part2 产品码。part1 只会重复付费，装配时一律丢弃。
var entCreditDuplicates = map[string]string{
	entCreditInvoice1: entCreditInvoice2,
	entCreditTax1:     entCreditTax2,
}

// EntCreditDuplicateOf 判断产品码是否为不再调用的 part1 重复产品；是则返回保留调用的
// part2 产品码。
func EntCreditDuplicateOf(product string) (string, bool) {
	kept, ok := entCreditDuplicates[product]
	return kept, ok
}

// msgSeq 提供 msgId 的 6 位序号（进程内原子自增，启动时以纳秒时间播种，
// 保证并发子请求与同秒连续请求的 msgId 互不相同，满足平台唯一性要求）。
var msgSeq = func() *atomic.Uint64 {
	var v atomic.Uint64
	v.Store(uint64(time.Now().UnixNano()))
	return &v
}()

// EntCreditConfig holds the 证通 entcreditapi 平台的 endpoint + 我方凭证 + 本子源
// 负责的单个产品码。Endpoint 只含 scheme+host(:port)，不含 requestUri（requestUri
// 固定，签名时需要把它与 endpoint 分开参与拼接，见 sign()）。swfp 的"四产品聚合"由
// 上层 Aggregator 并发调 4 个各自 Product 不同的 EntCreditClient 实现——本客户端只
// 负责一个产品码。
type EntCreditConfig struct {
	Endpoint        string // 如 https://cisp.zenitera.com（不同环境域名不同，见官方 demo 注释）
	OrgCode         string // 机构代码
	AccessKeyID     string // AK
	SecretAccessKey string // SK（Base64 编码，签名时先 Base64 解码取原始字节）
	Product         string // 本子源的产品码 (P0130081/83/82/84)
}

// EntCreditClient implements port.UpstreamPort for 证通 entcreditapi 的单个产品码：
// 查得 → "001"(Range=解码后的明细)；查无 → "999"；账户/参数/系统错误 → error。
// 多产品聚合 (001/999/002/error 判定 + range 分段合并) 由上层 upstream.Aggregator 承接。
type EntCreditClient struct {
	cfg  EntCreditConfig
	http *http.Client
}

// EntCreditLabel 返回产品码在聚合 range 里的缺省段名 (invoice1/invoice2/tax1/tax2)；
// 未知产品码回退为产品码本身。供装配层在 config 未显式指定 label 时缺省使用。
func EntCreditLabel(product string) string {
	if k := entCreditSectionKey[product]; k != "" {
		return k
	}
	return product
}

// NewEntCredit builds a 证通 entcreditapi 单产品 client.
// 官方 demo 的 getSingleSSLConnection() 关闭证书校验；部分环境用自签证书，保留
// 同等行为以避免联调时握手失败。实现上**不得修改传入的共享 client**（旧版直接改
// 共享 Transport，会把 InsecureSkipVerify 泄漏给所有路由）：复制 client + clone
// Transport（继承连接池参数）后仅在副本上关闭证书校验。
func NewEntCredit(cfg EntCreditConfig, httpClient *http.Client) *EntCreditClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	base, ok := httpClient.Transport.(*http.Transport)
	if !ok || base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	tr := base.Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{} //nolint:gosec
	}
	tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
	own := *httpClient
	own.Transport = tr
	return &EntCreditClient{cfg: cfg, http: &own}
}

// entCreditSection is 单产品调用的归一中间结果 (callProduct 返回)。
type entCreditSection struct {
	Status string          // ok=查得 / empty=查无 / error=该源失败
	Raw    string          // 上游状态码原值 (备查)
	Data   json.RawMessage // 解码后的明细 JSON
	Error  string          // status=error 时的原因摘要
}

// Query 调本子源的单个产品码并归一：查得→001(Range=明细)、查无→999、失败→error。
func (c *EntCreditClient) Query(ctx context.Context, req *model.UpstreamRequest) (*model.UpstreamResult, error) {
	sec, orderNo := c.callProduct(ctx, c.cfg.Product, req)
	switch sec.Status {
	case "ok":
		// 只有 orderNo 一个上游标识，UID/LogID 同填供后台「上游logId」对账。
		return &model.UpstreamResult{Code: "001", Msg: "成功", UID: orderNo, Reqid: req.Reqid, LogID: orderNo, Range: string(sec.Data)}, nil
	case "empty":
		return &model.UpstreamResult{Code: "999", Msg: "查无结果", UID: orderNo, Reqid: req.Reqid, LogID: orderNo}, nil
	default:
		// 失败也带上游状态码原值(sec.Raw)与订单号(orderNo)落审计供对账追查。
		// 上游只有 orderNo 一个标识，UID/LogID 同填——绝不能让「上游logId」列为空。
		return nil, busiErr(sec.Raw, fmt.Sprintf("产品 %s 失败: %s", c.cfg.Product, sec.Error), orderNo, orderNo)
	}
}

// entCreditResponse mirrors 平台响应外层字段 (docs/java-api-demo Main.java +
// 附录错误码表)。resultCode "00000" = 成功；resultData 结构随产品而异，透出原样
// json.RawMessage，段级判定看 <产品码>Status (4=有结果/1=无结果/其余=异常)。
type entCreditResponse struct {
	OrderNo    string          `json:"orderNo"`
	ResultData json.RawMessage `json:"resultData"`
	ResultCode string          `json:"resultCode"`
	ResultDesc string          `json:"resultDesc"`
}

// callProduct 调一个产品码并归一为 section，严格对齐 docs/java-api-demo 官方示例：
//   - 待签名串 = POST\n<endpoint>\n<requestUri>\n<version>\n<msgId>\n<orgCode>\n
//     <accessKeyId>\n<timestamp>\n<args>（args 参与签名时不做 URLEncode）
//   - HMAC-SHA256，密钥为 secretAccessKey 的 Base64 解码结果；签名结果先 Base64
//     编码，再对整体做一次 URLEncode（SignedRequestsHelper.hmac + percentEncodeRfc3986）
//   - 报文以 application/x-www-form-urlencoded 提交；args 字段在放入表单前也需
//     URLEncode（Main.java 注释：加签时不 encode，放入报文时才 encode）
//
// creditCode 是本路由的业务查询参数（统一社会信用代码），与 prodCode 一起放进
// args JSON——官方 demo 文档给的示例字段名是 entInfo，但 2026-07-08 用真实凭证
// 联调时上游直接报 "E1000 查询参数校验不通过,creditCode:为必填项"，证明四产品
// 聚合接口的真实参数名是 creditCode，以服务器报错为准。
func (c *EntCreditClient) callProduct(ctx context.Context, product string, req *model.UpstreamRequest) (entCreditSection, string) {
	fail := func(msg string) (entCreditSection, string) {
		return entCreditSection{Status: "error", Error: msg}, ""
	}

	// msgId 格式为「机构代码+8位日期+6位序号」且必须全局唯一（文档 E1006：重复提交
	// 导致 msgId 重复会被拒）。官方 demo 用 HHmmss 充当 6 位序号——但本客户端四个
	// 产品码并发出请求，同一秒内会产生相同 msgId，故序号改用进程内原子自增
	// （以启动纳秒时间播种，降低跨进程/重启撞号概率）。timestamp 字段仍用真实时间
	// （E1014：与服务器时差超 5 分钟被拒）。
	timestamp := time.Now().Format("20060102150405")
	msgID := c.cfg.OrgCode + time.Now().Format("20060102") + fmt.Sprintf("%06d", msgSeq.Add(1)%1000000)

	args, err := json.Marshal(map[string]string{
		"prodCode":   product,
		"creditCode": req.CreditCode,
	})
	if err != nil {
		return fail("marshal args: " + err.Error())
	}

	sig, err := c.sign(entCreditRequestURI, msgID, timestamp, string(args))
	if err != nil {
		return fail("sign: " + err.Error())
	}

	// 官方 demo（Main.java 注释"特别注意"）里 args 与 signature 都会被 URLEncode 两次：
	//   - args：业务 JSON（原始未编码）→ 手动 URLEncoder.encode() 一次 → 表单再编码一次。
	//   - signature：sign() 内部（percentEncodeRfc3986）已编码一次 → 直接放入表单，
	//     表单编码是第二次——sig 变量本身不能再手动编码，否则变成三次。
	// url.Values.Encode() 只编码一次，故 args 需要先手动 QueryEscape 补上第一次；
	// signature 的"第一次"已经在 sign() 内完成，这里直接 Set 走表单的第二次编码。
	form := url.Values{}
	form.Set("version", entCreditVersion)
	form.Set("msgId", msgID)
	form.Set("orgCode", c.cfg.OrgCode)
	form.Set("accessKeyId", c.cfg.AccessKeyID)
	form.Set("transTime", timestamp)
	form.Set("timestamp", timestamp)
	form.Set("args", url.QueryEscape(string(args)))
	form.Set("signature", sig)
	form.Set("queryMode", entCreditQueryMode)

	fullURL := strings.TrimRight(c.cfg.Endpoint, "/") + entCreditRequestURI
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fail("build request: " + err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	slog.Debug("entcredit request", "url", fullURL, "prodCode", product, "msgId", msgID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fail("call: " + err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fail("read body: " + err.Error())
	}
	slog.Debug("entcredit response", "status", resp.StatusCode, "raw", string(raw))

	var er entCreditResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return fail(fmt.Sprintf("decode body (http %d): %s", resp.StatusCode, err.Error()))
	}
	// 上游已应答（无论成功/业务失败）后的失败也要把 orderNo(上游订单号)带出，
	// 供聚合器与审计对账追查——绝不能像纯网络失败那样丢标识（失败也要可追查·铁律）。
	failResp := func(raw, msg string) (entCreditSection, string) {
		return entCreditSection{Status: "error", Raw: raw, Error: msg}, er.OrderNo
	}

	// 附录错误码表：00000 = 查询成功；其余 (E0001-E1027) 均视为该数据源失败。
	if er.ResultCode != "00000" {
		return failResp(er.ResultCode, fmt.Sprintf("resultCode=%s desc=%s", er.ResultCode, er.ResultDesc))
	}

	// resultData 结构随产品码变化 (<产品码>Status / <产品码>Data)；状态码
	// 4=查询成功有结果 / 1=查询成功无结果 / 3=查询失败 (docs 附录"状态码"表)。
	var resultData map[string]json.RawMessage
	if err := json.Unmarshal(er.ResultData, &resultData); err != nil {
		return failResp(er.ResultCode, "decode resultData: "+err.Error())
	}
	var status string
	if s, ok := resultData[product+"Status"]; ok {
		_ = json.Unmarshal(s, &status)
	}
	switch status {
	case "4":
		dataNode, ok := resultData[product+"Data"]
		if !ok || len(dataNode) == 0 {
			return failResp(status, "状态码4但缺少 "+product+"Data")
		}
		// <产品码>Data.result[].data 是 base64 编码的明细 JSON（PDF 样例展示）。
		// 解开 base64 后透出明细本体，客户拿到即可用（单条→对象，多条→数组）。
		decoded, err := decodeEntCreditData(dataNode)
		if err != nil {
			return failResp(status, "解码明细失败: "+err.Error())
		}
		return entCreditSection{Status: "ok", Raw: status, Data: decoded}, er.OrderNo
	case "1":
		return entCreditSection{Status: "empty", Raw: status}, er.OrderNo
	default:
		return failResp(status, fmt.Sprintf("状态码=%s (预期4/1)", status))
	}
}

// decodeEntCreditData 把 <产品码>Data 节点里 result[].data 的 base64 串解成明细
// JSON：单条返回对象本体，多条返回数组。异常（非法 base64/非法 JSON）报错，由
// 调用方把该段归一为 error。
func decodeEntCreditData(dataNode json.RawMessage) (json.RawMessage, error) {
	var dn struct {
		Result []struct {
			Data string `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(dataNode, &dn); err != nil {
		return nil, fmt.Errorf("解析 Data 节点: %w", err)
	}
	decoded := make([]json.RawMessage, 0, len(dn.Result))
	for _, item := range dn.Result {
		if item.Data == "" {
			continue
		}
		plain, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return nil, fmt.Errorf("base64 解码: %w", err)
		}
		if !json.Valid(plain) {
			return nil, fmt.Errorf("解码结果不是合法 JSON")
		}
		decoded = append(decoded, json.RawMessage(plain))
	}
	switch len(decoded) {
	case 0:
		return nil, fmt.Errorf("Data 节点无有效明细")
	case 1:
		return decoded[0], nil
	default:
		arr, err := json.Marshal(decoded)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(arr), nil
	}
}

// sign 复刻 docs/java-api-demo/.../SignedRequestsHelper.java 的算法：
//
//	待签名串 = "POST\n" + endpoint + "\n" + requestUri + "\n" + version + "\n"
//	         + msgId + "\n" + orgCode + "\n" + accessKeyId + "\n" + timestamp + "\n" + args
//	sig      = URLEncode(Base64(HMAC_SHA256(待签名串, Base64Decode(secretAccessKey))))
func (c *EntCreditClient) sign(requestURI, msgID, timestamp, args string) (string, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(c.cfg.SecretAccessKey)
	if err != nil {
		return "", fmt.Errorf("secretAccessKey 不是合法 base64: %w", err)
	}
	toSign := strings.Join([]string{
		http.MethodPost,
		c.cfg.Endpoint,
		requestURI,
		entCreditVersion,
		msgID,
		c.cfg.OrgCode,
		c.cfg.AccessKeyID,
		timestamp,
		args,
	}, "\n")

	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(toSign))
	b64 := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return url.QueryEscape(b64), nil
}

// Requery: 平台以 orderNo(=msgId) 幂等，对账查询接口待联调。在此之前返回
// Reachable=false, 记录保持 PENDING 由对账兜底 (与其余上游一致)。
func (c *EntCreditClient) Requery(ctx context.Context, reqid string) (*model.RequeryResult, error) {
	_ = ctx
	_ = reqid
	return &model.RequeryResult{Reachable: false}, nil
}
