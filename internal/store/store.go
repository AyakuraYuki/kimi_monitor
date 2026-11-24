package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"kimi_balance_monitor/internal/model"
)

var (
	ErrDuplicateToken = errors.New("令牌已存在，不可重复添加")
	ErrTokenNotFound  = errors.New("令牌不存在")
	ErrAmbiguousToken = errors.New("打码名称对应多个令牌，请使用 ID 操作")
)

type tokenRecord struct {
	ID              int64  `gorm:"primaryKey;autoIncrement"`
	Name            string `gorm:"not null"`
	TokenHash       []byte `gorm:"not null;uniqueIndex"`
	TokenCiphertext []byte `gorm:"not null"`
	Remark          string `gorm:"not null;default:''"`
	CreatedAt       int64  `gorm:"not null"`
}

func (tokenRecord) TableName() string {
	return "tokens"
}

type balanceRecord struct {
	ID              int64 `gorm:"primaryKey;autoIncrement;index:idx_balance_records_latest_success,where:error = '',sort:desc,priority:2"`
	TokenID         int64 `gorm:"not null;index:idx_balance_records_latest_success,priority:1"`
	CheckedAt       int64 `gorm:"not null;index:idx_balance_records_checked_at"`
	AvailableMicros *int64
	VoucherMicros   *int64
	CashMicros      *int64
	Error           string       `gorm:"not null;default:''"`
	Token           *tokenRecord `gorm:"foreignKey:TokenID;constraint:OnDelete:CASCADE"`
}

func (balanceRecord) TableName() string {
	return "balance_records"
}

// Store 使用 GORM 将令牌密文和所有查询记录持久化到 PostgreSQL。
type Store struct {
	db *gorm.DB
}

// Open 连接 PostgreSQL 并执行幂等表结构迁移。
func Open(ctx context.Context, dsn string) (*Store, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("PostgreSQL DSN 不能为空")
	}

	gormDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL 失败: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 PostgreSQL 连接失败: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("连接 PostgreSQL 失败: %w", err))
	}
	if err := gormDB.WithContext(ctx).AutoMigrate(&tokenRecord{}, &balanceRecord{}); err != nil {
		return closeOnError(fmt.Errorf("迁移数据库结构失败: %w", err))
	}
	return &Store{db: gormDB}, nil
}

func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// AddToken 添加令牌；token_hash 的唯一索引保证不会重复。
func (s *Store) AddToken(ctx context.Context, name string, digest, ciphertext []byte, now time.Time) (model.Token, error) {
	db := s.db.WithContext(ctx)
	var existing tokenRecord
	err := db.Select("id").Where("token_hash = ?", digest).Take(&existing).Error
	if err == nil {
		return model.Token{}, ErrDuplicateToken
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Token{}, fmt.Errorf("检查令牌是否重复失败: %w", err)
	}

	record := tokenRecord{
		Name:            name,
		TokenHash:       digest,
		TokenCiphertext: ciphertext,
		CreatedAt:       now.UTC().UnixMilli(),
	}
	if err := db.Create(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(strings.ToLower(err.Error()), "unique") {
			return model.Token{}, ErrDuplicateToken
		}
		return model.Token{}, fmt.Errorf("保存令牌失败: %w", err)
	}
	return tokenFromRecord(record), nil
}

// ListTokens 返回全部令牌，顺序固定便于生成报告。
func (s *Store) ListTokens(ctx context.Context) ([]model.Token, error) {
	var records []tokenRecord
	if err := s.db.WithContext(ctx).Order("id").Find(&records).Error; err != nil {
		return nil, fmt.Errorf("读取令牌列表失败: %w", err)
	}
	tokens := make([]model.Token, 0, len(records))
	for _, record := range records {
		tokens = append(tokens, tokenFromRecord(record))
	}
	return tokens, nil
}

