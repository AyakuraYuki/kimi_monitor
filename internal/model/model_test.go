package model

import (
	"errors"
	"testing"
	"time"
)

func TestParseMicrosAndFormatCNY(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input  string
		micros int64
		text   string
	}{
		{input: "49.58894", micros: 49_588_940, text: "¥49.588940"},
		{input: "1", micros: 1_000_000, text: "¥1.000000"},
		{input: "0.000001", micros: 1, text: "¥0.000001"},
		{input: "1e-6", micros: 1, text: "¥0.000001"},
		{input: "-3.00001", micros: -3_000_010, text: "-¥3.000010"},
	}
	for _, test := range tests {
		micros, err := ParseMicros(test.input)
		if err != nil {
			t.Fatalf("ParseMicros(%q) 返回错误: %v", test.input, err)
		}
		if micros != test.micros {
			t.Errorf("ParseMicros(%q) = %d，期望 %d", test.input, micros, test.micros)
		}
		if got := FormatCNY(micros); got != test.text {
			t.Errorf("FormatCNY(%d) = %q，期望 %q", micros, got, test.text)
		}
	}
	if _, err := ParseMicros("0.0000001"); err == nil {
		t.Fatal("超过 6 位小数的金额应当返回错误")
	}
}

func TestHealthAndChangeAreIndependent(t *testing.T) {
	t.Parallel()
	previous := &Snapshot{Balance: Balance{AvailableMicros: 20_000_000}}
	current := &Snapshot{Balance: Balance{AvailableMicros: 8_000_000}}
	result := TokenResult{Current: current, Previous: previous}
	if got := result.Health(10_000_000); got != HealthLow {
		t.Fatalf("低余额状态 = %s，期望 %s", got, HealthLow)
	}
	if got := result.Change(); got != ChangeConsumption {
		t.Fatalf("余额变动 = %s，期望 %s", got, ChangeConsumption)
	}

	result.Previous = &Snapshot{Balance: Balance{AvailableMicros: 3_000_000}}
	if got := result.Change(); got != ChangeReplenished {
		t.Fatalf("补充后仍低于预警线时变动 = %s，期望 %s", got, ChangeReplenished)
	}
	if got := result.Health(10_000_000); got != HealthLow {
		t.Fatalf("补充后仍应预警，实际状态 = %s", got)
	}

	result.Previous = &Snapshot{Balance: Balance{AvailableMicros: -3_000_000}}
	result.Current = &Snapshot{Balance: Balance{AvailableMicros: 0}}
	if got := result.Health(10_000_000); got != HealthExhausted {
		t.Fatalf("零余额状态 = %s，期望 %s", got, HealthExhausted)
	}
	if got := result.Change(); got != ChangeReplenished {
		t.Fatalf("从零以下补充到零仍应识别补充，实际 = %s", got)
	}

	result.Err = errors.New("查询失败")
	if got := result.Health(10_000_000); got != HealthError {
		t.Fatalf("查询异常应优先于余额状态，实际 = %s", got)
	}
	if got := result.Change(); got != ChangeUnavailable {
		t.Fatalf("查询异常不应产生环比，实际 = %s", got)
	}
}

func TestFirstAndUnchanged(t *testing.T) {
	t.Parallel()
	now := time.Now()
	current := &Snapshot{CheckedAt: now, Balance: Balance{AvailableMicros: 100}}
	result := TokenResult{Current: current}
	if result.Change() != ChangeFirst {
		t.Fatal("没有历史记录时应标记首次记录")
	}
	result.Previous = &Snapshot{Balance: current.Balance}
	if result.Change() != ChangeUnchanged {
		t.Fatal("相同余额应标记持平")
	}
}
