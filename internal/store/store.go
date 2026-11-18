package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kimi_balance_monitor/internal/model"

	_ "modernc.org/sqlite"
)

var (
	ErrDuplicateToken = errors.New("令牌已存在，不可重复添加")
	ErrTokenNotFound  = errors.New("令牌不存在")
	ErrAmbiguousToken = errors.New("打码名称对应多个令牌，请使用 ID 删除")
)

const schema = `
CREATE TABLE IF NOT EXISTS tokens (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT NOT NULL,
    token_hash      BLOB NOT NULL UNIQUE,
    token_ciphertext BLOB NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS balance_records (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    token_id          INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    checked_at        INTEGER NOT NULL,
    available_micros  INTEGER,
    voucher_micros    INTEGER,
    cash_micros       INTEGER,
    error             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_balance_records_latest_success
    ON balance_records(token_id, id DESC)
    WHERE ERROR = '';

PRAGMA user_version = 1;
`

// Store 持久化令牌密文和所有查询记录。
type Store struct {
	db *sql.DB
}

// Open 打开数据库并执行幂等建表。
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("数据库路径不能为空")
	}
	if path != ":memory:" {
		if directory := filepath.Dir(path); directory != "." {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return nil, fmt.Errorf("创建数据库目录失败: %w", err)
			}
		}
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("创建数据库失败: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("关闭数据库文件失败: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("设置数据库权限失败: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 单连接确保 PRAGMA 在每次查询中一致，并减少 SQLite 写锁竞争。
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 SQLite 失败: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("初始化 SQLite 参数失败: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化数据库结构失败: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// AddToken 添加令牌；token_hash 的唯一索引保证不会重复。
func (s *Store) AddToken(ctx context.Context, name string, digest, ciphertext []byte, now time.Time) (model.Token, error) {
	var existingID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tokens WHERE token_hash = ?`, digest).Scan(&existingID)
	if err == nil {
		return model.Token{}, ErrDuplicateToken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Token{}, fmt.Errorf("检查令牌是否重复失败: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(name, token_hash, token_ciphertext, created_at) VALUES (?, ?, ?, ?)`,
		name, digest, ciphertext, now.UTC().UnixMilli(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return model.Token{}, ErrDuplicateToken
		}
		return model.Token{}, fmt.Errorf("保存令牌失败: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Token{}, fmt.Errorf("读取令牌 ID 失败: %w", err)
	}
	return model.Token{ID: id, Name: name, Ciphertext: ciphertext, CreatedAt: now}, nil
}

// ListTokens 返回全部令牌，顺序固定便于生成报告。
func (s *Store) ListTokens(ctx context.Context) ([]model.Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_ciphertext, created_at FROM tokens ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("读取令牌列表失败: %w", err)
	}
	defer rows.Close()

	var tokens []model.Token
	for rows.Next() {
		var token model.Token
		var createdAt int64
		if err := rows.Scan(&token.ID, &token.Name, &token.Ciphertext, &createdAt); err != nil {
			return nil, fmt.Errorf("解析令牌记录失败: %w", err)
		}
		token.CreatedAt = time.UnixMilli(createdAt)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历令牌记录失败: %w", err)
	}
	return tokens, nil
}

// RemoveToken 支持使用数字 ID 或完整打码名称删除令牌。
func (s *Store) RemoveToken(ctx context.Context, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	var id int64
	var name string
	if parsed, err := strconv.ParseInt(selector, 10, 64); err == nil && parsed > 0 {
		id = parsed
		if err := s.db.QueryRowContext(ctx, `SELECT name FROM tokens WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrTokenNotFound
			}
			return "", fmt.Errorf("查找令牌失败: %w", err)
		}
	} else {
		rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM tokens WHERE name = ? LIMIT 2`, selector)
		if err != nil {
			return "", fmt.Errorf("查找令牌失败: %w", err)
		}
		var matches []model.Token
		for rows.Next() {
			var token model.Token
			if err := rows.Scan(&token.ID, &token.Name); err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("解析令牌记录失败: %w", err)
			}
			matches = append(matches, token)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("遍历令牌记录失败: %w", err)
		}
		_ = rows.Close()
		if len(matches) == 0 {
			return "", ErrTokenNotFound
		}
		if len(matches) > 1 {
			return "", ErrAmbiguousToken
		}
		id, name = matches[0].ID, matches[0].Name
	}

	result, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return "", fmt.Errorf("删除令牌失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("确认删除结果失败: %w", err)
	}
	if affected == 0 {
		return "", ErrTokenNotFound
	}
	return name, nil
}

// RecordSuccess 在同一事务中读取上一次成功快照并保存本轮结果。
func (s *Store) RecordSuccess(ctx context.Context, tokenID int64, checkedAt time.Time, balance model.Balance) (*model.Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启余额写入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	previous, err := latestSuccess(ctx, tx, tokenID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO balance_records(
			token_id, checked_at, available_micros, voucher_micros, cash_micros, error
		) VALUES (?, ?, ?, ?, ?, '')`,
		tokenID, checkedAt.UTC().UnixMilli(), balance.AvailableMicros, balance.VoucherMicros, balance.CashMicros,
	); err != nil {
		return nil, fmt.Errorf("保存余额快照失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交余额写入事务失败: %w", err)
	}
	return previous, nil
}

func latestSuccess(ctx context.Context, tx *sql.Tx, tokenID int64) (*model.Snapshot, error) {
	var snapshot model.Snapshot
	var checkedAt int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, token_id, checked_at, available_micros, voucher_micros, cash_micros
		FROM balance_records
		WHERE token_id = ? AND error = ''
		ORDER BY id DESC
		LIMIT 1`, tokenID,
	).Scan(
		&snapshot.ID, &snapshot.TokenID, &checkedAt,
		&snapshot.Balance.AvailableMicros, &snapshot.Balance.VoucherMicros, &snapshot.Balance.CashMicros,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取上一次成功快照失败: %w", err)
	}
	snapshot.CheckedAt = time.UnixMilli(checkedAt)
	return &snapshot, nil
}

// RecordFailure 保存查询异常；空余额字段确保异常不会被当成 0 余额。
func (s *Store) RecordFailure(ctx context.Context, tokenID int64, checkedAt time.Time, message string) error {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > 1000 {
		runes := []rune(message)
		message = string(runes[:1000])
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO balance_records(token_id, checked_at, error) VALUES (?, ?, ?)`,
		tokenID, checkedAt.UTC().UnixMilli(), message,
	); err != nil {
		return fmt.Errorf("保存查询异常失败: %w", err)
	}
	return nil
}
