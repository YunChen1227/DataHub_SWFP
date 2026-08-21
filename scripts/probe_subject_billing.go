//go:build ignore

// 主体年度计费联调：同一成功税号查 3 次 + 同一失败税号查 3 次，再读 quota/coverage。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/datahub/relay/test/harness"
)

const version = "swfp"

var (
	successCode = "91130104MA0F82EJ4A" // 历史实测 001
	failCode    = "91130104MA0F6WYU2L" // 历史实测 002（源1~4 error，源5 empty）
)

type quotaSnap struct {
	ServiceUsed float64
	TotalCalls  float64
	Billing     map[string]float64
	Raw         string
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

	fmt.Printf("=== 主体年度计费联调 ===\n")
	fmt.Printf("base=%s appKey=%s\n", base, appKey)
	fmt.Printf("成功样本=%s  失败样本=%s  各查 3 遍\n\n", successCode, failCode)

	before := fetchQuota(appKey, secret)
	printQuota("基线 quota", before)

	fmt.Println("\n--- 查询阶段 ---")
	fmt.Printf("%-6s | %-22s | head | body | note\n", "round", "creditCode")
	fmt.Println(strings.Repeat("-", 72))

	type row struct {
		label, cc, head, body, note string
	}
	var rows []row

	for i := 1; i <= 3; i++ {
		for _, tc := range []struct {
			label, cc string
		}{
			{fmt.Sprintf("S%d", i), successCode},
			{fmt.Sprintf("F%d", i), failCode},
		} {
			r := harness.Query(version, appKey, secret, map[string]string{"creditCode": tc.cc}, nil)
			note := briefRange(r.Range)
			if r.ErrorCode != "0" {
				note = extractErrMsg(r.Raw)
			}
			rows = append(rows, row{tc.label, tc.cc, r.ErrorCode, r.BodyCode, note})
			fmt.Printf("%-6s | %-22s | %-4s | %-4s | %s\n", tc.label, tc.cc, r.ErrorCode, r.BodyCode, note)
			time.Sleep(300 * time.Millisecond) // 给异步 Bookkeeper 一点时间落库
		}
	}

	// 等异步记账 drain
	fmt.Println("\n等待异步记账 3s …")
	time.Sleep(3 * time.Second)

	after := fetchQuota(appKey, secret)
	printQuota("\n最终 quota", after)

	fmt.Println("\n--- quota 增量（最终 − 基线）---")
	printDelta(before, after)

	fmt.Println("\n--- 成功样本免费期 coverage ---")
	printCoverage(appKey, secret, successCode)

	fmt.Println("\n--- 失败样本免费期 coverage ---")
	printCoverage(appKey, secret, failCode)

	fmt.Println("\n--- 预期核对 ---")
	fmt.Println("成功税号 ×3：")
	fmt.Println("  serviceUsed +3（三次皆查得 001）")
	fmt.Println("  totalCalls  +6（三次皆调上游；若某次幂等/失败则可能更少）")
	fmt.Println("  chargedTotal +1（仅首次计费，后两次免费期命中）")
	fmt.Println("  amountFen   +首笔应收（rates=0 时仍为 0，但 chargedTotal 应 +1）")
	fmt.Println("失败税号 ×3：")
	fmt.Println("  serviceUsed 不变（002 无实得不计成功查得数）")
	fmt.Println("  chargedTotal 不变（查无/失败不开免费期、也不计费）")
	fmt.Println("  coverage     两类目 covered=false（从未计费过）")
}

func fetchQuota(appKey, secret string) quotaSnap {
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           harness.SignX1(map[string]string{}, secret),
		"body":           map[string]string{},
	}
	_, m, raw := harness.Call(httpMethodGet(), harness.QuotaPath(version), payload, nil)
	s := quotaSnap{Raw: raw, Billing: map[string]float64{}}
	if v, ok := m["serviceUsed"].(float64); ok {
		s.ServiceUsed = v
	}
	if v, ok := m["totalCalls"].(float64); ok {
		s.TotalCalls = v
	}
	if b, ok := m["billing"].(map[string]any); ok {
		for _, k := range []string{"chargedInvoice", "chargedTax", "chargedBoth", "chargedTotal", "amountFen"} {
			if v, ok := b[k].(float64); ok {
				s.Billing[k] = v
			}
		}
	}
	return s
}

func printQuota(title string, s quotaSnap) {
	fmt.Printf("%s:\n", title)
	fmt.Printf("  serviceUsed=%.0f  totalCalls=%.0f\n", s.ServiceUsed, s.TotalCalls)
	if len(s.Billing) == 0 {
		fmt.Printf("  billing=(无 billing 子对象 — 可能尚未部署新代码)\n")
		return
	}
	fmt.Printf("  billing: invoice=%.0f tax=%.0f both=%.0f total=%.0f amountFen=%.0f\n",
		s.Billing["chargedInvoice"], s.Billing["chargedTax"], s.Billing["chargedBoth"],
		s.Billing["chargedTotal"], s.Billing["amountFen"])
}

func printDelta(before, after quotaSnap) {
	fmt.Printf("  ΔserviceUsed = %.0f\n", after.ServiceUsed-before.ServiceUsed)
	fmt.Printf("  ΔtotalCalls  = %.0f\n", after.TotalCalls-before.TotalCalls)
	if len(after.Billing) > 0 {
		fmt.Printf("  ΔchargedTotal = %.0f\n", after.Billing["chargedTotal"]-before.Billing["chargedTotal"])
		fmt.Printf("  ΔamountFen    = %.0f\n", after.Billing["amountFen"]-before.Billing["amountFen"])
	}
}

func printCoverage(appKey, secret, creditCode string) {
	body := map[string]string{"creditCode": creditCode}
	payload := map[string]any{
		"encryptionType": 1,
		"appKey":         appKey,
		"sign":           harness.SignX1(body, secret),
		"body":           body,
	}
	path := "/v1/openapi/zlx/coverage" + strings.ToUpper(version)
	_, m, raw := harness.Call(httpMethodGet(), path, payload, nil)
	fmt.Printf("  creditCode=%s\n", creditCode)
	if ec, _ := m["errorCode"].(string); ec != "0" {
		fmt.Printf("  error: %s  raw=%s\n", m["errorMsg"], raw)
		return
	}
	for _, cat := range []string{"invoice", "tax"} {
		if c, ok := m[cat].(map[string]any); ok {
			fmt.Printf("  %s: covered=%v expiresAt=%v chargeCount=%.0f\n",
				cat, c["covered"], c["expiresAt"], num(c["chargeCount"]))
		}
	}
}

func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func httpMethodGet() string { return "GET" }

func extractErrMsg(raw string) string {
	const marker = `"errorMsg":"`
	i := strings.Index(raw, marker)
	if i < 0 {
		return "err"
	}
	rest := raw[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

func briefRange(raw string) string {
	if raw == "" {
		return "-"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "range?"
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
	if ds, ok := m["dataScope"].(string); ok && ds != "" {
		return "dataScope=" + ds
	}
	return "range_ok"
}
