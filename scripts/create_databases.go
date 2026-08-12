//go:build ignore

// Create missing PostgreSQL databases listed in config (no DROP / no migrate).
// 阿里云 RDS 常无法连接 postgres 维护库；本脚本改连 config 里已有的库
// (默认 swfp 的 database.name) 执行 CREATE DATABASE。
//
// Usage:
//
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/create_databases.go
//	CONFIG_FILE=config.aliyun.prod.yaml go run ./scripts/create_databases.go zlf blk
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
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
	Versions map[string]fileVersion `yaml:"versions"`
}

var versionOrder = []string{"swfp"}

const perDBTimeout = 2 * time.Minute

func main() {
	path := os.Getenv("CONFIG_FILE")
	if path == "" {
		path = "config.aliyun.prod.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("read config: %v", err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		fatal("parse config: %v", err)
	}

	only := map[string]struct{}{}
	if len(os.Args) > 1 {
		for _, a := range os.Args[1:] {
			only[a] = struct{}{}
		}
	}

	seen := map[string]struct{}{}
	for _, v := range versionOrder {
		if len(only) > 0 {
			if _, ok := only[v]; !ok {
				continue
			}
		}
		fvv, ok := fc.Versions[v]
		if !ok || fvv.Database.Name == "" {
			fmt.Printf("== %s: no database.name, skip ==\n", v)
			continue
		}
		dbName := fvv.Database.Name
		if _, dup := seen[dbName]; dup {
			fmt.Printf("== %s: %s already scheduled, skip ==\n", v, dbName)
			continue
		}
		seen[dbName] = struct{}{}

		ctx, cancel := context.WithTimeout(context.Background(), perDBTimeout)
		bootstrap := bootstrapDBName(fc, dbName)
		fmt.Printf("== %s: ensure database %s (via bootstrap %s) ==\n", v, dbName, bootstrap)
		if err := ensureDatabase(ctx, fvv, bootstrap, dbName); err != nil {
			cancel()
			fatal("%s: %v", dbName, err)
		}
		cancel()
	}
	fmt.Println("\nDone.")
}

// bootstrapDBName picks an existing configured database to connect for CREATE DATABASE
// (Aliyun RDS often blocks the postgres maintenance DB). Skips skip (the target).
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
		maxConns = 2
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=15 pool_max_conns=%d",
		fv.Database.Host, port, fv.Database.User, fv.Database.Password, dbName, ssl, maxConns,
	)
}

func ensureDatabase(ctx context.Context, fv fileVersion, bootstrapDB, newDB string) error {
	// 直接连一个已存在的库 (bootstrap)，通过 pg_database catalog 判断目标库是否存在，
	// 不存在则 CREATE。不去 ping 目标库本身——RDS 上连不存在的库会长时间挂起超时。
	// ECS→RDS 网络偶发抖动，故对 bootstrap 连接做有限重试。
	pool, err := connectWithRetry(ctx, dsn(fv, bootstrapDB), bootstrapDB, 5)
	if err != nil {
		return err
	}
	defer pool.Close()

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, newDB,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check exists: %w", err)
	}
	if exists {
		fmt.Printf("  database %s already exists\n", newDB)
		return nil
	}

	if _, err := pool.Exec(ctx, "CREATE DATABASE "+quoteIdent(newDB)); err != nil {
		return fmt.Errorf("CREATE DATABASE: %w (need CREATEDB / RDS 高权限账号，或在控制台手动建库)", err)
	}
	fmt.Printf("  created database %s\n", newDB)
	return nil
}

// connectWithRetry opens a pool and pings it, retrying transient failures with
// backoff (ECS→RDS 偶发 dial timeout)。
func connectWithRetry(ctx context.Context, dsn, label string, tries int) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 1; i <= tries; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			lastErr = fmt.Errorf("connect %s: %w", label, err)
		} else {
			pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				return pool, nil
			}
			pool.Close()
			lastErr = fmt.Errorf("ping %s: %w", label, err)
		}
		fmt.Printf("  connect attempt %d/%d failed: %v\n", i, tries, lastErr)
		if i < tries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i) * 2 * time.Second):
			}
		}
	}
	return nil, lastErr
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
