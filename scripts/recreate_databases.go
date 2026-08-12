//go:build ignore

// Ensure per-domain databases exist and are migrated. SAFE BY DEFAULT: this
// script NEVER drops or truncates data. For a brand-new database it runs
// CREATE DATABASE (if missing) + ApplyMigrations (idempotent via
// schema_migrations). For an existing database it only applies any pending
// migrations — existing rows (license / audit_log / billing_ledger …) are left
// untouched.
//
// 存储按「域」隔离：v8/v9 共用 v8v9 域库——v8 在 config 中不单列 database，故此处
// 自动跳过，v8v9 域库由 owner 路由 v9 创建。demo license 仅在 SEED_DEMO=1 时播种
// (开发/e2e)，appKey 按域各不相同 (model.DemoAppKey)。
//
// 阿里云 RDS 常禁止普通账号连 postgres 维护库；若 ensureDatabase 失败，请先在
// RDS 控制台手动 CREATE DATABASE，再重跑本脚本。
//
// ⚠️ 破坏性重置（DROP 全部业务表后重建）仅用于**测试库**，默认关闭。必须显式设置
//    RESET_DESTRUCTIVE=1 才会执行，且脚本会**拒绝对生产库执行**（配置文件名含 prod
//    或库名含 _prod_ 即判定为生产）。生产环境永远不要设置该变量。
//
// Usage:
//
//	# 生产：只补建缺失的库并迁移，绝不删数据
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/recreate_databases.go
//
//	# e2e 测试：清空并重建测试库（仅测试库允许）
//	RESET_DESTRUCTIVE=1 SEED_DEMO=1 CONFIG_FILE=config.aliyun.e2e.yaml go run ./scripts/recreate_databases.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/datahub/relay/internal/infrastructure/persistence/postgres"
)

