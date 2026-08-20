package postgres

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSplitStatementsStripsInlineComments 覆盖历史事故：行尾 '--' 注释未被剥离时，
// 注释文本里的 ';' 会把语句从中间切断，上线表现为 relay 启动即崩、
// "apply 0007_...: ERROR: syntax error at end of input (SQLSTATE 42601)"。
func TestSplitStatementsStripsInlineComments(t *testing.T) {
	const sql = `CREATE TABLE t (
    seq  INT NOT NULL DEFAULT 0,   -- 调用顺序 (1 起; skipped 为 0)
    name TEXT NOT NULL
);`
	got := splitStatements(sql)
	if len(got) != 1 {
		t.Fatalf("want 1 statement, got %d: %q", len(got), got)
	}
	if strings.Contains(got[0], "--") {
		t.Errorf("comment not stripped: %q", got[0])
	}
	if strings.Count(got[0], "(") != strings.Count(got[0], ")") {
		t.Errorf("unbalanced parens (statement was truncated): %q", got[0])
	}
}

// TestSplitStatementsKeepsQuotedDashes 保证字符串字面量里的 '--' 不被误当注释。
func TestSplitStatementsKeepsQuotedDashes(t *testing.T) {
	got := splitStatements(`INSERT INTO t (v) VALUES ('a--b');`)
	if len(got) != 1 || !strings.Contains(got[0], "'a--b'") {
		t.Fatalf("quoted dashes must survive, got %q", got)
	}
}

// TestMigrationsSplitIntoBalancedStatements 是本仓 migrations/ 的守门测试：每个
// 迁移文件切分后的每条语句都必须括号与引号闭合。新增迁移若在行尾注释里写了 ';'
// (或其它导致截断的写法)，在此处就会失败，而不是等到生产启动时才炸。
func TestMigrationsSplitIntoBalancedStatements(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var seen int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		seen++
		for i, stmt := range splitStatements(string(raw)) {
			if strings.Count(stmt, "(") != strings.Count(stmt, ")") {
				t.Errorf("%s stmt#%d 括号不闭合（语句被截断）:\n%s", e.Name(), i, stmt)
			}
			if strings.Count(stmt, "'")%2 != 0 {
				t.Errorf("%s stmt#%d 单引号不闭合:\n%s", e.Name(), i, stmt)
			}
			if strings.Contains(stmt, "--") {
				t.Errorf("%s stmt#%d 仍残留注释:\n%s", e.Name(), i, stmt)
			}
		}
	}
	if seen == 0 {
		t.Fatalf("no migration files found under %s", dir)
	}
}
