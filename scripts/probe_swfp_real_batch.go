//go:build ignore

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/infrastructure/upstream"
)

func main() {
	const (
		endpoint = "https://cisp.zenitera.com"
		orgCode  = "4098500006"
		ak       = "P8NIAURDVCQZBN7LGPTK"
		sk       = "PhICNDET1R1CuldEdhNih6HHKVfdfH6PtJe/DbYCCCcE"
	)
	codes := []string{
		"911101055695184024",
		"914413035645245121",
		"91330200563871993X",
		"913101165647980623",
		"91330100MA2AAAAA0X",
	}
	products := []struct{ code, label string }{
		{"P0130081", "invoice1"},
		{"P0130083", "invoice2"},
		{"P0130082", "tax1"},
		{"P0130084", "tax2"},
	}
	httpClient := &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}

	fmt.Println("== 真实凭证四产品探测 (accessKeyId=机构授权码) ==")
	for _, cc := range codes {
		fmt.Printf("\n--- creditCode=%s ---\n", cc)
		ok, empty, fail := 0, 0, 0
		for _, p := range products {
			client := upstream.NewEntCredit(upstream.EntCreditConfig{
				Endpoint: endpoint, OrgCode: orgCode, AccessKeyID: ak,
				SecretAccessKey: sk, Product: p.code,
			}, httpClient)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			res, err := client.Query(ctx, &model.UpstreamRequest{CreditCode: cc, Reqid: "batch"})
			cancel()
			if err != nil {
				fail++
				msg := err.Error()
				if strings.Contains(msg, "状态码=1") {
					empty++
					fail--
					fmt.Printf("  %s: 查无(999)\n", p.label)
					continue
				}
				fmt.Printf("  %s: FAIL %s\n", p.label, msg)
				continue
			}
			ok++
			fmt.Printf("  %s: 查得(001) uid=%s\n", p.label, res.UID)
		}
		fmt.Printf("  汇总: 查得=%d 查无=%d 失败=%d\n", ok, empty, fail)
	}

	// 聚合四产品
	fmt.Println("\n== 四产品聚合 (911101055695184024) ==")
	calls := make([]upstream.Call, 0, 4)
	for _, p := range products {
		client := upstream.NewEntCredit(upstream.EntCreditConfig{
			Endpoint: endpoint, OrgCode: orgCode, AccessKeyID: ak,
			SecretAccessKey: sk, Product: p.code,
		}, httpClient)
		calls = append(calls, upstream.Call{Label: p.label, Dims: model.AllDims(), Port: client})
	}
	// 探针要打全部子源，不能被「命中即停」短路：全部调用装进一个逻辑源。
	agg, _ := upstream.NewSourcer([]upstream.Source{{
		Name: "probe", Provider: "entcredit", Provides: model.AllDims(), Calls: calls,
	}}, 120*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := agg.Query(ctx, &model.UpstreamRequest{CreditCode: "911101055695184024", Reqid: "agg-probe"})
	if err != nil {
		fmt.Println("聚合 FAIL:", err)
		return
	}
	fmt.Printf("聚合 code=%s msg=%s uid=%s\n", res.Code, res.Msg, res.UID)
	if res.Range != "" {
		fmt.Println(res.Range[:min(800, len(res.Range))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
