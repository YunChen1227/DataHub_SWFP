// One-off smoke test: hit every public route and print status + body.
// Usage: go run ./scripts/smoke_routes.go
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const (
	baseURL = "http://localhost:8080"
	secret  = "demo-app-secret"
	appKey  = "y890swfp"
)

func sign(params map[string]string, secret string) string {
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

func call(method, path string, body any) (int, string, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, reqBody)
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	ok := true
	check := func(name string, status int, body string, err error) {
		if err != nil {
			fmt.Printf("[FAIL] %s  err=%v\n", name, err)
			ok = false
			return
		}
		if status != http.StatusOK {
			fmt.Printf("[FAIL] %s  HTTP %d  body=%s\n", name, status, body)
			ok = false
			return
		}
		fmt.Printf("[OK]   %s  HTTP %d\n       %s\n", name, status, body)
	}

	// 1. GET /healthz
	st, body, err := call(http.MethodGet, "/healthz", nil)
	check("GET /healthz", st, body, err)

	// 2. POST /v1/openapi/zlx/querySrmxSWFP
	qBody := map[string]string{
		"creditCode": "92500233MA60R5KW8M",
	}
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           sign(qBody, secret),
		"body":           qBody,
	}
	st, body, err = call(http.MethodPost, "/v1/openapi/zlx/querySrmxSWFP", payload)
	check("POST /v1/openapi/zlx/querySrmxSWFP", st, body, err)

	// 3. GET /v1/openapi/zlx/quota (empty body → sign = MD5(secret))
	quotaPayload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           sign(map[string]string{}, secret),
		"body":           map[string]string{},
	}
	st, body, err = call(http.MethodGet, "/v1/openapi/zlx/quotaSWFP", quotaPayload)
	check("GET /v1/openapi/zlx/quotaSWFP", st, body, err)

	if ok {
		fmt.Println("\nAll routes responded successfully.")
	} else {
		fmt.Println("\nSome routes failed.")
	}
}
