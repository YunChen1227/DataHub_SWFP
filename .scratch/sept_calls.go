//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

func main() {
	raw, err := os.ReadFile("config.aliyun.prod.yaml")
	if err != nil {
		panic(err)
	}
	var cfg struct {
		Versions map[string]struct {
			Database struct {
				Host, Name, User, Password, SSLMode string
				Port                                int
			} `yaml:"database"`
		} `yaml:"versions"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		panic(err)
	}
	db := cfg.Versions["swfp"].Database
	if db.Port == 0 {
		db.Port = 5432
	}
	if db.SSLMode == "" {
		db.SSLMode = "disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)
	conn.Exec(ctx, "SET default_transaction_read_only = on")

	const where = `source_label IN ('invoice1','invoice2','tax1','tax2')
  AND created_at >= '2026-09-01 00:00:00+08' AND created_at < '2026-10-01 00:00:00+08'`

	run := func(title, sql string) {
		fmt.Println("== " + title)
		rows, err := conn.Query(ctx, sql)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		for _, f := range rows.FieldDescriptions() {
			fmt.Printf("%s\t", f.Name)
		}
		fmt.Println()
		for rows.Next() {
			vals, _ := rows.Values()
			for _, v := range vals {
				fmt.Printf("%v\t", v)
			}
			fmt.Println()
		}
	}

	run("by label", `SELECT source_label,
  count(*) FILTER (WHERE status<>'skipped') AS called,
  count(*) FILTER (WHERE status='ok') AS ok,
  count(*) FILTER (WHERE status='empty') AS empty,
  count(*) FILTER (WHERE status='error') AS err,
  count(*) FILTER (WHERE status='skipped') AS skipped,
  count(*) FILTER (WHERE status<>'skipped' AND NOT cost_covered AND cost_fen>0) AS paid,
  sum(cost_fen) AS cost_fen
FROM upstream_call WHERE `+where+` GROUP BY 1 ORDER BY 1`)

	run("by label x client", `SELECT u.source_label, u.app_key, COALESCE(li.name,'') AS name,
  count(*) FILTER (WHERE u.status<>'skipped') AS called
FROM (SELECT * FROM upstream_call WHERE `+where+`) u LEFT JOIN license li ON li.app_key=u.app_key
GROUP BY 1,2,3 ORDER BY 1,4 DESC`)

	run("data range", `SELECT min(created_at), max(created_at), count(*) FROM upstream_call`)
}
