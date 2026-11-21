package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/testdb"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(context.Background(), testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestTokenDeduplicationAndNameCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
	now := time.Now()
	first, err := database.AddToken(ctx, "sk-abcd****5678", []byte("hash-1"), []byte("cipher-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AddToken(ctx, first.Name, []byte("hash-1"), []byte("cipher-2"), now); !errors.Is(err, ErrDuplicateToken) {
		t.Fatalf("重复令牌错误 = %v，期望 ErrDuplicateToken", err)
	}
	// 打码碰撞不等于令牌重复，应允许添加并通过 ID 管理。
	if _, err := database.AddToken(ctx, first.Name, []byte("hash-2"), []byte("cipher-3"), now); err != nil {
		t.Fatalf("不同令牌仅名称碰撞时不应被误判重复: %v", err)
	}
	if _, err := database.RemoveToken(ctx, first.Name); !errors.Is(err, ErrAmbiguousToken) {
		t.Fatalf("名称碰撞时删除错误 = %v，期望 ErrAmbiguousToken", err)
	}
	if _, err := database.SetTokenRemark(ctx, first.Name, "生产环境"); !errors.Is(err, ErrAmbiguousToken) {
		t.Fatalf("名称碰撞时设置备注错误 = %v，期望 ErrAmbiguousToken", err)
	}
	if _, err := database.RemoveToken(ctx, "1"); err != nil {
		t.Fatalf("按 ID 删除失败: %v", err)
	}
	tokens, err := database.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].ID == first.ID {
		t.Fatalf("删除后的令牌列表不符合预期: %+v", tokens)
	}
}

func TestTokenRemark(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
	token, err := database.AddToken(ctx, "sk-abcd****5678", []byte("hash"), []byte("cipher"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if token.Remark != "" {
		t.Fatalf("新令牌备注应为空，实际 = %q", token.Remark)
	}

	updated, err := database.SetTokenRemark(ctx, strconv.FormatInt(token.ID, 10), "  生产环境主账号  ")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Remark != "生产环境主账号" {
		t.Fatalf("设置后的备注 = %q", updated.Remark)
	}
	tokens, err := database.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Remark != "生产环境主账号" {
		t.Fatalf("备注未持久化: %+v", tokens)
	}

	updated, err = database.SetTokenRemark(ctx, token.Name, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Remark != "" {
		t.Fatalf("空白备注应清除已有内容，实际 = %q", updated.Remark)
	}
	if _, err := database.SetTokenRemark(ctx, "999", "备注"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("不存在的令牌设置备注错误 = %v，期望 ErrTokenNotFound", err)
	}
}

func TestOpenMigratesLegacyTokensTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	legacy, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`
		CREATE TABLE tokens (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			token_hash BYTEA NOT NULL CONSTRAINT uni_tokens_token_hash UNIQUE,
			token_ciphertext BYTEA NOT NULL,
			created_at BIGINT NOT NULL
		)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(
		"INSERT INTO tokens(name, token_hash, token_ciphertext, created_at) VALUES (?, ?, ?, ?)",
		"sk-abcd****5678", []byte("hash"), []byte("cipher"), time.Now().UnixMilli(),
	).Error; err != nil {
		t.Fatal(err)
	}
	legacySQLDB, err := legacy.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacySQLDB.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("打开旧数据库并迁移失败: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	tokens, err := database.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Name != "sk-abcd****5678" || tokens[0].Remark != "" {
		t.Fatalf("旧令牌迁移后数据错误: %+v", tokens)
	}
	if _, err := database.SetTokenRemark(ctx, "1", "迁移后备注"); err != nil {
		t.Fatalf("迁移后设置备注失败: %v", err)
	}
}

func TestFailureDoesNotBreakSuccessfulHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openTestStore(t)
	token, err := database.AddToken(ctx, "sk-abcd****5678", []byte("hash"), []byte("cipher"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	firstBalance := model.Balance{AvailableMicros: 50_000_000, VoucherMicros: 20_000_000, CashMicros: 30_000_000}
	previous, err := database.RecordSuccess(ctx, token.ID, firstTime, firstBalance)
	if err != nil {
		t.Fatal(err)
	}
	if previous != nil {
		t.Fatal("首次成功查询不应有上一次快照")
	}
	secondTime := firstTime.Add(10 * time.Minute)
	secondBalance := model.Balance{AvailableMicros: 40_000_000}
	previous, err = database.RecordSuccess(ctx, token.ID, secondTime, secondBalance)
	if err != nil {
		t.Fatal(err)
	}
	if previous == nil || previous.Balance.AvailableMicros != firstBalance.AvailableMicros {
		t.Fatalf("第二次查询的上一次快照错误: %+v", previous)
	}
	if err := database.RecordFailure(ctx, token.ID, secondTime.Add(10*time.Minute), "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	thirdTime := secondTime.Add(20 * time.Minute)
	thirdBalance := model.Balance{AvailableMicros: 35_000_000}
	previous, err = database.RecordSuccess(ctx, token.ID, thirdTime, thirdBalance)
	if err != nil {
		t.Fatal(err)
	}
	if previous == nil || !previous.CheckedAt.Equal(secondTime) || previous.Balance.AvailableMicros != secondBalance.AvailableMicros {
		t.Fatalf("异常记录不应被当作 0 余额或上一次成功记录: %+v", previous)
	}
}

func TestSuccessfulHistorySurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if database != nil {
			_ = database.Close()
		}
	})
	token, err := database.AddToken(ctx, "sk-abcd****5678", []byte("hash"), []byte("cipher"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	firstBalance := model.Balance{AvailableMicros: 50_000_000, VoucherMicros: 20_000_000, CashMicros: 30_000_000}
	if previous, err := database.RecordSuccess(ctx, token.ID, firstTime, firstBalance); err != nil || previous != nil {
		t.Fatalf("首次查询记录错误: previous=%+v, err=%v", previous, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := database.RecordSuccess(ctx, token.ID, firstTime.Add(10*time.Minute), firstBalance)
	if err != nil {
		t.Fatal(err)
	}
	if previous == nil || !previous.CheckedAt.Equal(firstTime) || previous.Balance != firstBalance {
		t.Fatalf("重新打开数据库后未恢复上一次成功快照: %+v", previous)
	}
}