type fileVersion struct {
	Database struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		Name     string `yaml:"name"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		SSLMode  string `yaml:"sslmode"`
		MaxConns int    `yaml:"maxConns"`
	} `yaml:"database"`
}

type fileConfig struct {
	Storage struct {
		MigrationsDir string `yaml:"migrationsDir"`
	} `yaml:"storage"`
	Versions map[string]fileVersion `yaml:"versions"`
}

// versionOrder keeps a deterministic processing order matching model.Versions.
var versionOrder = []string{"swfp"}

const perDBTimeout = 2 * time.Minute

func main() {
	path := os.Getenv("CONFIG_FILE")
	if path == "" {
		path = "config.aliyun.e2e.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("read config: %v", err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		fatal("parse config: %v", err)
	}
	migDir := fc.Storage.MigrationsDir
	if migDir == "" {
		migDir = "migrations"
	}

	// 破坏性重置：默认关闭，且对生产库硬拒绝。
	destructive := os.Getenv("RESET_DESTRUCTIVE") == "1"
	prod := looksLikeProd(path, fc)
	if destructive && prod {
		fatal("REFUSED: RESET_DESTRUCTIVE=1 但目标疑似生产环境 (config=%q)。"+
			"生产库禁止 DROP 重建，请去掉 RESET_DESTRUCTIVE。", path)
	}

	var recreateSQL []byte
	if destructive {
		recreateSQL, err = os.ReadFile("scripts/recreate_schema.sql")
		if err != nil {
			fatal("read recreate_schema.sql: %v", err)
		}
		fmt.Println("!! RESET_DESTRUCTIVE=1: 将 DROP 并重建测试库业务表 !!")
	} else {
		fmt.Println("== 安全模式：只补建缺失的库 + 迁移，不删除任何数据 ==")
	}

	for _, v := range versionOrder {
		fv, ok := fc.Versions[v]
		if !ok || fv.Database.Name == "" {
			fmt.Printf("== %s: no database configured, skipping ==\n", v)
			continue
		}
		dbName := fv.Database.Name
		ctx, cancel := context.WithTimeout(context.Background(), perDBTimeout)
		bootstrap := bootstrapDBName(fc, dbName)
		fmt.Printf("== %s: ensure database %s exists ==\n", v, dbName)
		if err := ensureDatabase(ctx, fv, bootstrap, dbName); err != nil {
			// Fallback message when bootstrap also fails.
			fmt.Printf("  (ensureDatabase warning for %s: %v; assuming it exists — if next step fails, CREATE DATABASE manually in RDS console)\n", dbName, err)
		}
		if destructive {
			fmt.Printf("== %s: [DESTRUCTIVE] drop + recreate tables on %s ==\n", v, dbName)
			if err := execSQL(ctx, dsn(fv, dbName), string(recreateSQL)); err != nil {
				cancel()
				fatal("%s recreate: %v", v, err)
			}
		}
		if err := migrateAndSeed(ctx, fv, v, dbName, migDir); err != nil {
			cancel()
			fatal("%s migrate: %v", v, err)
		}
		cancel()
		fmt.Printf("%s (%s) OK\n", v, dbName)
	}
	if destructive {
		fmt.Println("\nDone. Test databases rebuilt (destructive).")
	} else {
		fmt.Println("\nDone. Missing databases created + migrations applied. 现有数据未改动。")
	}
}

// looksLikeProd 判定目标是否为生产环境，用于硬拒绝破坏性重置。
// 判据：配置文件名含 "prod"，或任一库名含 "_prod_"。
func looksLikeProd(path string, fc fileConfig) bool {
	if strings.Contains(strings.ToLower(path), "prod") {
		return true
	}
	for _, v := range fc.Versions {
		if strings.Contains(v.Database.Name, "_prod_") {
			return true
		}
	}
	return false
}

// bootstrapDBName picks an existing configured database for CREATE DATABASE
// (Aliyun RDS often blocks the postgres maintenance DB).
func bootstrapDBName(fc fileConfig, skip string) string {
	for _, v := range versionOrder {
		fvv, ok := fc.Versions[v]
		if !ok || fvv.Database.Name == "" || fvv.Database.Name == skip {
			continue
		}
		return fvv.Database.Name
	}
	return "postgres"
}

func dsn(fv fileVersion, dbName string) string {
	port := fv.Database.Port
	if port == 0 {
		port = 5432
	}
	ssl := fv.Database.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	maxConns := fv.Database.MaxConns
	if maxConns == 0 {
		maxConns = 10
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10 pool_max_conns=%d",
		fv.Database.Host, port, fv.Database.User, fv.Database.Password, dbName, ssl, maxConns,
	)
}

func execSQL(ctx context.Context, connDSN, sqlText string) error {
	pool, err := pgxpool.New(ctx, connDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, stmt := range splitStatements(sqlText) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%w\nstmt: %s", err, stmt)
		}
	}
	return nil
}

func ensureDatabase(ctx context.Context, fv fileVersion, adminDB, newDB string) error {
	pool, err := pgxpool.New(ctx, dsn(fv, adminDB))
	if err != nil {
		return err
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, newDB,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		fmt.Printf("  database %s already exists\n", newDB)
		return nil
	}
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+quoteIdent(newDB)); err != nil {
		return err
	}
	fmt.Printf("  created database %s\n", newDB)
	return nil
}

func migrateAndSeed(ctx context.Context, fv fileVersion, route, dbName, migDir string) error {
	store, err := postgres.New(ctx, dsn(fv, dbName))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := postgres.ApplyMigrations(ctx, store.Pool(), migDir); err != nil {
		return err
	}
	// demo license 仅开发/e2e 需要（SEED_DEMO=1 时播种）；生产库不播种，
	// 避免公开 secret 的 demo 凭证进入生产。
	if os.Getenv("SEED_DEMO") == "1" {
		return postgres.SeedDemo(ctx, store, route)
	}
	return nil
}

func splitStatements(sqlText string) []string {
	var b strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
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

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
