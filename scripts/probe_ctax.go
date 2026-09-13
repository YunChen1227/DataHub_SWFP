//go:build ignore

// probe_ctax: 用真实惠众征信环境 + test/cases/测试税号.xlsx 批量探测源6。
//
// 源6 的连接参数（baseURL / appId / token / authCode / authTime）**直接读配置文件**，
// 与 relay 用的是同一份 config，避免手抄凭证抄错：
//
//	# 阿里云生产（默认读 config.aliyun.prod.yaml 里的 ctax 源）：
//	go run ./scripts/probe_ctax.go
//
//	# 指定别的配置（如本地测试环境）：
//	CONFIG_FILE=config.local.real.yaml go run ./scripts/probe_ctax.go
//
// 只测单个税号：
//
//	CTAX_CREDIT_CODE=911101055695184024 go run ./scripts/probe_ctax.go
//
// 其它可调项（非连接参数，故不放 config）：
//
//	CTAX_TYPE=3        # 1=税务 2=发票 3=两者(默认)
//	CTAX_DELAY_MS=1500 # 批量间隔，测试环境有频控可调大
//	VERSION=swfp       # 读哪个 version 下的 ctax 源
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
	"gopkg.in/yaml.v3"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// probeConfig 只解析定位 ctax 源所需的最小字段（与 cmd/relay/config.go 的 yaml 标签一致）。
type probeConfig struct {
	Versions map[string]struct {
		Upstreams []struct {
			Kind        string `yaml:"kind"`
			BaseURL     string `yaml:"baseURL"`
			AppID       string `yaml:"appId"`
			Token       string `yaml:"token"`
			AuthCode    string `yaml:"authCode"`
			AuthTimeBeg string `yaml:"authTimeBeg"`
			AuthTimeEnd string `yaml:"authTimeEnd"`
		} `yaml:"upstreams"`
	} `yaml:"versions"`
}

