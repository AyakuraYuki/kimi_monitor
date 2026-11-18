package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"kimi_balance_monitor/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(context.Background(), t.TempDir()+"/monitor.db")
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
