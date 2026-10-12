//go:build ignore

// check_swfp_wiring.go —— 启动前自检：确认某个配置文件里 swfp 路由的五个子源都装配到位
// （源1-4 entcredit 各带产品码 + 源5 salesdata），且源5 的 AppKey 能解出合法 AES 密钥。
//
// 用法：
//
//	go run ./scripts/check_swfp_wiring.go config.aliyun.prod.yaml
package main

import (
	"crypto/aes"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type fileUpstream struct {
	Kind            string `yaml:"kind"`
	Label           string `yaml:"label"`
	Product         string `yaml:"product"`
	BaseURL         string `yaml:"baseURL"`
	OrgCode         string `yaml:"orgCode"`
	AccessKeyID     string `yaml:"accessKeyId"`
	SecretAccessKey string `yaml:"secretAccessKey"`
	AppID           string `yaml:"appId"`
	AppSecret       string `yaml:"appSecret"`
	Optional        bool   `yaml:"optional"`
}

type fileVersion struct {
	Upstreams []fileUpstream `yaml:"upstreams"`
	Upstream  fileUpstream   `yaml:"upstream"`
}

type fileConfig struct {
	Versions map[string]fileVersion `yaml:"versions"`
}

// 契约层的 label → 源编号映射 (internal/infrastructure/upstream/swfpcontract.go)。
var alias = map[string]string{
	"invoice1": "源1", "invoice2": "源2", "tax1": "源3", "tax2": "源4", "sales": "源5",
	"ctax": "源6", "sy": "源7",
}

func main() {
	path := "config.aliyun.prod.yaml"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", path, err)
		os.Exit(1)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		fmt.Fprintf(os.Stderr, "解析 %s 失败: %v\n", path, err)
		os.Exit(1)
	}

	fv, ok := fc.Versions["swfp"]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s 里没有 versions.swfp\n", path)
		os.Exit(1)
	}
	ups := fv.Upstreams
	if len(ups) == 0 {
		fmt.Println("！仍在使用已废弃的单块 upstream: 写法，products: 数组不会被解析")
		ups = []fileUpstream{fv.Upstream}
	}

	fmt.Printf("%s → swfp 子源 %d 个\n\n", path, len(ups))
	bad := 0
	for i, u := range ups {
		name := alias[u.Label]
		if name == "" {
			name = fmt.Sprintf("未登记段名 %q", u.Label)
			bad++
		}
		fmt.Printf("[%d] %-4s kind=%-10s label=%-9s product=%-10s optional=%v\n",
			i+1, name, u.Kind, u.Label, u.Product, u.Optional)
		fmt.Printf("     baseURL=%s\n", u.BaseURL)

		switch u.Kind {
		case "entcredit":
			switch {
			case u.Product == "" || u.OrgCode == "" || u.AccessKeyID == "" || u.SecretAccessKey == "":
				fmt.Println("     ✗ entcredit 子源缺 product/orgCode/accessKeyId/secretAccessKey")
				bad++
			case isPlaceholder(u.BaseURL) || isPlaceholder(u.OrgCode) || isPlaceholder(u.AccessKeyID):
				fmt.Println("     - 模板占位符，跳过凭证校验")
			default:
				fmt.Println("     ✓ 凭证齐全")
			}
		case "salesdata":
			if u.AppID == "" {
				fmt.Println("     ✗ 缺 appId")
				bad++
			}
			if isPlaceholder(u.AppSecret) {
				fmt.Println("     - 模板占位符，跳过密钥校验")
				fmt.Println()
				continue
			}
			key, err := salesAESKey(u.AppSecret)
			if err != nil {
				fmt.Printf("     ✗ AppKey 不可用: %v\n", err)
				bad++
			} else if _, err := aes.NewCipher(key); err != nil {
				fmt.Printf("     ✗ AES 密钥非法: %v\n", err)
				bad++
			} else {
				fmt.Printf("     ✓ AppID=%s，AppKey 解出 %d 字节密钥 (AES-%d)\n", u.AppID, len(key), len(key)*8)
			}
		default:
			fmt.Printf("     ✗ 未知 kind %q\n", u.Kind)
			bad++
		}
		fmt.Println()
	}

	for _, want := range []string{"invoice1", "invoice2", "tax1", "tax2", "sales"} {
		found := false
		for _, u := range ups {
			if u.Label == want {
				found = true
			}
		}
		if !found {
			fmt.Printf("✗ 缺少子源 %s (%s)\n", want, alias[want])
			bad++
		}
	}

	if bad > 0 {
		fmt.Printf("\n%d 处问题\n", bad)
		os.Exit(1)
	}
	fmt.Println("✓ swfp 五源装配完整")
}

// isPlaceholder 认出 config.example.yaml 里的模板占位符（未填真值不算配置错）。
func isPlaceholder(v string) bool {
	return strings.HasPrefix(v, "REPLACE_WITH_")
}

// salesAESKey 与 internal/infrastructure/upstream/salesdata.go 的实现保持一致。
func salesAESKey(appKey string) ([]byte, error) {
	raw := []byte(appKey)
	switch len(raw) {
	case 16, 24, 32:
		return raw, nil
	}
	if decoded, err := hex.DecodeString(appKey); err == nil {
		switch len(decoded) {
		case 16, 24, 32:
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("AppKey 长度 %d 不是合法 AES 密钥：需 16/24/32 字节原文，或 32/48/64 个十六进制字符", len(raw))
}
