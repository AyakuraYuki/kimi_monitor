// Package testdb 为集成测试提供相互隔离的 PostgreSQL schema。
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const DSNEnv = "KIMI_MONITOR_TEST_POSTGRES_DSN"

// DSN 创建独立 schema，并返回将其作为 search_path 的测试 DSN。
func DSN(t *testing.T) string {
	t.Helper()
	baseDSN := strings.TrimSpace(os.Getenv(DSNEnv))
	if baseDSN == "" {
		t.Skipf("设置 %s 后运行 PostgreSQL 集成测试", DSNEnv)
	}

	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("生成测试 schema 名称失败: %v", err)
	}
	schema := fmt.Sprintf("kimi_monitor_test_%x", random[:])

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("打开 PostgreSQL 测试连接失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("创建 PostgreSQL 测试 schema 失败: %v", err)
	}
	if err := admin.Close(); err != nil {
		t.Fatalf("关闭 PostgreSQL 测试管理连接失败: %v", err)
	}

	t.Cleanup(func() {
		admin, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Errorf("打开 PostgreSQL 清理连接失败: %v", err)
			return
		}
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("清理 PostgreSQL 测试 schema 失败: %v", err)
		}
	})

	dsn, err := withSearchPath(baseDSN, schema)
	if err != nil {
		t.Fatalf("生成 PostgreSQL 测试 DSN 失败: %v", err)
	}
	return dsn
}

func withSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	return strings.TrimSpace(dsn) + " search_path=" + schema, nil
}