// loadCTaxConfig 从配置文件里取出 ctax（源6）的连接参数，与 relay 同源同一份 config。
func loadCTaxConfig(path, version string) (upstream.CTaxConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return upstream.CTaxConfig{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var pc probeConfig
	if err := yaml.Unmarshal(raw, &pc); err != nil {
		return upstream.CTaxConfig{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	ver, ok := pc.Versions[version]
	if !ok {
		return upstream.CTaxConfig{}, fmt.Errorf("配置里没有 versions.%s", version)
	}
	for _, u := range ver.Upstreams {
		if u.Kind == "ctax" {
			return upstream.CTaxConfig{
				BaseURL:     u.BaseURL,
				AppID:       u.AppID,
				Token:       u.Token,
				AuthCode:    u.AuthCode,
				AuthTimeBeg: u.AuthTimeBeg,
				AuthTimeEnd: u.AuthTimeEnd,
			}, nil
		}
	}
	return upstream.CTaxConfig{}, fmt.Errorf("versions.%s 下没有 kind=ctax 的源（源6 未启用？）", version)
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	version := env("VERSION", "swfp")

	cfg, err := loadCTaxConfig(configFile, version)
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	if cfg.BaseURL == "" || cfg.AppID == "" || cfg.Token == "" {
		fmt.Printf("FAIL: %s 里 ctax 源缺 baseURL/appId/token（请先在配置里填全）\n", configFile)
		os.Exit(1)
	}

	wantType := env("CTAX_TYPE", "3") // 1=税务 2=发票 3=两者
	delayMs := 1500                   // 测试环境有频控，批量扫 xlsx 时默认放慢
	if v := env("CTAX_DELAY_MS", ""); v != "" {
		fmt.Sscanf(v, "%d", &delayMs)
	}

	var codes []string
	if one := env("CTAX_CREDIT_CODE", ""); one != "" {
		codes = []string{one}
	} else {
		codes, err = harness.LoadTestTaxNumbersResolved()
		if err != nil {
			fmt.Println("FAIL: load xlsx:", err)
			os.Exit(1)
		}
	}

	client := upstream.NewCTax(cfg, &http.Client{Timeout: 45 * time.Second})
	fmt.Printf("== ctax real probe ==\n")
	fmt.Printf("  config   : %s (versions.%s)\n", configFile, version)
	fmt.Printf("  baseURL  : %s\n", cfg.BaseURL)
	fmt.Printf("  appId    : %s\n", cfg.AppID)
	fmt.Printf("  authCode : %s\n", cfg.AuthCode)
	fmt.Printf("  type     : %s\n", wantType)
	fmt.Printf("  tax nums : %d (from 测试税号.xlsx)\n\n", len(codes))

	var hit, empty, authFail, rateLimit, other int
	for i, cc := range codes {
		if i > 0 {
			time.Sleep(time.Duration(delayMs) * time.Millisecond)
		}
		res, err := queryOnce(client, cc, wantFromType(wantType), fmt.Sprintf("probe-%d", i))
		if err != nil || (res != nil && res.Code != "001" && res.Code != "999") {
			var ue *model.UpstreamError
			if errors.As(err, &ue) && (ue.Code == "500" || strings.Contains(ue.Msg, "频繁")) {
				time.Sleep(3 * time.Second)
				res, err = queryOnce(client, cc, wantFromType(wantType), fmt.Sprintf("probe-retry-%d", i))
			}
		}
		switch {
		case err == nil && res.Code == "001":
			hit++
			got := res.Got.String()
			var sections []string
			var data map[string]json.RawMessage
			if json.Unmarshal([]byte(res.Range), &data) == nil {
				for k, v := range data {
					if len(v) > 2 && string(v) != "null" && string(v) != "[]" && string(v) != "{}" {
						sections = append(sections, k)
					}
				}
			}
			fmt.Printf("[HIT ] %s  code=001  got=%s  uid=%s  sections=%v\n", cc, got, res.UID, sections)
		case err == nil && res.Code == "999":
			empty++
			fmt.Printf("[EMPTY] %s  code=999  uid=%s\n", cc, res.UID)
		default:
			var ue *model.UpstreamError
			if errors.As(err, &ue) {
				if ue.Code == "203" || strings.Contains(ue.Msg, "A20029") || strings.Contains(ue.Msg, "A03001") {
					authFail++
					fmt.Printf("[AUTH] %s  code=%s  msg=%s  uid=%s\n", cc, ue.Code, ue.Msg, ue.UID)
				} else if ue.Code == "500" || strings.Contains(ue.Msg, "频繁") {
					rateLimit++
					fmt.Printf("[RATE] %s  code=%s  msg=%s  uid=%s\n", cc, ue.Code, ue.Msg, ue.UID)
				} else {
					other++
					fmt.Printf("[FAIL] %s  code=%s  msg=%s  uid=%s\n", cc, ue.Code, ue.Msg, ue.UID)
				}
			} else if err != nil {
				other++
				fmt.Printf("[ERR ] %s  %v\n", cc, err)
			} else {
				other++
				fmt.Printf("[??? ] %s  code=%s  msg=%s\n", cc, res.Code, res.Msg)
			}
		}
	}

	fmt.Printf("\n== summary: hit=%d empty=%d auth/config=%d rateLimit=%d other=%d total=%d ==\n",
		hit, empty, authFail, rateLimit, other, len(codes))
	if authFail > 0 {
		os.Exit(2) // 凭证/授权窗口配错
	}
	if other > 0 {
		os.Exit(3) // 非预期的上游失败
	}
	// rateLimit 只提示放慢 CTAX_DELAY_MS，不算联调失败
}

func queryOnce(client *upstream.CTaxClient, cc string, want model.DimSet, reqid string) (*model.UpstreamResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return client.Query(ctx, &model.UpstreamRequest{CreditCode: cc, Reqid: reqid, Want: want})
}

func wantFromType(t string) model.DimSet {
	switch t {
	case "1":
		return model.DimSet{Tax: true}
	case "2":
		return model.DimSet{Invoice: true}
	default:
		return model.AllDims()
	}
}
