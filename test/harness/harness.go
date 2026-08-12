// Package harness provides shared helpers for the DataHub_SWFP test suite.
package harness

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Primary test client credentials: 每个「域」的存储各自播种一个独立的 demo license
// (memory seedDemo / postgres SeedDemo)，appKey 按域各不相同 (model.DemoAppKey)，
// secret 相同。v8 与 v9 同属 v8v9 域，共用同一个 demo appKey（license 共享，但
// 统计/日志按路由独立）；任何域的 demo appKey 在其它域的路由上都会鉴权失败 (505004)。
const (
	AppKey    = "y890swfp" // swfp 域的 demo appKey
	Secret    = "demo-app-secret"
	AdminUser = "admin"
	AdminPass = "admin12345"
)

// Versions is the ordered list of service versions under test.
var Versions = []string{"swfp"}

// demoAppKeys mirrors model.DemoAppKey：按域独立的 demo appKey。
var demoAppKeys = map[string]string{
	"swfp": "y890swfp",
}

// AppKeyFor returns the demo appKey seeded for the given route's 域.
func AppKeyFor(version string) string {
	if k, ok := demoAppKeys[version]; ok {
		return k
	}
	return "demo-" + version
}

// QueryPath returns the public query route for a version (统一 x1 信封, POST)。
func QueryPath(version string) string {
	return "/v1/openapi/zlx/querySrmx" + strings.ToUpper(version)
}

// QuotaPath returns the per-version quota route (GET, 同主接口鉴权)。
func QuotaPath(version string) string {
	return "/v1/openapi/zlx/quota" + strings.ToUpper(version)
}

// AdminBase returns the version-scoped admin API prefix (/admin/api/{ver})。
func AdminBase(version string) string {
	return "/admin/api/" + version
}

// BaseURL is the relay address (override via RELAY_BASE_URL).
func BaseURL() string {
	if v := os.Getenv("RELAY_BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

// SignX1 builds the x1 client signature: body 非空业务参数按键 ASCII 升序拼接
// (name+value)…，末尾追加 secret，再 MD5 小写 hex（appKey/sign/encryptionType 不参与）。
func SignX1(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params[k])
	}
	sb.WriteString(secret)
	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// Call issues an HTTP request and returns (status, decoded-json-map, raw-body).
func Call(method, path string, body any, headers map[string]string) (int, map[string]any, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, BaseURL()+path, rdr)
	if err != nil {
		return 0, nil, err.Error()
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

// X1Result is a parsed query response (统一 x1 信封, 三版本通用)。
type X1Result struct {
	HTTPStatus int
	ErrorCode  string // head.errorCode
	BodyCode   string // body.code (001/999)
	Range      string // body.result.range
	Raw        string
}

// Query builds the信封, signs the body, optionally overrides envelope fields
// (e.g. {"sign":"bad"} or {"appKey":""}), POSTs to the given version's route,
// and returns the parsed response.
func Query(version, appKey, secret string, body map[string]string, overrides map[string]any) X1Result {
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           SignX1(body, secret),
		"body":           body,
	}
	for k, v := range overrides {
		payload[k] = v
	}
	st, m, raw := Call(http.MethodPost, QueryPath(version), payload, nil)
	r := X1Result{HTTPStatus: st, Raw: raw}
	if head, ok := m["head"].(map[string]any); ok {
		r.ErrorCode, _ = head["errorCode"].(string)
	}
	if b, ok := m["body"].(map[string]any); ok {
		r.BodyCode, _ = b["code"].(string)
		if res, ok := b["result"].(map[string]any); ok {
			r.Range, _ = res["range"].(string)
		}
	}
	return r
}

// QuerySWFP is a convenience wrapper for the swfp version.
func QuerySWFP(appKey, secret string, body map[string]string, overrides map[string]any) X1Result {
	return Query("swfp", appKey, secret, body, overrides)
}

// ServiceUsed reads the cumulative 成功查得数 via the version's /quota route.
// Returns -1 when the field is absent (error path).
func ServiceUsed(version, appKey, secret string) float64 {
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           SignX1(map[string]string{}, secret),
		"body":           map[string]string{},
	}
	_, m, _ := Call(http.MethodGet, QuotaPath(version), payload, nil)
	if u, ok := m["serviceUsed"].(float64); ok {
		return u
	}
	return -1
}

// TotalCalls reads the cumulative 调用上游次数 via the version's /quota route.
// Returns -1 when the field is absent (error path). 计数按路由独立。
func TotalCalls(version, appKey, secret string) float64 {
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           SignX1(map[string]string{}, secret),
		"body":           map[string]string{},
	}
	_, m, _ := Call(http.MethodGet, QuotaPath(version), payload, nil)
	if u, ok := m["totalCalls"].(float64); ok {
		return u
	}
	return -1
}

// AdminLogin returns a bearer token for the bootstrap admin (empty on failure).
func AdminLogin() (string, string) {
	st, m, raw := Call(http.MethodPost, "/admin/api/login",
		map[string]string{"username": AdminUser, "password": AdminPass}, nil)
	if st != 200 {
		return "", raw
	}
	tok, _ := m["token"].(string)
	return tok, raw
}

// AuthHeader builds the bearer auth header map.
func AuthHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// ShortReqid builds a unique reqid (≤20 chars) for idempotency-sensitive cases.
func ShortReqid(prefix string) string {
	r := prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	if len(r) > 20 {
		r = r[len(r)-20:]
	}
	return r
}
