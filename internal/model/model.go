package model

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// MicrosPerYuan 表示一元对应的微元数。
const MicrosPerYuan int64 = 1_000_000

// Balance 使用整数微元保存余额，避免浮点运算造成环比误差。
type Balance struct {
	AvailableMicros int64
	VoucherMicros   int64
	CashMicros      int64
}

// Token 是数据库中的令牌记录，Ciphertext 不包含明文。
type Token struct {
	ID         int64
	Name       string
	Remark     string
	Ciphertext []byte
	CreatedAt  time.Time
}

// Snapshot 是一次成功的余额查询结果。
type Snapshot struct {
	ID        int64
	TokenID   int64
	CheckedAt time.Time
	Balance   Balance
}

// TokenResult 包含本轮结果、最近一次成功快照和当日累计消耗。
type TokenResult struct {
	TokenID                int64
	Name                   string
	Remark                 string
	Current                *Snapshot
	Previous               *Snapshot
	TodayConsumptionMicros int64
	Err                    error
}

// ConsumptionSummary 是从持久化快照汇总的当日和昨日消耗，单位为微元。
type ConsumptionSummary struct {
	TodayMicros     int64
	YesterdayMicros int64
}

// Health 表示余额可用状态，与余额变动方向无关。
type Health string

const (
	HealthNormal    Health = "normal"
	HealthLow       Health = "low"
	HealthExhausted Health = "exhausted"
	HealthError     Health = "error"
)

// Health 按“异常 > 用尽 > 低余额 > 正常”的优先级计算状态。
func (r TokenResult) Health(lowThresholdMicros int64) Health {
	if r.Err != nil || r.Current == nil {
		return HealthError
	}
	available := r.Current.Balance.AvailableMicros
	if available <= 0 {
		return HealthExhausted
	}
	if available <= lowThresholdMicros {
		return HealthLow
	}
	return HealthNormal
}

// Change 表示与上一次成功查询相比的变动。
type Change string

const (
	ChangeFirst       Change = "first"
	ChangeConsumption Change = "consumption"
	ChangeReplenished Change = "replenished"
	ChangeUnchanged   Change = "unchanged"
	ChangeUnavailable Change = "unavailable"
)

// Change 只在当前和上一次查询都成功时给出环比结果。
func (r TokenResult) Change() Change {
	if r.Err != nil || r.Current == nil {
		return ChangeUnavailable
	}
	if r.Previous == nil {
		return ChangeFirst
	}
	delta := r.Current.Balance.AvailableMicros - r.Previous.Balance.AvailableMicros
	switch {
	case delta > 0:
		return ChangeReplenished
	case delta < 0:
		return ChangeConsumption
	default:
		return ChangeUnchanged
	}
}

// DeltaMicros 返回当前可用余额与上一次成功记录的差值。
func (r TokenResult) DeltaMicros() (int64, bool) {
	if r.Current == nil || r.Previous == nil || r.Err != nil {
		return 0, false
	}
	return r.Current.Balance.AvailableMicros - r.Previous.Balance.AvailableMicros, true
}

// ParseMicros 将十进制人民币金额精确转换为微元。
func ParseMicros(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("金额不能为空")
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0, fmt.Errorf("无效金额 %q", value)
	}
	scaled := new(big.Rat).Mul(r, big.NewRat(MicrosPerYuan, 1))
	if !scaled.IsInt() {
		return 0, fmt.Errorf("金额 %q 的小数精度超过 6 位", value)
	}
	if !scaled.Num().IsInt64() {
		return 0, fmt.Errorf("金额 %q 超出支持范围", value)
	}
	return scaled.Num().Int64(), nil
}

// FormatCNY 将微元格式化为带人民币符号的字符串。
func FormatCNY(micros int64) string {
	sign := ""
	var absolute uint64
	if micros < 0 {
		sign = "-"
		absolute = uint64(-(micros + 1)) + 1
	} else {
		absolute = uint64(micros)
	}
	return fmt.Sprintf("%s¥%d.%06d", sign, absolute/uint64(MicrosPerYuan), absolute%uint64(MicrosPerYuan))
}
