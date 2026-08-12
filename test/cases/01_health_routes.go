//go:build ignore

// 01_health_routes: /healthz 与 SWFP 业务路由 (querySrmxSWFP + quotaSWFP) 的可达性。
//
// Run: go run test/cases/01_health_routes.go
package main

import (
	"net/http"

	"github.com/datahub/relay/test/harness"
)

func main() {
	rec := harness.NewRecorder("01_health_routes", "健康检查与 SWFP 路由可达性")
	defer rec.Finish()

	st, _, raw := harness.Call(http.MethodGet, "/healthz", nil, nil)
	rec.Check("GET /healthz == 200 ok", "HTTP 200 + body 含 ok",
		st == 200 && contains(raw, "ok"), itoa(st)+" "+raw)

	for _, v := range harness.Versions {
		// query: POST，带最小信封；只要不是 404 即视为已注册。
		x := harness.Query(v, harness.AppKeyFor(v), harness.Secret,
			map[string]string{"creditCode": "92500233MA60R5KW8M"}, nil)
		rec.Check("POST querySrmx"+up(v)+" 已注册", "返回 head 信封(非404)",
			x.HTTPStatus == 200 && x.ErrorCode != "", "status="+itoa(x.HTTPStatus)+" raw="+x.Raw)

		// quota: GET（带信封 body）。
		used := harness.ServiceUsed(v, harness.AppKeyFor(v), harness.Secret)
		rec.Check("GET quota"+up(v)+" 已注册", "返回 serviceUsed 数值", used >= 0, ftoa(used))
	}
}

func up(v string) string {
	b := []byte(v)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func ftoa(f float64) string {
	return itoa(int(f))
}
