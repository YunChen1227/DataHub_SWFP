package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/datahub/relay/internal/domain/model"
)

// ApplyMigrations runs every *.sql under dir exactly once, tracked in a
// schema_migrations table. Each file is split into individual statements on ';'
// (comment lines starting with '--' are stripped first) because pgx's extended
// protocol executes a single statement per Exec.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	// Best-effort: managed PG (e.g. Aliyun accounts in pg_rds_superuser) often
	// ships PG15+ where the `public` schema lacks CREATE for the app account.
	// Self-grant when permitted; ignored elsewhere (real DDL errors still surface).
	_, _ = pool.Exec(ctx, `GRANT CREATE ON SCHEMA public TO CURRENT_USER`)

	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		for _, stmt := range splitStatements(string(raw)) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("apply %s: %w", name, err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}

// splitStatements strips '--' comments and splits SQL on ';'. 行尾注释必须一并剥离
// 再切分：注释里的 ';'（如列说明 "(1 起; skipped 为 0)"）否则会把 CREATE TABLE
// 截成两半，前半段未闭合，报 "syntax error at end of input" (42601)。
func splitStatements(sqlText string) []string {
	var b strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		b.WriteString(stripLineComment(line))
		b.WriteString("\n")
	}
	var out []string
	for _, part := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stripLineComment drops a trailing '--' comment. 单引号字符串内的 '--' 不算注释
// （'' 是 SQL 的转义写法，连续两次翻转状态即为正确处理）。
func stripLineComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\'':
			inQuote = !inQuote
		case !inQuote && line[i] == '-' && i+1 < len(line) && line[i+1] == '-':
			return line[:i]
		}
	}
	return line
}

// SeedDemo inserts the 域's dev demo license idempotently so the e2e/admin
// flows have a known client. 仅供开发/e2e 建库脚本调用（或生产显式设置
// demo.seed=true，不推荐）；relay 默认不在生产播种 demo。demo appKey 按域各
// 不相同 (model.DemoAppKey)，demo 凭证无法跨域使用；v8/v9 同属 v8v9 域，共用
// 一个。不用 ON CONFLICT，避免旧库/迁移后约束名不一致导致 42P10。Mirrors memory
// seedDemo (cmd/relay/main.go)。
func SeedDemo(ctx context.Context, s *Store, route string) error {
	const insLicense = `INSERT INTO license
		(license_id, app_key, app_secret_enc, client_uuid, name, mobile, status, valid_from, valid_to, secret_created_at)
		SELECT $1, $2, 'demo-app-secret', $3, $4, '13800001234', 'ACTIVE',
			now(), now() + interval '3650 days', now()
		WHERE NOT EXISTS (SELECT 1 FROM license WHERE license_id = $1)`
	domain := model.RouteDomain(route)
	up := strings.ToUpper(domain)
	if _, err := s.pool.Exec(ctx, insLicense,
		"LIC-DEMO-"+up, model.DemoAppKey(route), "demo-client-"+domain, "Demo 商户("+up+")",
	); err != nil {
		return err
	}
	// 计数行 (license, route, dim) 由首次累加时 UPSERT 按需创建，无需预插。
	return nil
}
