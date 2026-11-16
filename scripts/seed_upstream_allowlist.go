//go:build ignore

// seed_upstream_allowlist: 从对账单 xlsx + data/sy_allowlist.csv 生成
// data/upstream_allowlist.csv（不读北京联信）。也可只校验已有 CSV。
//
//	go run ./scripts/seed_upstream_allowlist.go
//	BILL_DIR=~/Downloads OUT=data/upstream_allowlist.csv go run ./scripts/seed_upstream_allowlist.go
//
// 环境变量：
//
//	BILL_DIR   对账单目录（缺省用常见 Downloads 路径；找不到则只并入 sy CSV）
//	OUT        输出 CSV（缺省 data/upstream_allowlist.csv）
//	SY_CSV     税友旧白名单（缺省 data/sy_allowlist.csv）
package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/datahub/relay/internal/domain/allowlist"
	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/parse"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf("FAIL: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	out := env("OUT", "data/upstream_allowlist.csv")
	syCSV := env("SY_CSV", "data/sy_allowlist.csv")

	type key struct{ code, provider string }
	merged := map[key]*model.UpstreamAllowEntry{}
	add := func(e *model.UpstreamAllowEntry) {
		k := key{e.CreditCode, e.Provider}
		if cur, ok := merged[k]; ok {
			cur.Tax = cur.Tax || e.Tax
			cur.Invoice = cur.Invoice || e.Invoice
			if e.CallTime.After(cur.CallTime) {
				cur.CallTime = e.CallTime
			}
			return
		}
		cp := *e
		merged[k] = &cp
	}

	// 已有 OUT（对账单导入结果）先读入，再并 sy，重复运行幂等。
	if _, err := os.Stat(out); err == nil {
		entries, err := allowlist.LoadCSV(out)
		if err != nil {
			fail("读已有 %s: %v", out, err)
		}
		for _, e := range entries {
			add(e)
		}
		fmt.Printf("loaded existing %s: %d\n", out, len(entries))
	}

	if f, err := os.Open(syCSV); err == nil {
		defer f.Close()
		cr := csv.NewReader(f)
		recs, err := cr.ReadAll()
		if err != nil {
			fail("读 %s: %v", syCSV, err)
		}
		for i, rec := range recs {
			if i == 0 {
				continue
			}
			if len(rec) < 4 {
				fail("%s 第 %d 行列数不足", syCSV, i+1)
			}
			code, err := parse.NormalizeCreditCode(rec[0])
			if err != nil {
				fail("%s 第 %d 行税号非法: %q", syCSV, i+1, rec[0])
			}
			tax := rec[1] == "1"
			inv := rec[2] == "1"
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[3]))
			if err != nil {
				fail("%s 第 %d 行时间非法: %q", syCSV, i+1, rec[3])
			}
			add(&model.UpstreamAllowEntry{
				CreditCode: code, Provider: "sy", Tax: tax, Invoice: inv,
				CallTime: t, SourceFile: filepath.Base(syCSV),
			})
		}
		fmt.Printf("merged %s\n", syCSV)
	} else {
		fmt.Printf("skip %s: %v\n", syCSV, err)
	}

	if len(merged) == 0 {
		fail("没有任何条目；请先准备 %s 或 %s", out, syCSV)
	}

	keys := make([]key, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].code != keys[j].code {
			return keys[i].code < keys[j].code
		}
		return keys[i].provider < keys[j].provider
	})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("%v", err)
	}
	f, err := os.Create(out)
	if err != nil {
		fail("%v", err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"creditCode", "provider", "tax", "invoice", "callTime"})
	by := map[string]int{}
	for _, k := range keys {
		e := merged[k]
		_ = w.Write([]string{
			e.CreditCode, e.Provider,
			bool01(e.Tax), bool01(e.Invoice),
			e.CallTime.Format(time.RFC3339),
		})
		by[e.Provider]++
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fail("%v", err)
	}
	if err := f.Close(); err != nil {
		fail("%v", err)
	}
	if _, err := allowlist.LoadCSV(out); err != nil {
		fail("回读校验失败: %v", err)
	}
	fmt.Printf("OK: %s 共 %d 条 %v\n", out, len(keys), by)
}

func bool01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}