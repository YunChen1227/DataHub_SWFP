//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/datahub/relay/test/harness"
)

var creditCodes = []string{
	"91130104MA0F82EJ4A",
	"91130104MA0F7L6T16",
	"91130104MA0F6WYU2L",
	"91130104MA0F4AW03Y",
	"91130104MA0F3AEL5J",
}

func main() {
	appKey := os.Getenv("SWFP_APP_KEY")
	secret := os.Getenv("SWFP_SECRET")
	if appKey == "" || secret == "" {
		fmt.Println("need SWFP_APP_KEY and SWFP_SECRET")
		os.Exit(1)
	}
	base := os.Getenv("RELAY_BASE_URL")
	if base == "" {
		base = "http://www.aiszcloud.cn:8081"
	}
	os.Setenv("RELAY_BASE_URL", base)

	fmt.Printf("SWFP batch @ %s limit=%d\n\n", base, len(creditCodes))
	fmt.Printf("%-22s | %-8s | %-8s | %-6s | note\n", "creditCode", "head", "body", "http")
	fmt.Println(strings.Repeat("-", 70))

	stats := map[string]int{}
	for i, cc := range creditCodes {
		res := harness.Query("swfp", appKey, secret, map[string]string{"creditCode": cc}, nil)
		note := ""
		if res.ErrorCode != "0" {
			note = extractErrMsg(res.Raw)
		} else if res.Range != "" {
			note = summarizeRange(res.Range)
		}
		fmt.Printf("%-22s | %-8s | %-8s | %-6d | %s\n", cc, res.ErrorCode, res.BodyCode, res.HTTPStatus, note)
		stats[res.ErrorCode+"/"+res.BodyCode]++
		if i == 0 && res.BodyCode != "" {
			fmt.Printf("\nfirst body.code=%s\n\n", res.BodyCode)
		}
	}
	fmt.Println("\n汇总:")
	for k, v := range stats {
		fmt.Printf("  %s: %d\n", k, v)
	}
}

func extractErrMsg(raw string) string {
	const marker = `"errorMsg":"`
	i := strings.Index(raw, marker)
	if i < 0 {
		return ""
	}
	rest := raw[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

func summarizeRange(raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "parse_err"
	}
	if ss, ok := m["sourceStatus"].(map[string]any); ok {
		parts := make([]string, 0, 5)
		for _, k := range []string{"源1", "源2", "源3", "源4", "源5"} {
			if v, ok := ss[k]; ok {
				parts = append(parts, k+":"+fmt.Sprint(v))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if len(raw) > 80 {
		return "range_len=" + fmt.Sprint(len(raw))
	}
	return "range_ok"
}
