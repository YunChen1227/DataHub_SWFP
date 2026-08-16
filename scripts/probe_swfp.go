//go:build ignore

// probe_swfp: 用真实凭证对证通 entcreditapi 四产品码做一次聚合联调探测。
// 凭证从 CONFIG_FILE (默认 config.aliyun.prod.yaml, gitignored) 的
// versions.swfp.upstreams 列表读取，不硬编码进本文件。
//
// 用法：
//   go run ./scripts/probe_swfp.go                            # 默认虚构信用代码（预期查无, 不计费）
//   SWFP_CREDIT_CODE=91xxxxxxxx go run ./scripts/probe_swfp.go # 指定真实企业（可能计费, 慎用）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/upstream"
)

type fileUpstream struct {
	Kind            string `yaml:"kind"`
	BaseURL         string `yaml:"baseURL"`
	OrgCode         string `yaml:"orgCode"`
	AccessKeyID     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	Product         string `yaml:"product"`
	Label           string `yaml:"label"`
}

type fileConfig struct {
	Versions map[string]struct {
		Upstreams []fileUpstream `yaml:"upstreams"`
	} `yaml:"versions"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))

	path := os.Getenv("CONFIG_FILE")
	if path == "" {
		path = "config.aliyun.prod.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读取配置失败:", err)
		os.Exit(1)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		fmt.Println("解析配置失败:", err)
		os.Exit(1)
	}
	ups := fc.Versions["swfp"].Upstreams
	if len(ups) == 0 {
		fmt.Println("配置缺少 versions.swfp.upstreams 列表")
		os.Exit(1)
	}

	creditCode := os.Getenv("SWFP_CREDIT_CODE")
	if creditCode == "" {
		creditCode = "91330100MA2AAAAA0X" // 虚构但格式合法（预期查无, 不计费）
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	calls := make([]upstream.Call, 0, len(ups))
	for i, u := range ups {
		if u.BaseURL == "" || u.OrgCode == "" {
			fmt.Printf("配置缺少 swfp 子源 %d 的 baseURL/orgCode\n", i)
			os.Exit(1)
		}
		label := u.Label
		if label == "" {
			label = upstream.EntCreditLabel(u.Product)
		}
		client := upstream.NewEntCredit(upstream.EntCreditConfig{
			Endpoint:        u.BaseURL,
			OrgCode:         u.OrgCode,
			AccessKeyID:     u.AccessKeyID,
			SecretAccessKey: u.SecretAccessKey,
			Product:         u.Product,
		}, httpClient)
		calls = append(calls, upstream.Call{Label: label, Dims: model.AllDims(), Port: client})
		fmt.Printf("  子源[%d] label=%s product=%s endpoint=%s\n", i, label, u.Product, u.BaseURL)
	}
	// 探针要打全部子源，不能被「命中即停」短路：把所有调用装进一个逻辑源
	// （同一逻辑源内的调用互补、必须全部发出），即恢复旧聚合器的全量扇出行为。
	agg, err := upstream.NewSourcer([]upstream.Source{{
		Name: "probe", Provider: "entcredit", Provides: model.AllDims(), Calls: calls,
	}}, 60*time.Second)
	if err != nil {
		fmt.Println("构建寻源器失败:", err)
		os.Exit(1)
	}

	fmt.Printf("== 探测开始: %d 个子源 creditCode=%s ==\n", len(calls), creditCode)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := agg.Query(ctx, &model.UpstreamRequest{CreditCode: creditCode, Reqid: "probe"})
	if err != nil {
		fmt.Println("\n== 聚合结果: 全部数据源失败 ==")
		fmt.Println("error:", err)
		os.Exit(1)
	}

	fmt.Printf("\n== 聚合结果: code=%s msg=%s uid=%s ==\n", result.Code, result.Msg, result.UID)
	if result.Range != "" {
		var pretty map[string]any
		_ = json.Unmarshal([]byte(result.Range), &pretty)
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
	}
}
