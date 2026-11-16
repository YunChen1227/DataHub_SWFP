package bill24

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// job 描述一份对账单：从哪抓、怎么重调、表头写什么。
type job struct {
	name     string // 文件名前缀，如「北京联信」「惠众」
	issuer   string // 对账单标题里的出单方
	customer string // 对账单「客户名称」
	appID    func(Item) string
	fetch    func(ctx context.Context, db *pgx.Conn, m Month) ([]Item, error)
	caller   func(rawDir string) (caller, error)
	// foundOnly 为查得口径：库里查得即计费，不重调、不做金额判定。
	foundOnly bool
}

// RunDownstream 生成某下游客户（缺省北京联信）的 24 个月口径月度对账单。
//
// 环境变量：CUSTOMER（license.name 模糊匹配，缺省 北京联信）、RELAY_BASE_URL、
// SWFP_APP_KEY / SWFP_SECRET（重调用的凭证）、ISSUER（标题里的出单方，即我方）。
func RunDownstream() error { return runDownstream(false) }

// RunDownstreamFound 生成某下游客户的查得口径月度对账单（不重调、不判金额）。
func RunDownstreamFound() error { return runDownstream(true) }

func runDownstream(foundOnly bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := connect(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close(ctx)

	customer := env("CUSTOMER", "北京联信")
	keys, display, err := customerAppKeys(ctx, db, customer)
	if err != nil {
		return err
	}
	fmt.Printf("客户 %s  app_key=%v\n", display, keys)
	route := env("ROUTE", "swfp")
	rc := &relayCaller{
		base:   env("RELAY_BASE_URL", "http://www.aiszcloud.cn:8081"),
		appKey: env("SWFP_APP_KEY", "j92eme33uipj"),
		secret: env("SWFP_SECRET", "edd726edad2da85f39285e5e81f21af5"),
		http:   newHTTPClient(60 * time.Second),
	}
	return run(ctx, db, job{
		name:     customer,
		issuer:   env("ISSUER", ""),
		customer: display,
		appID:    func(it Item) string { return it.AppKey },
		fetch: func(ctx context.Context, db *pgx.Conn, m Month) ([]Item, error) {
			return fetchDownstream(ctx, db, route, keys, m)
		},
		caller: func(rawDir string) (caller, error) {
			c := *rc
			c.rawDir = rawDir
			return &c, nil
		},
		foundOnly: foundOnly,
	})
}

// Upstream 名称：provider -> 文件名/出单方缺省值。
var upstreamNames = map[string]string{
	"entcredit": "证通",
	"ctax":      "惠众",
	"sy":        "税友",
	"salesdata": "凯盈云",
}

// RunUpstream 生成某上游的 24 个月口径月度对账单（上游账单不分下游客户）。
//
// 环境变量：ISSUER（标题里的出单方，缺省上游简称）、OUR_NAME（客户名称，即我方）。
func RunUpstream(provider string) error { return runUpstream(provider, false) }

// RunUpstreamFound 生成某上游的查得口径月度对账单（不重调、不判金额）。
func RunUpstreamFound(provider string) error { return runUpstream(provider, true) }

func runUpstream(provider string, foundOnly bool) error {
	name, ok := upstreamNames[provider]
	if !ok {
		return fmt.Errorf("未知上游 %s", provider)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	entries := cfg.upstreamsOf(provider)
	if len(entries) == 0 {
		return fmt.Errorf("配置里没有 kind=%s 的上游", provider)
	}
	ourID := def(entries[0].AppID, entries[0].OrgCode)
	ctx := context.Background()
	db, err := connect(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer db.Close(ctx)

	route := env("ROUTE", "swfp")
	return run(ctx, db, job{
		name:     name,
		issuer:   env("ISSUER", name),
		customer: env("OUR_NAME", ""),
		appID:    func(Item) string { return ourID },
		fetch: func(ctx context.Context, db *pgx.Conn, m Month) ([]Item, error) {
			return fetchUpstream(ctx, db, route, provider, m)
		},
		caller: func(rawDir string) (caller, error) {
			return newUpstreamCaller(cfg, provider, rawDir)
		},
		foundOnly: foundOnly,
	})
}

func run(ctx context.Context, db *pgx.Conn, j job) error {
	months, err := parseMonths(env("MONTHS", "2026-09,2026-10"))
	if err != nil {
		return err
	}
	if j.foundOnly {
		return runFound(ctx, db, j, months)
	}
	outRoot := env("OUT_DIR", filepath.Join("test_res", "bill24"))
	workers := envInt("WORKERS", 6)
	limit := envInt("LIMIT", 0)

	for _, m := range months {
		items, err := j.fetch(ctx, db, m)
		if err != nil {
			return fmt.Errorf("%s 抓取: %w", m.Name, err)
		}
		items = dedupe(items)
		total := len(items)
		if limit > 0 && len(items) > limit {
			items = items[:limit]
		}
		fmt.Printf("\n== %s %s：查得 %d 条（税号×接口去重）%s ==\n", j.name, m.Name, total, limitNote(limit, len(items)))

		dir := filepath.Join(outRoot, j.name, m.Name)
		rawDir := filepath.Join(dir, "raw")
		if err := os.MkdirAll(rawDir, 0o755); err != nil {
			return err
		}
		c, err := j.caller(rawDir)
		if err != nil {
			return err
		}
		results, err := recallAll(ctx, c, items, filepath.Join(dir, "progress.jsonl"), workers)
		if err != nil {
			return err
		}
		out := filepath.Join(outRoot, fmt.Sprintf("%s-月度对账单-%s.xlsx", j.name, m.Name))
		if err := writeBill(out, j, m, results); err != nil {
			return err
		}
		printSummary(m, results, out)
	}
	return nil
}

// runFound 是查得口径：库里查得的记录全部计费，不重调。DEDUPE=0 时按调用逐条列出，
// 否则同月同 (税号, 接口) 只计首次。
func runFound(ctx context.Context, db *pgx.Conn, j job, months []Month) error {
	outRoot := env("OUT_DIR", filepath.Join("test_res", "bill_found"))
	perCall := env("DEDUPE", "1") == "0"
	for _, m := range months {
		items, err := j.fetch(ctx, db, m)
		if err != nil {
			return fmt.Errorf("%s 抓取: %w", m.Name, err)
		}
		if perCall {
			items = withCredit(items)
		} else {
			items = dedupe(items)
		}
		results := make([]Result, len(items))
		count := map[Iface]int{}
		for i, it := range items {
			results[i] = Result{Item: it, Outcome: OutcomeBill}
			count[it.Iface]++
		}
		out := filepath.Join(outRoot, fmt.Sprintf("%s-月度对账单-查得口径-%s.xlsx", j.name, m.Name))
		if err := writeBill(out, j, m, results); err != nil {
			return err
		}
		fmt.Printf("%s %s 查得：只调用票 %d / 只调用税 %d / 同时调用票税 %d，合计 %d\n写出 %s\n",
			j.name, m.Name, count[IfaceInvoice], count[IfaceTax], count[IfaceBoth], len(items), out)
	}
	return nil
}

func limitNote(limit, n int) string {
	if limit > 0 {
		return fmt.Sprintf("，LIMIT=%d 本次只复核 %d 条", limit, n)
	}
	return ""
}

// recallAll 并发重调，结果逐条追加到 progress.jsonl；已有终态（非失败）的记录直接复用。
func recallAll(ctx context.Context, c caller, items []Item, progress string, workers int) ([]Result, error) {
	done := map[string]Result{}
	if f, err := os.Open(progress); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var r Result
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Outcome != OutcomeFailed {
				done[r.Key()] = r
			}
		}
		f.Close()
	}
	pf, err := os.OpenFile(progress, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	results := make([]Result, len(items))
	var pending []int
	for i, it := range items {
		if r, ok := done[it.Key()]; ok && r.At.Equal(it.At) {
			results[i] = r
			continue
		}
		pending = append(pending, i)
	}
	fmt.Printf("已完成 %d，待重调 %d（并发 %d）\n", len(items)-len(pending), len(pending), workers)

	var mu sync.Mutex
	var n int
	ch := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				r := c.recall(cctx, items[i])
				cancel()
				b, _ := json.Marshal(r)
				mu.Lock()
				results[i] = r
				pf.Write(append(b, '\n'))
				n++
				fmt.Printf("[%d/%d] %s %s %s total=%s %s\n", n, len(pending), r.Credit, r.Iface.Label(), r.Outcome, r.Total, truncate(r.Note, 80))
				mu.Unlock()
			}
		}()
	}
	for _, i := range pending {
		ch <- i
	}
	close(ch)
	wg.Wait()
	return results, nil
}

func printSummary(m Month, results []Result, out string) {
	count := map[Iface]int{}
	var zero, pend int
	for _, r := range results {
		switch r.Outcome {
		case OutcomeBill:
			count[r.Iface]++
		case OutcomeZero:
			zero++
		default:
			pend++
		}
	}
	fmt.Printf("%s 计费：只调用票 %d / 只调用税 %d / 同时调用票税 %d；不计费(24月<=0) %d；待确认 %d\n写出 %s\n",
		m.Name, count[IfaceInvoice], count[IfaceTax], count[IfaceBoth], zero, pend, out)
}

// Main 是脚本入口的统一错误处理。
func Main(f func() error) {
	if err := f(); err != nil {
		fmt.Println("失败:", err)
		os.Exit(1)
	}
}
