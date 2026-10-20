//go:build ignore

// export_lianxin_upstream_samples: 从北京联信的成功查得记录里，按上游各取最新一条
// 「查得票」和一条「查得税」的税号，写成 CSV（只读）。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/export_lianxin_upstream_samples.go
//
// 可选环境变量：
//
//	CUSTOMER=北京联信征信咨询有限责任公司   license.name，精确匹配（可多条 license）
//	OUT=test_res/xxx.csv                    缺省 test_res/北京联信-各上游最新查得.csv
//
// 口径：audit_log.found_data = true，且 upstream_call.status = ok。
// dim_invoice / dim_tax 是该次调用实际查得的维度（综合源可能只得其中一维）。
// 每个逻辑源、每个维度只留 created_at 最新、且税号非空的一条。
// 只提供单一维度的上游（证通发票、证通税务、销项）不会凭空补出另一维。
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

var cst = time.FixedZone("CST", 8*3600)

type dbConfig struct {
	Versions map[string]struct {
		Database struct {
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
			Name     string `yaml:"name"`
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			SSLMode  string `yaml:"sslmode"`
		} `yaml:"database"`
	} `yaml:"versions"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
	os.Exit(1)
}

type sample struct {
	creditCode string
	dim        string // 票 / 税
	source     string
	label      string
	provider   string
	at         time.Time
	requestID  string
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	customer := env("CUSTOMER", "北京联信征信咨询有限责任公司")
	out := env("OUT", filepath.Join("test_res", "北京联信-各上游最新查得.csv"))

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg dbConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fail("解析配置失败: %v", err)
	}
	db := cfg.Versions["swfp"].Database
	if db.Host == "" {
		fail("配置里没有 versions.swfp.database")
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		fail("连接失败: %v", err)
	}
	defer conn.Close(ctx)
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '120s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fail("%s 失败: %v", s, err)
		}
	}

	appKeys := loadAppKeys(ctx, conn, customer)
	if len(appKeys) == 0 {
		fail("license 里没有名称为 %q 的客户", customer)
	}
	rows := loadSamples(ctx, conn, appKeys)

	// 同一展示名（如历史 part1/part2）合并，只留该上游该维度最新的一条。
	type key struct{ upstream, dim string }
	best := map[key]sample{}
	for _, r := range rows {
		k := key{upstreamName(r), r.dim}
		old, ok := best[k]
		if !ok || r.at.After(old.at) {
			best[k] = r
		}
	}
	picked := make([]sample, 0, len(best))
	for _, r := range best {
		picked = append(picked, r)
	}
	sort.Slice(picked, func(i, j int) bool {
		di, dj := dimRank(picked[i].dim), dimRank(picked[j].dim)
		if di != dj {
			return di < dj
		}
		ui, uj := upstreamRank(upstreamName(picked[i])), upstreamRank(upstreamName(picked[j]))
		if ui != uj {
			return ui < uj
		}
		return upstreamName(picked[i]) < upstreamName(picked[j])
	})

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail("创建输出目录失败: %v", err)
	}
	f, err := os.Create(out)
	if err != nil {
		fail("创建 CSV 失败: %v", err)
	}
	defer f.Close()
	if _, err := f.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		fail("写 CSV 失败: %v", err)
	}
	w := csv.NewWriter(f)
	if err := w.Write([]string{"税号", "查询类型", "上游", "标记", "查得时间", "request_id"}); err != nil {
		fail("写 CSV 失败: %v", err)
	}
	for _, r := range picked {
		kind := queryKind(r.dim)
		up := upstreamName(r)
		mark := r.creditCode + " - " + kind + " - " + up
		if err := w.Write([]string{
			r.creditCode, kind, up, mark,
			r.at.In(cst).Format("2006-01-02 15:04:05"), r.requestID,
		}); err != nil {
			fail("写 CSV 失败: %v", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fail("写 CSV 失败: %v", err)
	}

	fmt.Printf("%s：各上游最新查得 %d 条\n", customer, len(picked))
	for _, r := range picked {
		fmt.Printf("  %s - %s - %s  (%s)\n",
			r.creditCode, queryKind(r.dim), upstreamName(r), r.at.In(cst).Format("2006-01-02 15:04:05"))
	}
	fmt.Println("已输出:", out)
}

func loadAppKeys(ctx context.Context, conn *pgx.Conn, name string) []string {
	rows, err := conn.Query(ctx, `SELECT app_key FROM license WHERE name=$1`, name)
	if err != nil {
		fail("查询 license 失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			fail("读取 license 失败: %v", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		fail("读取 license 失败: %v", err)
	}
	return out
}

// loadSamples 每个逻辑源、每个维度取最新一条成功查得且税号非空的记录。
func loadSamples(ctx context.Context, conn *pgx.Conn, appKeys []string) []sample {
	rows, err := conn.Query(ctx, `
WITH hits AS (
    SELECT btrim(a.credit_code) AS credit_code,
           '票' AS dim,
           uc.source_name, uc.source_label, uc.provider, uc.created_at, uc.request_id
    FROM audit_log a
    JOIN upstream_call uc ON uc.request_id = a.request_id
    WHERE a.app_key = ANY($1)
      AND a.found_data
      AND uc.status = 'ok'
      AND uc.dim_invoice
      AND btrim(COALESCE(a.credit_code, '')) <> ''
    UNION ALL
    SELECT btrim(a.credit_code),
           '税',
           uc.source_name, uc.source_label, uc.provider, uc.created_at, uc.request_id
    FROM audit_log a
    JOIN upstream_call uc ON uc.request_id = a.request_id
    WHERE a.app_key = ANY($1)
      AND a.found_data
      AND uc.status = 'ok'
      AND uc.dim_tax
      AND btrim(COALESCE(a.credit_code, '')) <> ''
)
SELECT DISTINCT ON (source_name, dim)
       credit_code, dim, source_name, source_label, provider, created_at, request_id
FROM hits
ORDER BY source_name, dim, created_at DESC`, appKeys)
	if err != nil {
		fail("查询上游查得失败: %v", err)
	}
	defer rows.Close()
	var out []sample
	for rows.Next() {
		var r sample
		if err := rows.Scan(&r.creditCode, &r.dim, &r.source, &r.label, &r.provider, &r.at, &r.requestID); err != nil {
			fail("读取上游查得失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		fail("读取上游查得失败: %v", err)
	}
	return out
}

func queryKind(dim string) string {
	if dim == "票" {
		return "查询票"
	}
	return "查询税"
}

func upstreamName(r sample) string {
	switch r.source {
	case "ent_invoice":
		return "证通发票"
	case "ent_tax":
		return "证通税务"
	case "sales":
		return "销项"
	case "ctax":
		return "惠众C"
	case "sy":
		return "税友"
	}
	switch r.provider {
	case "entcredit":
		if strings.HasPrefix(r.label, "invoice") {
			return "证通发票"
		}
		return "证通税务"
	case "ctax":
		return "惠众C"
	case "sy":
		return "税友"
	case "salesdata":
		return "销项"
	}
	if r.source != "" {
		return r.source
	}
	return r.provider + "/" + r.label
}

func dimRank(dim string) int {
	if dim == "票" {
		return 0
	}
	return 1
}

func upstreamRank(name string) int {
	switch name {
	case "税友":
		return 0
	case "惠众C":
		return 1
	case "证通发票":
		return 2
	case "证通税务":
		return 3
	case "销项":
		return 4
	}
	return 9
}
