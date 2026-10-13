//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/datahub/relay/test/harness"
)

func main() {
	base := os.Getenv("RELAY_BASE_URL")
	if base == "" {
		base = "http://aiszcloud.cn:8080"
	}
	appKey := os.Getenv("SWFP_APP_KEY")
	secret := os.Getenv("SWFP_SECRET")
	cc := os.Getenv("SWFP_CREDIT_CODE")
	if appKey == "" || secret == "" || cc == "" {
		fmt.Println("need SWFP_APP_KEY, SWFP_SECRET, SWFP_CREDIT_CODE")
		os.Exit(1)
	}
	os.Setenv("RELAY_BASE_URL", base)

	r := harness.Query("swfp", appKey, secret, map[string]string{"creditCode": cc}, nil)
	out := map[string]any{
		"baseURL":    base,
		"creditCode": cc,
		"httpStatus": r.HTTPStatus,
		"errorCode":  r.ErrorCode,
		"bodyCode":   r.BodyCode,
		"range":      r.Range,
	}
	if r.Raw != "" {
		var raw any
		if err := json.Unmarshal([]byte(r.Raw), &raw); err == nil {
			out["raw"] = raw
		} else {
			out["raw"] = r.Raw
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
