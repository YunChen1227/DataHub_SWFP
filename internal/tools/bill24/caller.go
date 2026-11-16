package bill24

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/zerorule"
	"github.com/datahub/relay/internal/infrastructure/upstream"
)

// caller 按请求时间重调一条记录并给出结论。
type caller interface {
	recall(ctx context.Context, it Item) Result
}

func newResult(it Item) Result {
	from, to := zerorule.WindowMonths(it.At, Months)
	return Result{Item: it, From: from, To: to}
}

func saveRaw(dir string, it Item, body []byte) {
	if dir == "" || len(body) == 0 {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, it.Credit+"_"+string(it.Iface)+".json"), body, 0o644)
}

var reqSeq atomic.Int64

// newReqid 生成 ≤20 位的上游幂等键。
func newReqid() string {
	return fmt.Sprintf("b24%d%04d", time.Now().Unix()%1e10, reqSeq.Add(1)%10000)
}

// ---------------- 下游：swfp 中继 ----------------

type relayCaller struct {
	base, appKey, secret string
	http                 *http.Client
	rawDir               string
}

func (c *relayCaller) sign(body map[string]string) string {
	keys := make([]string, 0, len(body))
	for k, v := range body {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(body[k])
	}
	b.WriteString(c.secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (c *relayCaller) recall(ctx context.Context, it Item) Result {
	r := newResult(it)
	body := map[string]string{"creditCode": it.Credit, "dataType": string(it.Iface)}
	payload, _ := json.Marshal(map[string]any{
		"encryptionType": 1, "appKey": c.appKey, "sign": c.sign(body), "body": body,
	})
	var raw []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err = c.post(ctx, payload)
		if err == nil {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if err != nil {
		r.Outcome, r.Note = OutcomeFailed, err.Error()
		return r
	}
	saveRaw(c.rawDir, it, raw)

	var resp struct {
		Head struct {
			ErrorCode json.Number `json:"errorCode"`
			ErrorMsg  string      `json:"errorMsg"`
			LogID     string      `json:"logId"`
		} `json:"head"`
		Body struct {
			Code   string `json:"code"`
			Msg    string `json:"msg"`
			Result struct {
				Range json.RawMessage `json:"range"`
			} `json:"result"`
		} `json:"body"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		r.Outcome, r.Note = OutcomeFailed, "响应不是 JSON: "+truncate(string(raw), 120)
		return r
	}
	if resp.Head.ErrorCode.String() != "0" {
		r.Outcome, r.Note = OutcomeFailed, fmt.Sprintf("errorCode=%s %s", resp.Head.ErrorCode, resp.Head.ErrorMsg)
		return r
	}
	switch resp.Body.Code {
	case "001", "002":
	case "999", "":
		r.Outcome, r.Note = OutcomeMiss, resp.Body.Msg
		return r
	default:
		r.Outcome, r.Note = OutcomeFailed, fmt.Sprintf("code=%s %s", resp.Body.Code, resp.Body.Msg)
		return r
	}

	rng := []byte(resp.Body.Result.Range)
	var s string
	if json.Unmarshal(rng, &s) == nil { // range 是 JSON 字符串，需二次解析
		rng = []byte(s)
	}
	var scope struct {
		DataScope map[string]bool `json:"dataScope"`
	}
	_ = json.Unmarshal(rng, &scope)
	gotInv, gotTax := scope.DataScope["发票"], scope.DataScope["税务"]
	if (!it.Iface.Invoice() || !gotInv) && (!it.Iface.Tax() || !gotTax) {
		r.Outcome, r.Note = OutcomeNoData, "本次未取得所调接口的数据 code="+resp.Body.Code
		return r
	}
	a := zerorule.ExtractWindow(zerorule.LabelContract, rng, r.From, r.To)
	judge(&r, a.Invoice, a.Tax)
	r.Company = companyName(rng)
	if resp.Body.Code == "002" {
		r.Note = "部分返回 (002)"
	}
	return r
}

func (c *relayCaller) post(ctx context.Context, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/openapi/zlx/querySrmxSWFP", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 120))
	}
	return b, nil
}

// ---------------- 上游：直连数据源 ----------------

// upCall 是一次上游调用：label 决定 zerorule 取数规则。
type upCall struct {
	label string
	want  model.DimSet
	port  interface {
		Query(context.Context, *model.UpstreamRequest) (*model.UpstreamResult, error)
	}
}

type upstreamCaller struct {
	provider string
	// plan 给出一条记录要发出的调用（证通票税 = 发票 + 税务两次）。
	plan   func(it Item) ([]upCall, error)
	rawDir string
}

func (c *upstreamCaller) recall(ctx context.Context, it Item) Result {
	r := newResult(it)
	calls, err := c.plan(it)
	if err != nil {
		r.Outcome, r.Note = OutcomeFailed, err.Error()
		return r
	}
	inv, tax := new(big.Rat), new(big.Rat)
	raws := map[string]json.RawMessage{}
	var okN, emptyN int
	var errs, missing []string
	for _, call := range calls {
		var res *model.UpstreamResult
		var qerr error
		for attempt := 0; attempt < 2; attempt++ {
			res, qerr = call.port.Query(ctx, &model.UpstreamRequest{
				CreditCode: it.Credit, Scope: model.ScopeAll, Want: call.want, Reqid: newReqid(),
			})
			var ue *model.UpstreamError
			if qerr == nil || errors.As(qerr, &ue) {
				break // 业务失败不重试，只重试网络类错误
			}
			time.Sleep(1500 * time.Millisecond)
		}
		switch {
		case qerr != nil:
			errs = append(errs, call.label+": "+truncate(qerr.Error(), 160))
		case res.Code == "001":
			okN++
			raws[call.label] = json.RawMessage(def(res.Range, "null"))
			a := zerorule.ExtractWindow(call.label, []byte(res.Range), r.From, r.To)
			inv.Add(inv, a.Invoice)
			tax.Add(tax, a.Tax)
			if r.Company == "" {
				r.Company = companyName([]byte(res.Range))
			}
		default:
			emptyN++
			missing = append(missing, call.label)
		}
	}
	if len(raws) > 0 {
		b, _ := json.Marshal(raws)
		saveRaw(c.rawDir, it, b)
	}
	switch {
	case okN == 0 && len(errs) > 0:
		r.Outcome, r.Note = OutcomeFailed, strings.Join(errs, "; ")
		return r
	case okN == 0:
		r.Outcome = OutcomeMiss
		return r
	}
	judge(&r, inv, tax)
	var notes []string
	if len(missing) > 0 {
		notes = append(notes, "查无: "+strings.Join(missing, ","))
	}
	if len(errs) > 0 {
		notes = append(notes, "失败: "+strings.Join(errs, "; "))
	}
	r.Note = strings.Join(notes, "；")
	return r
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, MaxIdleConnsPerHost: 16}, //nolint:gosec // 证通证书链与 relay 同样跳过校验
	}
}

// newUpstreamCaller 按 provider 从配置构造直连客户端。
func newUpstreamCaller(cfg *Config, provider, rawDir string) (*upstreamCaller, error) {
	entries := cfg.upstreamsOf(provider)
	if len(entries) == 0 {
		return nil, fmt.Errorf("配置里没有 kind=%s 的上游", provider)
	}
	hc := newHTTPClient(60 * time.Second)
	c := &upstreamCaller{provider: provider, rawDir: rawDir}
	invDim, taxDim := model.DimSet{Invoice: true}, model.DimSet{Tax: true}

	switch provider {
	case "entcredit":
		var invCall, taxCall *upCall
		for _, e := range entries {
			client := upstream.NewEntCredit(upstream.EntCreditConfig{
				Endpoint: e.BaseURL, OrgCode: e.OrgCode, AccessKeyID: e.AccessKeyID,
				SecretAccessKey: e.SecretAccessKey, Product: e.Product,
			}, hc)
			label := def(e.Label, upstream.EntCreditLabel(e.Product))
			switch label {
			case "invoice1", "invoice2":
				invCall = &upCall{label: label, want: invDim, port: client}
			case "tax1", "tax2":
				taxCall = &upCall{label: label, want: taxDim, port: client}
			}
		}
		if invCall == nil || taxCall == nil {
			return nil, fmt.Errorf("证通配置需同时有发票与税务产品码")
		}
		c.plan = func(it Item) ([]upCall, error) {
			var calls []upCall
			if it.Iface.Invoice() {
				calls = append(calls, *invCall)
			}
			if it.Iface.Tax() {
				calls = append(calls, *taxCall)
			}
			return calls, nil
		}
	case "ctax":
		e := entries[0]
		client := upstream.NewCTax(upstream.CTaxConfig{BaseURL: e.BaseURL, AppID: e.AppID, Token: e.Token}, hc)
		c.plan = func(it Item) ([]upCall, error) {
			return []upCall{{label: "ctax", want: dimsOf(it.Iface), port: client}}, nil
		}
	case "sy":
		e := entries[0]
		c.plan = func(it Item) ([]upCall, error) {
			// 税友是唯一能带时间参数的上游：授权截止日 = 当时的请求日期。
			client := upstream.NewSy(upstream.SyConfig{
				BaseURL: e.BaseURL, AppID: e.AppID, Token: e.Token, AuthCode: e.AuthCode,
				AuthTimeEnd: it.At.In(cst).Format("2006-01-02"),
			}, hc)
			return []upCall{{label: "sy", want: dimsOf(it.Iface), port: client}}, nil
		}
	case "salesdata":
		e := entries[0]
		client := upstream.NewSalesData(upstream.SalesDataConfig{BaseURL: e.BaseURL, AppID: e.AppID, AppKey: e.AppSecret}, hc)
		c.plan = func(it Item) ([]upCall, error) {
			if !it.Iface.Invoice() {
				return nil, fmt.Errorf("凯盈云只提供发票数据，无法复核 %s", it.Iface.Label())
			}
			return []upCall{{label: "sales", want: invDim, port: client}}, nil
		}
	default:
		return nil, fmt.Errorf("不支持的上游 %s", provider)
	}
	return c, nil
}

func dimsOf(i Iface) model.DimSet { return model.DimSet{Invoice: i.Invoice(), Tax: i.Tax()} }

// ---------------- 工具 ----------------

var nameKeys = map[string]bool{"nsrmc": true, "qymc": true, "entName": true, "taxpayerName": true, "companyName": true}

// companyName 在数据里找第一个企业名称字段（各源字段名不同，深度优先、限深）。
func companyName(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return findName(v, 0)
}

func findName(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := x[k].(string); ok && nameKeys[k] && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		for _, k := range keys {
			if s := findName(x[k], depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range x {
			if s := findName(e, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
