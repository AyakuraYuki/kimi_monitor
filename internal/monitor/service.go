package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
)

// BalanceClient 抽象 Kimi 查询接口，便于测试状态变化。
type BalanceClient interface {
	FetchBalance(ctx context.Context, token string) (model.Balance, error)
}

// Store 抽象监控所需的持久化操作。
type Store interface {
	ListTokens(ctx context.Context) ([]model.Token, error)
	RecordSuccess(ctx context.Context, tokenID int64, checkedAt time.Time, balance model.Balance) (*model.Snapshot, error)
	RecordFailure(ctx context.Context, tokenID int64, checkedAt time.Time, message string) error
}

// Service 执行单轮全量余额查询。
type Service struct {
	Store       Store
	API         BalanceClient
	Cipher      *secrets.Cipher
	MaxParallel int
	Now         func() time.Time
}

// CheckAll 并发查询全部令牌，返回顺序与数据库中的令牌 ID 顺序一致。
func (s *Service) CheckAll(ctx context.Context) ([]model.TokenResult, error) {
	tokens, err := s.Store.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return []model.TokenResult{}, nil
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	checkedAt := now().UTC()
	parallel := s.MaxParallel
	if parallel <= 0 {
		parallel = 4
	}
	if parallel > len(tokens) {
		parallel = len(tokens)
	}

	results := make([]model.TokenResult, len(tokens))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index] = s.checkOne(ctx, tokens[index], checkedAt)
			}
		}()
	}
	for index := range tokens {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	return results, nil
}

func (s *Service) checkOne(ctx context.Context, token model.Token, checkedAt time.Time) model.TokenResult {
	result := model.TokenResult{TokenID: token.ID, Name: token.Name}
	plaintext, err := s.Cipher.Decrypt(token.Ciphertext)
	if err == nil {
		var balance model.Balance
		balance, err = s.API.FetchBalance(ctx, plaintext)
		if err == nil {
			var previous *model.Snapshot
			previous, err = s.Store.RecordSuccess(ctx, token.ID, checkedAt, balance)
			if err == nil {
				result.Current = &model.Snapshot{TokenID: token.ID, CheckedAt: checkedAt, Balance: balance}
				result.Previous = previous
				return result
			}
		}
	}

	result.Err = err
	if recordErr := s.Store.RecordFailure(ctx, token.ID, checkedAt, err.Error()); recordErr != nil {
		result.Err = errors.Join(err, recordErr)
	}
	return result
}

// Run 按固定间隔执行查询和整体推送；启动后立即执行第一轮。
func Run(ctx context.Context, service *Service, notifier interface {
	SendMarkdown(ctx context.Context, content string) error
}, interval time.Duration, lowThresholdMicros int64, logf func(format string, args ...any)) error {
	if interval <= 0 {
		return fmt.Errorf("查询间隔必须大于 0")
	}
	runRound := func() error {
		results, err := service.CheckAll(ctx)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			logf("没有已配置的令牌，本轮跳过推送")
			return nil
		}
		messages := BuildMessages(results, time.Now(), lowThresholdMicros)
		for index, message := range messages {
			if err := notifier.SendMarkdown(ctx, message); err != nil {
				return fmt.Errorf("推送第 %d/%d 条消息失败: %w", index+1, len(messages), err)
			}
		}
		failures := 0
		for _, result := range results {
			if result.Err != nil {
				failures++
			}
		}
		logf("本轮完成：%d 个令牌，%d 个查询异常，%d 条企业微信消息", len(results), failures, len(messages))
		return nil
	}

	if err := runRound(); err != nil {
		logf("本轮监控失败: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := runRound(); err != nil {
				logf("本轮监控失败: %v", err)
			}
		}
	}
}
