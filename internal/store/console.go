package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"kimi_balance_monitor/internal/config"
)

type Overview struct {
	Tokens       []TokenOverview
	Consumption  map[int64]int64
	Notification *Notification
}

// Overview reads the balances and their daily totals from the same PostgreSQL snapshot.
func (s *Store) Overview(ctx context.Context, start, now time.Time) (Overview, error) {
	var overview Overview
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		view := &Store{db: tx}
		var err error
		if overview.Tokens, err = view.LatestTokens(ctx); err != nil {
			return err
		}
		if overview.Consumption, err = view.SumConsumptionByToken(ctx, start, now); err != nil {
			return err
		}
		overview.Notification, err = view.LatestNotification(ctx)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return overview, err
}

// TokenOverview 不读取凭据。最近尝试的错误与最近一次成功查询的金额各有自己的时间戳。
type TokenOverview struct {
	ID              int64
	Name            string
	Remark          string
	CreatedAt       time.Time
	CheckedAt       *time.Time
	Error           string
	BalanceAt       *time.Time
	AvailableMicros *int64
	VoucherMicros   *int64
	CashMicros      *int64
}

func (s *Store) LatestTokens(ctx context.Context) ([]TokenOverview, error) {
	const query = `
SELECT t.id, t.name, t.remark, to_timestamp(t.created_at / 1000.0) AS created_at,
	to_timestamp(attempt.checked_at / 1000.0) AS checked_at, coalesce(attempt.error, '') AS error,
	to_timestamp(balance.checked_at / 1000.0) AS balance_at,
	balance.available_micros, balance.voucher_micros, balance.cash_micros
FROM tokens t
LEFT JOIN LATERAL (
	SELECT checked_at, error FROM balance_records WHERE token_id = t.id ORDER BY id DESC LIMIT 1
) attempt ON TRUE
LEFT JOIN LATERAL (
	SELECT checked_at, available_micros, voucher_micros, cash_micros
	FROM balance_records WHERE token_id = t.id AND error = '' ORDER BY id DESC LIMIT 1
) balance ON TRUE
ORDER BY t.id`
	rows := []TokenOverview{}
	if err := s.db.WithContext(ctx).Raw(query).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取令牌监控概览失败: %w", err)
	}
	return rows, nil
}

type DailyRow struct {
	Date              string
	TokenID           int64
	Name              string
	Remark            string
	ConsumptionMicros *int64 // 无成功采样时未知；有采样且未观察到下降时为零。
}

// DailyConsumption 查询北京时间自然日区间 [start, endExclusive)，包括注册日至今的无采样日期。
func (s *Store) DailyConsumption(ctx context.Context, start, endExclusive time.Time) ([]DailyRow, error) {
	const query = `
WITH changes AS (` + snapshotChanges + `), totals AS (
	SELECT token_id, (to_timestamp(checked_at / 1000.0) AT TIME ZONE 'Asia/Shanghai')::date AS day,
		SUM(consumed)::bigint AS consumption_micros
	FROM changes GROUP BY token_id, day
), days AS (
	SELECT day::date FROM generate_series(?::date::timestamp,
		LEAST(?::date, (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Shanghai')::date)::timestamp,
		interval '1 day') AS day
)
SELECT to_char(days.day, 'YYYY-MM-DD') AS date, t.id AS token_id, t.name, t.remark, totals.consumption_micros
FROM days JOIN tokens t ON (to_timestamp(t.created_at / 1000.0) AT TIME ZONE 'Asia/Shanghai')::date <= days.day
LEFT JOIN totals ON totals.token_id = t.id AND totals.day = days.day
ORDER BY days.day DESC, t.id`
	end := endExclusive.Add(-time.Millisecond)
	rows := []DailyRow{}
	if err := s.db.WithContext(ctx).Raw(query, start.UnixMilli(), end.UnixMilli(),
		start.In(config.CST()).Format(time.DateOnly), end.In(config.CST()).Format(time.DateOnly)).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取每日消耗失败: %w", err)
	}
	return rows, nil
}

// webhook_settings 只保存界面配置的覆盖值。缺少该行时使用 YAML 中的设置。
type webhookSetting struct {
	ID         int    `gorm:"primaryKey;autoIncrement:false"`
	Ciphertext []byte `gorm:"not null"`
}

func (s *Store) WebhookOverride(ctx context.Context) ([]byte, error) {
	var row webhookSetting
	err := s.db.WithContext(ctx).Take(&row, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取企业微信配置失败: %w", err)
	}
	return row.Ciphertext, nil
}

func (s *Store) SetWebhookOverride(ctx context.Context, ciphertext []byte) error {
	db := s.db.WithContext(ctx)
	if len(ciphertext) == 0 {
		return db.Delete(&webhookSetting{}, 1).Error
	}
	return db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"ciphertext"}),
	}).Create(&webhookSetting{ID: 1, Ciphertext: ciphertext}).Error
}

// 同一轮报告的分片共享批次号，每片由企业微信确认后单独插入。
type notificationPart struct {
	ID      int64     `gorm:"primaryKey"`
	Batch   string    `gorm:"not null;uniqueIndex:idx_notification_batch_part"`
	Part    int       `gorm:"not null;uniqueIndex:idx_notification_batch_part"`
	Total   int       `gorm:"not null"`
	SentAt  time.Time `gorm:"not null"`
	Content string    `gorm:"not null"`
}

func (s *Store) RecordNotificationPart(ctx context.Context, batch string, part, total int, text string, sentAt time.Time) error {
	row := notificationPart{
		Batch:   batch,
		Part:    part,
		Total:   total,
		Content: text,
		SentAt:  sentAt,
	}
	return s.db.WithContext(ctx).Create(&row).Error
}

type Notification struct {
	SentAt    time.Time
	Text      string
	Delivered int
	Total     int
}

func (s *Store) LatestNotification(ctx context.Context) (*Notification, error) {
	const query = `SELECT max(sent_at) AS sent_at, string_agg(content, E'\n\n' ORDER BY part) AS text,
		count(*) AS delivered, max(total) AS total
	FROM notification_parts WHERE batch = (SELECT batch FROM notification_parts ORDER BY id DESC LIMIT 1)
	GROUP BY batch`
	var result Notification
	tx := s.db.WithContext(ctx).Raw(query).Scan(&result)
	if tx.Error != nil {
		return nil, fmt.Errorf("读取最近一次企业微信通知失败: %w", tx.Error)
	}
	if tx.RowsAffected == 0 {
		return nil, nil
	}
	return &result, nil
}
