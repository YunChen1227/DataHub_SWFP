//go:build ignore

// direct_swfp_upstream: 直连证通 entcreditapi 上游，批量测 Excel 税号。
// 凭证优先读环境变量，否则读 CONFIG_FILE 的 swfp 配置（支持 upstreams 列表或 legacy upstream+products）。
//
//   SWFP_ENDPOINT=https://cisp.zenitera.com
//   SWFP_ORG_CODE=...
//   SWFP_ACCESS_KEY_ID=...
//   SWFP_SECRET_ACCESS_KEY=...   # Base64
//   go run ./scripts/direct_swfp_upstream.go
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/upstream"
)

var creditCodes = []string{
	"911101055695184024",
	"914413035645245121",
	"91441381MAD7PP0M1U",
	"91441303MA52HE1729",
	"91441300MA530DHF52",
	"91330200563871993X",
	"913205067615027420",
	"913101165647980623",
	"91320583MA1PCENB13",
	"91320583MA1YLAK333",
	"91330201MA290X463B",
	"913302015670062191",
	"91310115MA1K41YN6W",
	"91320594398379945T",
	"91320594MA27DUCL8R",
}

var defaultProducts = []string{"P0130081", "P0130083", "P0130082", "P0130084"}

type fu struct {
	BaseURL         string   `yaml:"baseURL"`
	OrgCode         string   `yaml:"orgCode"`
	AccessKeyID     string   `yaml:"accessKeyId"`
	SecretAccessKey string   `yaml:"secretAccessKey"`
	Product         string   `yaml:"product"`
	Label           string   `yaml:"label"`
	Products        []string `yaml:"products"`
}

type fc struct {
	Versions map[string]struct {
		Upstreams []fu `yaml:"upstreams"`
		Upstream  fu   `yaml:"upstream"`
	} `yaml:"versions"`
}

func main() {
	endpoint, org, ak, sk, sources := loadSources()
	if endpoint == "" || org == "" || ak == "" || sk == "" {
		fmt.Println("缺少 SWFP 上游凭证。请设置环境变量 SWFP_ENDPOINT/SWFP_ORG_CODE/SWFP_ACCESS_KEY_ID/SWFP_SECRET_ACCESS_KEY")
		os.Exit(1)
	}
	if len(sources) == 0 {
		fmt.Println("未配置任何产品子源")
		os.Exit(1)
	}

	fmt.Printf("== 连通性探测 endpoint=%s orgCode=%s 子源=%d ==\n", endpoint, org, len(sources))
	cc := creditCodes[0]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := sources[0].Port.Query(ctx, &model.UpstreamRequest{CreditCode: cc, Reqid: "connect-probe"})
	if err != nil {
		fmt.Printf("首条税号 %s 探测(product=%s): FAIL %v\n", cc, sources[0].Label, err)
	} else {
		fmt.Printf("首条税号 %s 探测(product=%s): OK code=%s msg=%s\n", cc, sources[0].Label, result.Code, result.Msg)
	}

	// 探针要打全部子源，不能被「命中即停」短路：把所有调用装进一个逻辑源
	// （同一逻辑源内的调用互补、必须全部发出），即恢复旧聚合器的全量扇出行为。
	agg, err := upstream.NewSourcer([]upstream.Source{{
		Name: "probe", Provider: "entcredit", Provides: model.AllDims(), Calls: sources,
	}}, 120*time.Second)
	if err != nil {
		fmt.Println("寻源器失败:", err)
		os.Exit(1)
	}

	fmt.Printf("\n== 批量聚合 %d 个税号 (四产品并发) ==\n\n", len(creditCodes))
	fmt.Printf("%-22s | %-8s | %-12s | sections\n", "creditCode", "code", "aggregate")
	fmt.Println(strings.Repeat("-", 100))

	hasData := 0
	for _, code := range creditCodes {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
		r, err := agg.Query(ctx2, &model.UpstreamRequest{CreditCode: code, Reqid: "batch-" + code[len(code)-6:]})
		cancel2()
		if err != nil {
			fmt.Printf("%-22s | ERROR    |              | %v\n", code, err)
			continue
		}
		secs := summarizeRange(r.Range)
		fmt.Printf("%-22s | %-8s | %-12s | %s\n", code, r.Code, r.Msg, secs)
		if r.Code == "001" || r.Code == "002" {
			hasData++
		}
	}
	fmt.Printf("\n合计有数据(001/002): %d/%d\n", hasData, len(creditCodes))
}

