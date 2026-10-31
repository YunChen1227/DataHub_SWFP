//go:build ignore

// recompute_payload_flags: 零数据规则 (internal/domain/zerorule) 变更后，用库里留存的上游原始
// 数据 (upstream_payload.raw_data) 重算近 12 个月金额与零数据标记，回写 upstream_payload 和
// request_payload，并把 rule_version 更新为当前规则版本。
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/recompute_payload_flags.go          # 只统计不写库
//	CONFIG_FILE=config.aliyun.prod.yaml APPLY=1 go run ./scripts/recompute_payload_flags.go  # 写库
//
// 可选环境变量：
//
//	APPLY=1          真正写库；缺省只打印将要变化的条数
//	ALL=1            全部重算；缺省只重算 rule_version 不等于当前版本的请求
//	FROM=2026-10-01  只重算该北京时间日期（含）之后的请求
//	TO=2026-10-31    只重算该北京时间日期（含）之前的请求
//	BATCH=500        每批请求数（一批一个事务）
//
// 口径与线上写入一致 (internal/application/payload.go)：
//   - 窗口以请求时间 (request_payload.created_at) 为基准往前 12 个月。
//   - 源级标记按该源自己的数据判定；只有查得 (status=ok) 的源参与请求级合并，查得维度取
//     upstream_call.dim_invoice / dim_tax。
//   - 没有原始数据的老请求不在 request_payload 里，本脚本不会碰。
package main

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/datahub/relay/internal/domain/model"
	"github.com/datahub/relay/internal/domain/zerorule"
)

var cst = time.FixedZone("CST", 8*3600)

type exportConfig struct {
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

type reqRow struct {
	id        int64
	requestID string
	at        time.Time
	verdict   verdict
}

type srcRow struct {
	requestID, label, status string
	raw                      []byte
	got                      model.DimSet
	verdict                  verdict
}

// verdict 是库里一行的标记，金额统一成两位小数的文本便于比较。
type verdict struct {
	inv, tax         *string
	invZero, taxZero *bool
	ruleVersion      string
}

func (v verdict) equal(o verdict) bool {
	return eqStr(v.inv, o.inv) && eqStr(v.tax, o.tax) && eqBool(v.invZero, o.invZero) &&
		eqBool(v.taxZero, o.taxZero) && v.ruleVersion == o.ruleVersion
}

func eqStr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func eqBool(a, b *bool) bool  { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func ratText(r *big.Rat) *string {
	if r == nil {
		return nil
	}
	s := r.FloatString(2)
	return &s
}

func fromVerdict(v zerorule.Verdict) verdict {
	return verdict{inv: ratText(v.InvAmt), tax: ratText(v.TaxAmt), invZero: v.InvZero, taxZero: v.TaxZero, ruleVersion: zerorule.Version}
}

// normalize 把库里读出的 NUMERIC 文本统一成两位小数。
func normalize(s *string) *string {
	if s == nil {
		return nil
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok {
		return s
	}
	return ratText(r)
}

func main() {
	configFile := env("CONFIG_FILE", "config.aliyun.prod.yaml")
	apply := env("APPLY", "") == "1"
	all := env("ALL", "") == "1"
	batch, err := strconv.Atoi(env("BATCH", "500"))
	if err != nil || batch <= 0 {
		fail("BATCH 应为正整数")
	}
	var from, to *time.Time
	if s := env("FROM", ""); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, cst)
		if err != nil {
			fail("FROM 格式应为 YYYY-MM-DD: %v", err)
		}
		from = &t
	}
	if s := env("TO", ""); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, cst)
		if err != nil {
			fail("TO 格式应为 YYYY-MM-DD: %v", err)
		}
		t = t.AddDate(0, 0, 1)
		to = &t
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		fail("读取配置失败: %v", err)
	}
	var cfg exportConfig
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

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15",
		db.Host, db.Port, db.User, db.Password, db.Name, db.SSLMode))
	if err != nil {
		fail("连接数据库失败: %v", err)
	}
	defer conn.Close(ctx)

	fmt.Printf("规则版本 %s，模式：%s，范围：%s\n", zerorule.Version, map[bool]string{true: "写库", false: "只统计"}[apply],
		map[bool]string{true: "全部请求", false: "仅旧版本请求"}[all])

	var lastID int64
	var scanned, reqChanged, srcChanged int
	for {
		reqs, err := loadRequests(ctx, conn, lastID, batch, all, from, to)
		if err != nil {
			fail("读取 request_payload 失败: %v", err)
		}
		if len(reqs) == 0 {
			break
		}
		lastID = reqs[len(reqs)-1].id
		ids := make([]string, len(reqs))
		for i, r := range reqs {
			ids[i] = r.requestID
		}
		srcs, err := loadSources(ctx, conn, ids)
		if err != nil {
			fail("读取 upstream_payload 失败: %v", err)
		}

		var reqUpd []reqRow
		var srcUpd []srcRow
		for _, r := range reqs {
			var merged []zerorule.Source
			for _, s := range srcs[r.requestID] {
				amounts := zerorule.Extract(s.label, s.raw, r.at)
				var got model.DimSet
				if s.status == string(model.CallOK) {
					got = s.got
					merged = append(merged, zerorule.Source{Amounts: amounts, Got: got})
				}
				nv := fromVerdict(zerorule.Judge(amounts, got))
				if !nv.equal(s.verdict) {
					s.verdict = nv
					srcUpd = append(srcUpd, s)
				}
			}
			nv := fromVerdict(zerorule.Merge(merged))
			if !nv.equal(r.verdict) {
				r.verdict = nv
				reqUpd = append(reqUpd, r)
			}
		}
		scanned += len(reqs)
		reqChanged += len(reqUpd)
		srcChanged += len(srcUpd)
		if apply && (len(reqUpd) > 0 || len(srcUpd) > 0) {
			if err := write(ctx, conn, reqUpd, srcUpd); err != nil {
				fail("写库失败（本批已回滚，已处理到 id=%d 之前）: %v", reqs[0].id, err)
			}
		}
		fmt.Printf("已扫描 %d 个请求，请求级变化 %d，源级变化 %d\n", scanned, reqChanged, srcChanged)
	}
	if !apply && (reqChanged > 0 || srcChanged > 0) {
		fmt.Println("只统计未写库；确认无误后加 APPLY=1 重跑。")
	}
}

