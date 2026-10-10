//go:build ignore

// diag_ctax_calls: 只读排查「huizhong 源6 调用量 vs 我方记录」的差异。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/diag_ctax_calls.go
//	DAYS=14 CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/diag_ctax_calls.go
//
// 会话强制 default_transaction_read_only=on，不会写库。
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

type diagConfig struct {
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

type query struct {
	title string
	sql   string
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	days, _ := strconv.Atoi(env("DAYS", "30"))
	if days <= 0 {
		days = 30
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fmt.Println("读取配置失败:", err)
		os.Exit(1)
	}
	var cfg diagConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fmt.Println("解析配置失败:", err)
		os.Exit(1)
	}
	db := cfg.Versions["swfp"].Database
	if db.Host == "" {
		fmt.Println("配置里没有 versions.swfp.database")
		os.Exit(1)
	}
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)
	for _, s := range []string{"SET default_transaction_read_only = on", "SET statement_timeout = '60s'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			fmt.Println(s, "失败:", err)
			os.Exit(1)
		}
	}

	since := fmt.Sprintf("now() - interval '%d days'", days)
	queries := []query{
		{"0. 总量与明细表时间范围（全量）", `
SELECT (SELECT count(*) FROM audit_log) AS audit_rows,
       (SELECT count(*) FROM billing_ledger) AS ledger_rows,
       (SELECT count(*) FROM upstream_call) AS call_rows,
       (SELECT min(created_at) FROM upstream_call) AS calls_from,
       (SELECT max(created_at) FROM upstream_call) AS calls_to`},
		{"1. ctax 按天按状态（ok/empty/error 才是真打了 huizhong）", `
SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, status, count(*)
FROM upstream_call WHERE source_label='ctax' AND created_at >= ` + since + `
GROUP BY 1,2 ORDER BY 1 DESC, 2`},
		{"2. ctax 实际调用：有无订单号 / 上游码 / 原因", `
SELECT status,
       CASE WHEN upstream_uid='' THEN 'no_order' ELSE 'has_order' END AS order_no,
       upstream_code,
       CASE WHEN upstream_msg ILIKE '%deadline%' OR upstream_msg ILIKE '%timeout%' THEN '[超时]'
            WHEN upstream_msg LIKE '%频繁%' THEN '[限流]'
            ELSE left(upstream_msg, 80) END AS reason,
       count(*)
FROM upstream_call
WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + `
GROUP BY 1,2,3,4 ORDER BY 5 DESC LIMIT 40`},
		{"3. ctax 耗时分布（ms）", `
SELECT status, count(*),
       percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms) AS p50,
       percentile_cont(0.9)  WITHIN GROUP (ORDER BY latency_ms) AS p90,
       percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms) AS p99,
       max(latency_ms) AS max,
       count(*) FILTER (WHERE latency_ms >= 3900) AS ge_3900ms
FROM upstream_call
WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + `
GROUP BY 1`},
		{"4. ctax 被跳过的原因", `
SELECT skip_reason, count(*) FROM upstream_call
WHERE source_label='ctax' AND status='skipped' AND created_at >= ` + since + `
GROUP BY 1 ORDER BY 2 DESC`},
		{"5. 有台账但完全没有逐源明细（异步明细丢失）", `
SELECT date(l.created_at AT TIME ZONE 'Asia/Shanghai') AS d, l.state, count(*)
FROM billing_ledger l
WHERE l.created_at >= ` + since + `
  AND NOT EXISTS (SELECT 1 FROM upstream_call u WHERE u.app_key=l.app_key AND u.reqid=l.reqid)
GROUP BY 1,2 ORDER BY 1 DESC, 2`},
		{"6. 有明细但没有审计行（统计页主表漏掉的请求）", `
SELECT date(u.created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(DISTINCT u.request_id)
FROM upstream_call u
WHERE u.created_at >= ` + since + `
  AND NOT EXISTS (SELECT 1 FROM audit_log a WHERE a.request_id=u.request_id)
GROUP BY 1 ORDER BY 1 DESC`},
		{"7. 审计按天（统计页主表口径）对比台账与 ctax 实调", `
WITH a AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS audit_total,
         count(*) FILTER (WHERE called_upstream) AS audit_called,
         count(*) FILTER (WHERE found_data) AS audit_found
  FROM audit_log WHERE created_at >= ` + since + ` GROUP BY 1),
l AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS ledger
  FROM billing_ledger WHERE created_at >= ` + since + ` GROUP BY 1),
c AS (
  SELECT date(created_at AT TIME ZONE 'Asia/Shanghai') AS d, count(*) AS ctax_called
  FROM upstream_call WHERE source_label='ctax' AND status<>'skipped' AND created_at >= ` + since + ` GROUP BY 1)
days AS (SELECT d FROM a UNION SELECT d FROM l UNION SELECT d FROM c)
SELECT days.d, a.audit_total, a.audit_called, a.audit_found, l.ledger, c.ctax_called
FROM days LEFT JOIN a ON a.d=days.d LEFT JOIN l ON l.d=days.d LEFT JOIN c ON c.d=days.d
ORDER BY 1 DESC`},
		{"8. 按客户的 ctax 实调量", `
SELECT u.app_key, COALESCE(li.name,'') AS name, count(*) AS ctax_called,
       count(*) FILTER (WHERE u.status='ok') AS ok,
       count(*) FILTER (WHERE u.status='empty') AS empty,
       count(*) FILTER (WHERE u.status='error') AS err
FROM upstream_call u LEFT JOIN license li ON li.app_key=u.app_key
WHERE u.source_label='ctax' AND u.status<>'skipped' AND u.created_at >= ` + since + `
GROUP BY 1,2 ORDER BY 3 DESC`},
	}

	fmt.Printf("== diag_ctax_calls  db=%s@%s/%s  最近 %d 天 ==\n\n", db.User, db.Host, db.Name, days)
	for _, q := range queries {
		fmt.Println("### " + q.title)
		runQuery(ctx, conn, q.sql)
		fmt.Println()
	}
}

func runQuery(ctx context.Context, conn *pgx.Conn, sql string) {
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		fmt.Println("ERR:", err)
		return
	}
	defer rows.Close()
	var hdr []string
	for _, f := range rows.FieldDescriptions() {
		hdr = append(hdr, f.Name)
	}
	fmt.Println(strings.Join(hdr, " | "))
	n := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			fmt.Println("ERR:", err)
			return
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch t := v.(type) {
			case time.Time:
				if rows.FieldDescriptions()[i].DataTypeOID == 1082 { // date
					parts[i] = t.Format("2006-01-02")
					break
				}
				parts[i] = t.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
			case nil:
				parts[i] = "-"
			default:
				parts[i] = fmt.Sprint(t)
			}
		}
		fmt.Println(strings.Join(parts, " | "))
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Println("ERR:", err)
	}
	if n == 0 {
		fmt.Println("(无数据)")
	}
}