func loadSources() (endpoint, org, ak, sk string, sources []upstream.Call) {
	endpoint = os.Getenv("SWFP_ENDPOINT")
	org = os.Getenv("SWFP_ORG_CODE")
	ak = os.Getenv("SWFP_ACCESS_KEY_ID")
	sk = os.Getenv("SWFP_SECRET_ACCESS_KEY")

	var entries []fu
	if org == "" || ak == "" || sk == "" || endpoint == "" {
		path := os.Getenv("CONFIG_FILE")
		if path == "" {
			path = "config.aliyun.prod.yaml"
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			var c fc
			if yaml.Unmarshal(raw, &c) == nil {
				s := c.Versions["swfp"]
				entries = s.Upstreams
				if len(entries) == 0 && (s.Upstream.BaseURL != "" || s.Upstream.OrgCode != "") {
					base := s.Upstream
					prods := base.Products
					if len(prods) == 0 {
						prods = defaultProducts
					}
					for _, p := range prods {
						e := base
						e.Product = p
						e.Label = upstream.EntCreditLabel(p)
						entries = append(entries, e)
					}
				}
				if endpoint == "" && len(entries) > 0 {
					endpoint = entries[0].BaseURL
				}
				if org == "" && len(entries) > 0 {
					org = entries[0].OrgCode
				}
				if ak == "" && len(entries) > 0 {
					ak = entries[0].AccessKeyID
				}
				if sk == "" && len(entries) > 0 {
					sk = entries[0].SecretAccessKey
				}
			}
		}
	}

	if strings.Contains(endpoint, "REPLACE_WITH") || strings.Contains(org, "REPLACE_WITH") ||
		strings.Contains(ak, "REPLACE_WITH") || strings.Contains(sk, "REPLACE_WITH") {
		endpoint, org, ak, sk = "", "", "", ""
	}

	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	for i, e := range entries {
		if e.Product == "" {
			continue
		}
		base := e.BaseURL
		if base == "" {
			base = endpoint
		}
		label := e.Label
		if label == "" {
			label = upstream.EntCreditLabel(e.Product)
		}
		client := upstream.NewEntCredit(upstream.EntCreditConfig{
			Endpoint:        base,
			OrgCode:         def(e.OrgCode, org),
			AccessKeyID:     def(e.AccessKeyID, ak),
			SecretAccessKey: def(e.SecretAccessKey, sk),
			Product:         e.Product,
		}, httpClient)
		sources = append(sources, upstream.Call{Label: label, Dims: model.AllDims(), Port: client})
		fmt.Printf("  子源[%d] label=%s product=%s endpoint=%s\n", i, label, e.Product, base)
	}
	return endpoint, org, ak, sk, sources
}

func def(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type section struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error"`
}

func summarizeRange(raw string) string {
	if raw == "" {
		return "-"
	}
	var m map[string]section
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "parse_err"
	}
	parts := make([]string, 0, 4)
	for _, k := range []string{"invoice1", "invoice2", "tax1", "tax2"} {
		s, ok := m[k]
		if !ok {
			parts = append(parts, k+":?")
			continue
		}
		tag := s.Status
		if tag == "ok" && len(s.Data) > 2 {
			tag = "ok+data"
		}
		parts = append(parts, k+":"+tag)
	}
	return strings.Join(parts, " | ")
}