// RemoveToken 支持使用数字 ID 或完整打码名称删除令牌。
func (s *Store) RemoveToken(ctx context.Context, selector string) (string, error) {
	record, err := s.findToken(ctx, selector)
	if err != nil {
		return "", err
	}
	result := s.db.WithContext(ctx).Delete(&tokenRecord{}, record.ID)
	if result.Error != nil {
		return "", fmt.Errorf("删除令牌失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return "", ErrTokenNotFound
	}
	return record.Name, nil
}

// SetTokenRemark 设置令牌备注；空白备注会清除已有备注。
func (s *Store) SetTokenRemark(ctx context.Context, selector, remark string) (model.Token, error) {
	record, err := s.findToken(ctx, selector)
	if err != nil {
		return model.Token{}, err
	}
	remark = strings.TrimSpace(remark)
	result := s.db.WithContext(ctx).Model(&tokenRecord{}).Where("id = ?", record.ID).Update("remark", remark)
	if result.Error != nil {
		return model.Token{}, fmt.Errorf("保存令牌备注失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return model.Token{}, ErrTokenNotFound
	}
	record.Remark = remark
	return tokenFromRecord(*record), nil
}

func (s *Store) findToken(ctx context.Context, selector string) (*tokenRecord, error) {
	selector = strings.TrimSpace(selector)
	db := s.db.WithContext(ctx)
	if id, err := strconv.ParseInt(selector, 10, 64); err == nil && id > 0 {
		var record tokenRecord
		if err := db.Where("id = ?", id).Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrTokenNotFound
			}
			return nil, fmt.Errorf("查找令牌失败: %w", err)
		}
		return &record, nil
	}

	var records []tokenRecord
	if err := db.Where("name = ?", selector).Order("id").Limit(2).Find(&records).Error; err != nil {
		return nil, fmt.Errorf("查找令牌失败: %w", err)
	}
	if len(records) == 0 {
		return nil, ErrTokenNotFound
	}
	if len(records) > 1 {
		return nil, ErrAmbiguousToken
	}
	return &records[0], nil
}

// RecordSuccess 在同一事务中读取上一次成功快照并保存本轮结果。
func (s *Store) RecordSuccess(ctx context.Context, tokenID int64, checkedAt time.Time, balance model.Balance) (*model.Snapshot, error) {
	var previous *model.Snapshot
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		previous, err = latestSuccess(tx, tokenID)
		if err != nil {
			return err
		}
		record := balanceRecord{
			TokenID:         tokenID,
			CheckedAt:       checkedAt.UTC().UnixMilli(),
			AvailableMicros: &balance.AvailableMicros,
			VoucherMicros:   &balance.VoucherMicros,
			CashMicros:      &balance.CashMicros,
			Error:           "",
		}
		if err := tx.Create(&record).Error; err != nil {
			return fmt.Errorf("保存余额快照失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return previous, nil
}

func latestSuccess(tx *gorm.DB, tokenID int64) (*model.Snapshot, error) {
	var record balanceRecord
	err := tx.Where("token_id = ? AND error = ''", tokenID).Order("id DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取上一次成功快照失败: %w", err)
	}
	if record.AvailableMicros == nil || record.VoucherMicros == nil || record.CashMicros == nil {
		return nil, fmt.Errorf("上一次成功快照缺少余额字段")
	}
	return &model.Snapshot{
		ID:        record.ID,
		TokenID:   record.TokenID,
		CheckedAt: time.UnixMilli(record.CheckedAt).UTC(),
		Balance: model.Balance{
			AvailableMicros: *record.AvailableMicros,
			VoucherMicros:   *record.VoucherMicros,
			CashMicros:      *record.CashMicros,
		},
	}, nil
}

// RecordFailure 保存查询异常；空余额字段确保异常不会被当成 0 余额。
func (s *Store) RecordFailure(ctx context.Context, tokenID int64, checkedAt time.Time, message string) error {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > 1000 {
		runes := []rune(message)
		message = string(runes[:1000])
	}
	record := balanceRecord{
		TokenID:   tokenID,
		CheckedAt: checkedAt.UTC().UnixMilli(),
		Error:     message,
	}
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return fmt.Errorf("保存查询异常失败: %w", err)
	}
	return nil
}

// SumConsumption 汇总闭区间 [start, end] 内相邻成功快照的余额下降量。
// 当天首条快照会与午夜前的最近一次成功快照比较，失败和补充记录不计为消耗。
func (s *Store) SumConsumption(ctx context.Context, start, end time.Time) (int64, error) {
	const query = `
SELECT COALESCE(SUM(previous_record.available_micros - current_record.available_micros), 0)::bigint
FROM balance_records AS current_record
JOIN LATERAL (
	SELECT previous_record.available_micros
	FROM balance_records AS previous_record
	WHERE previous_record.token_id = current_record.token_id
		AND previous_record.error = ''
		AND previous_record.id < current_record.id
	ORDER BY previous_record.id DESC
	LIMIT 1
) AS previous_record ON TRUE
WHERE current_record.error = ''
	AND current_record.checked_at >= ?
	AND current_record.checked_at <= ?
	AND current_record.available_micros < previous_record.available_micros`
	var total int64
	if err := s.db.WithContext(ctx).Raw(query, start.UTC().UnixMilli(), end.UTC().UnixMilli()).Scan(&total).Error; err != nil {
		return 0, fmt.Errorf("汇总消耗失败: %w", err)
	}
	return total, nil
}

func tokenFromRecord(record tokenRecord) model.Token {
	return model.Token{
		ID:         record.ID,
		Name:       record.Name,
		Remark:     record.Remark,
		Ciphertext: record.TokenCiphertext,
		CreatedAt:  time.UnixMilli(record.CreatedAt).UTC(),
	}
}
