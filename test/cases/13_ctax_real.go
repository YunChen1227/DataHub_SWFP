//go:build ignore

// 13_ctax_real: 源6 真实环境联调（读 test/cases/测试税号.xlsx，直连惠众征信，不走 mock）。
//
// 默认打测试环境；生产需 ECS 白名单：
//
//	CTAX_BASE_URL=https://api.huizhongcredit.com/hzservice/sy/tax \
//	CTAX_APP_ID=9VTYC6YU CTAX_TOKEN='...' CTAX_AUTH_CODE=gckj \
//	go run test/cases/13_ctax_real.go
//
// 可选走 relay 全链路（relay 须用 config.local.real.yaml 启动，无 mock）：
//
//	RELAY_BASE_URL=http://localhost:8080 go run test/cases/13_ctax_real.go
//
// Run: go run test/cases/13_ctax_real.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/upstream"
	"github.com/datahub/relay/test/harness"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	rec := harness.NewRecorder("13_ctax_real", "源6 真实环境 (测试税号.xlsx → 惠众征信)")
	defer rec.Finish()

	codes, err := harness.LoadTestTaxNumbersResolved()
	if err != nil {
		rec.Check("读取测试税号.xlsx", "ok", false, err.Error())
		return
	}
	rec.Check("读取测试税号.xlsx", fmt.Sprintf("%d 条税号", len(codes)), len(codes) > 0, fmt.Sprintf("%v", codes))

	cfg := upstream.CTaxConfig{
		BaseURL:  env("CTAX_BASE_URL", "https://cloud-test.huizhongcredit.com/hzservice/sy/tax"),
		AppID:    env("CTAX_APP_ID", "FMRJGBVB"),
		Token:    env("CTAX_TOKEN", "L0HsaFt5LBemICthuAsBHs2k8CPf9xrEDpNQXbSWyLde2A8B0tPbcjFoSSGVKWbO"),
		AuthCode: env("CTAX_AUTH_CODE", "test"),
	}
	client := upstream.NewCTax(cfg, &http.Client{Timeout: 45 * time.Second})

	var okRound, hit, authFail, transport int
	var firstHit string
	delay := 1500 * time.Millisecond

	for i, cc := range codes {
		if i > 0 {
			time.Sleep(delay)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		res, qerr := client.Query(ctx, &model.UpstreamRequest{
			CreditCode: cc, Reqid: fmt.Sprintf("real-%d", i), Want: model.AllDims(),
		})
		cancel()

		switch {
		case qerr == nil && res.Code == "001":
			okRound++
			hit++
			if firstHit == "" {
				firstHit = cc
			}
		case qerr == nil && res.Code == "999":
			okRound++ // SYS404 查无：协议通、授权窗口可接受，只是这家没数据
		default:
			var ue *model.UpstreamError
			if errors.As(qerr, &ue) {
				if ue.Code == "203" || strings.Contains(ue.Msg, "A20029") {
					authFail++
				} else if ue.Code == "500" || strings.Contains(ue.Msg, "频繁") {
					// 限流：不算鉴权/配置失败，稍后重试一次
					time.Sleep(2 * time.Second)
					ctx2, cancel2 := context.WithTimeout(context.Background(), 45*time.Second)
					res2, qerr2 := client.Query(ctx2, &model.UpstreamRequest{
						CreditCode: cc, Reqid: fmt.Sprintf("real-retry-%d", i), Want: model.AllDims(),
					})
					cancel2()
					if qerr2 == nil && (res2.Code == "001" || res2.Code == "999") {
						okRound++
						if res2.Code == "001" {
							hit++
							if firstHit == "" {
								firstHit = cc
							}
						}
					} else {
						transport++
					}
				} else {
					transport++
				}
			} else if qerr != nil {
				transport++
			}
		}
	}

	rec.Check("鉴权/参数无致命错误", "authFail=0", authFail == 0,
		fmt.Sprintf("authFail=%d（203/A20029 表示 authCode/凭证/窗口配错）", authFail))
	rec.Check("真实往返成功率", fmt.Sprintf(">=%d/%d", len(codes)/2, len(codes)),
		okRound >= len(codes)/2,
		fmt.Sprintf("okRound=%d hit=%d transport/other=%d total=%d", okRound, hit, transport, len(codes)))

	expectHit := env("CTAX_EXPECT_HIT", "")
	if expectHit != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		res, qerr := client.Query(ctx, &model.UpstreamRequest{
			CreditCode: expectHit, Reqid: "expect-hit", Want: model.AllDims(),
		})
		cancel()
		rec.Check("指定税号必须查得", expectHit+" → 001",
			qerr == nil && res != nil && res.Code == "001" && !res.Got.Empty(),
			fmt.Sprintf("code=%v err=%v got=%v", codeOf(res), qerr, gotOf(res)))
	}

	// 可选：relay 全链路（源6 真实 + 其余源真实，config.local.real.yaml）。
	if base := os.Getenv("RELAY_BASE_URL"); base != "" {
		os.Setenv("RELAY_BASE_URL", base)
		appKey := harness.AppKeyFor("swfp")
		probe := codes[0]
		if firstHit != "" {
			probe = firstHit
		}
		r := harness.Query("swfp", appKey, harness.Secret, map[string]string{"creditCode": probe}, nil)
		ok := r.ErrorCode == "0" && (r.BodyCode == "001" || r.BodyCode == "999") &&
			!strings.Contains(r.Raw, "sourceStatus") && !strings.Contains(r.Raw, "源6")
		rec.Check("relay 全链路(真实源6)", fmt.Sprintf("creditCode=%s errorCode=0 & body∈{001,999} & 无源泄漏", probe),
			ok, fmt.Sprintf("errorCode=%s body=%s dataScope=%v", r.ErrorCode, r.BodyCode, dataScopeOf(r.Range)))
	}
}

func codeOf(res *model.UpstreamResult) string {
	if res == nil {
		return ""
	}
	return res.Code
}

func gotOf(res *model.UpstreamResult) string {
	if res == nil {
		return ""
	}
	return res.Got.String()
}

func dataScopeOf(rangeJSON string) map[string]any {
	if rangeJSON == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(rangeJSON), &m) != nil {
		return nil
	}
	ds, _ := m["dataScope"].(map[string]any)
	return ds
}
