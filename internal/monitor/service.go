package monitor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"kimi_balance_monitor/internal/config"
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
	SumConsumptionByToken(ctx context.Context, start, end time.Time) (map[int64]int64, error)
}

// Service 执行单轮全量余额查询。
type Service struct {
	Store       Store
	API         BalanceClient
	Cipher      *secrets.Cipher
	MaxParallel int
	Now         func() time.Time
}

// CheckAll 并发查询全部已启用令牌，返回顺序与数据库中的令牌 ID 顺序一致。
func (s *Service) CheckAll(ctx context.Context) ([]model.TokenResult, error) {
	tokens, err := s.Store.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	tokens = slices.DeleteFunc(tokens, func(token model.Token) bool {
		return !token.MonitoringEnabled
	})
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
	parallel = min(parallel, len(tokens))

	results := make([]model.TokenResult, len(tokens))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range parallel {
		workers.Go(func() {
			for index := range jobs {
				results[index] = s.checkOne(ctx, tokens[index], checkedAt)
			}
		})
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
	result := model.TokenResult{
		TokenID: token.ID,
		Name:    token.Name,
		Remark:  token.Remark,
	}
	plaintext, err := s.Cipher.Decrypt(token.Ciphertext)
	if err == nil {
		var balance model.Balance
		balance, err = s.API.FetchBalance(ctx, plaintext)
		if err == nil {
			var previous *model.Snapshot
			previous, err = s.Store.RecordSuccess(ctx, token.ID, checkedAt, balance)
			if err == nil {
				result.Current = &model.Snapshot{
					TokenID:   token.ID,
					CheckedAt: checkedAt,
					Balance:   balance,
				}
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

// BuildReport 从 PostgreSQL 汇总北京时间当日消耗，并在指定时段同时汇总昨日消耗。
func (s *Service) BuildReport(ctx context.Context, results []model.TokenResult, checkedAt time.Time, lowThresholdMicros int64) ([]string, error) {
	localTime := checkedAt.In(config.CST())
	year, month, day := localTime.Date()
	todayStart := time.Date(year, month, day, 0, 0, 0, 0, config.CST())
	todayByToken, err := s.Store.SumConsumptionByToken(ctx, todayStart, checkedAt)
	if err != nil {
		return nil, fmt.Errorf("统计今日消耗失败: %w", err)
	}
	for index := range results {
		results[index].TodayConsumptionMicros = todayByToken[results[index].TokenID]
	}
	consumption := model.ConsumptionSummary{TodayMicros: totalConsumption(todayByToken)}
	if shouldIncludeYesterday(checkedAt) {
		yesterdayStart := todayStart.AddDate(0, 0, -1)
		yesterdayByToken, err := s.Store.SumConsumptionByToken(ctx, yesterdayStart, todayStart.Add(-time.Millisecond))
		if err != nil {
			return nil, fmt.Errorf("统计昨日消耗失败: %w", err)
		}
		for index := range results {
			results[index].YesterdayConsumptionMicros = yesterdayByToken[results[index].TokenID]
		}
		consumption.YesterdayMicros = totalConsumption(yesterdayByToken)
	}
	return BuildMessages(results, checkedAt, lowThresholdMicros, consumption), nil
}

func totalConsumption(byToken map[int64]int64) int64 {
	var total int64
	for amount := range maps.Values(byToken) {
		total += amount
	}
	return total
}

// RunOnce 与持续监控共享同一轮采集、推送和通知留存逻辑。
func RunOnce(ctx context.Context, service *Service, delivery *Delivery, lowThresholdMicros int64, logger *zerolog.Logger) error {
	results, err := service.CheckAll(ctx)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		logger.Info().Msg("没有已启用额度查询的令牌，本轮跳过推送")
		return nil
	}
	client, err := delivery.client(ctx)
	if err != nil {
		return err
	}
	if client == nil {
		logger.Info().Int("token_count", len(results)).Msg("本轮采集完成，尚未配置企业微信通知")
		return nil
	}
	messages, err := service.BuildReport(ctx, results, time.Now(), lowThresholdMicros)
	if err != nil {
		return err
	}
	if err := delivery.send(ctx, client, messages); err != nil {
		return err
	}
	failures := 0
	for _, result := range results {
		if result.Err != nil {
			failures++
		}
	}
	logger.Info().
		Int("token_count", len(results)).
		Int("failure_count", failures).
		Int("message_count", len(messages)).
		Msg("本轮完成")
	return nil
}

// Run 按固定间隔执行查询和整体推送；启动后立即执行第一轮。
func Run(ctx context.Context, service *Service, delivery *Delivery, interval time.Duration, lowThresholdMicros int64, logger *zerolog.Logger) error {
	if interval <= 0 {
		return fmt.Errorf("查询间隔必须大于 0")
	}
	run := func() {
		if err := RunOnce(ctx, service, delivery, lowThresholdMicros, logger); err != nil {
			logger.Error().Err(err).Msg("本轮监控失败")
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}