func loadRequests(ctx context.Context, conn *pgx.Conn, after int64, limit int, all bool, from, to *time.Time) ([]reqRow, error) {
	rows, err := conn.Query(ctx, `
SELECT id, request_id, created_at, inv_amt_12m::text, tax_amt_12m::text, inv_zero, tax_zero, rule_version
FROM request_payload
WHERE id > $1
  AND ($2 OR rule_version IS DISTINCT FROM $3)
  AND ($4::timestamptz IS NULL OR created_at >= $4)
  AND ($5::timestamptz IS NULL OR created_at < $5)
ORDER BY id
LIMIT $6`, after, all, zerorule.Version, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reqRow
	for rows.Next() {
		var r reqRow
		var rv *string
		if err := rows.Scan(&r.id, &r.requestID, &r.at, &r.verdict.inv, &r.verdict.tax, &r.verdict.invZero, &r.verdict.taxZero, &rv); err != nil {
			return nil, err
		}
		r.verdict.inv, r.verdict.tax = normalize(r.verdict.inv), normalize(r.verdict.tax)
		if rv != nil {
			r.verdict.ruleVersion = *rv
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadSources(ctx context.Context, conn *pgx.Conn, ids []string) (map[string][]srcRow, error) {
	rows, err := conn.Query(ctx, `
SELECT p.request_id, p.source_label, p.status, p.raw_data::text,
       COALESCE(c.dim_invoice, FALSE), COALESCE(c.dim_tax, FALSE),
       p.inv_amt_12m::text, p.tax_amt_12m::text, p.inv_zero, p.tax_zero, p.rule_version
FROM upstream_payload p
LEFT JOIN upstream_call c ON c.request_id = p.request_id AND c.source_label = p.source_label
WHERE p.request_id = ANY($1)
ORDER BY p.request_id, p.id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]srcRow{}
	for rows.Next() {
		var s srcRow
		var rawText, rv *string
		if err := rows.Scan(&s.requestID, &s.label, &s.status, &rawText, &s.got.Invoice, &s.got.Tax,
			&s.verdict.inv, &s.verdict.tax, &s.verdict.invZero, &s.verdict.taxZero, &rv); err != nil {
			return nil, err
		}
		if rawText != nil {
			s.raw = []byte(*rawText)
		}
		s.verdict.inv, s.verdict.tax = normalize(s.verdict.inv), normalize(s.verdict.tax)
		if rv != nil {
			s.verdict.ruleVersion = *rv
		}
		out[s.requestID] = append(out[s.requestID], s)
	}
	return out, rows.Err()
}

func write(ctx context.Context, conn *pgx.Conn, reqs []reqRow, srcs []srcRow) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, s := range srcs {
			if _, err := tx.Exec(ctx, `
UPDATE upstream_payload
SET inv_amt_12m = $3::text::numeric, tax_amt_12m = $4::text::numeric, inv_zero = $5, tax_zero = $6, rule_version = $7
WHERE request_id = $1 AND source_label = $2`,
				s.requestID, s.label, s.verdict.inv, s.verdict.tax, s.verdict.invZero, s.verdict.taxZero, s.verdict.ruleVersion); err != nil {
				return err
			}
		}
		for _, r := range reqs {
			if _, err := tx.Exec(ctx, `
UPDATE request_payload
SET inv_amt_12m = $2::text::numeric, tax_amt_12m = $3::text::numeric, inv_zero = $4, tax_zero = $5, rule_version = $6
WHERE id = $1`,
				r.id, r.verdict.inv, r.verdict.tax, r.verdict.invZero, r.verdict.taxZero, r.verdict.ruleVersion); err != nil {
				return err
			}
		}
		return nil
	})
}
