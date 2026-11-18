package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/wecom"
)

type apiStep struct {
	balance model.Balance
	err     error
}

type sequenceAPI struct {
	mutex sync.Mutex
	steps []apiStep
	calls int
}

func (a *sequenceAPI) FetchBalance(ctx context.Context, token string) (model.Balance, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if a.calls >= len(a.steps) {
		return model.Balance{}, fmt.Errorf("没有配置第 %d 次响应", a.calls+1)
	}
	step := a.steps[a.calls]
	a.calls++
	return step.balance, step.err
}

func newIntegrationService(t *testing.T, api BalanceClient) (*Service, *store.Store) {
	t.Helper()
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}
	cipher, err := secrets.NewCipher(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), t.TempDir()+"/monitor.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	plaintext := "sk-abcdefghijklmnop12345678"
	name, _ := secrets.MaskToken(plaintext)
	ciphertext, _ := cipher.Encrypt(plaintext)
	if _, err := database.AddToken(context.Background(), name, cipher.Digest(plaintext), ciphertext, time.Now()); err != nil {
		t.Fatal(err)
	}
	return &Service{Store: database, API: api, Cipher: cipher, MaxParallel: 1}, database
}

func TestStrictBalanceTransitions(t *testing.T) {
	t.Parallel()
	values := []int64{50_000_000, 8_000_000, 20_000_000, 0, -2_000_000, -1_000_000, 15_000_000}
	steps := make([]apiStep, len(values))
	for i, value := range values {
		steps[i].balance = model.Balance{AvailableMicros: value}
	}
	api := &sequenceAPI{steps: steps}
	service, _ := newIntegrationService(t, api)
	currentTime := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return currentTime }
	expected := []struct {
		health model.Health
		change model.Change
	}{
		{model.HealthNormal, model.ChangeFirst},
		{model.HealthLow, model.ChangeConsumption},
		{model.HealthNormal, model.ChangeReplenished},
		{model.HealthExhausted, model.ChangeConsumption},
		{model.HealthExhausted, model.ChangeConsumption},
		{model.HealthExhausted, model.ChangeReplenished},
		{model.HealthNormal, model.ChangeReplenished},
	}
	for i, want := range expected {
		results, err := service.CheckAll(context.Background())
		if err != nil {
			t.Fatalf("第 %d 轮失败: %v", i+1, err)
		}
		result := results[0]
		if result.Err != nil {
			t.Fatalf("第 %d 轮查询异常: %v", i+1, result.Err)
		}
		if got := result.Health(10_000_000); got != want.health {
			t.Errorf("第 %d 轮状态 = %s，期望 %s", i+1, got, want.health)
		}
		if got := result.Change(); got != want.change {
			t.Errorf("第 %d 轮变动 = %s，期望 %s", i+1, got, want.change)
		}
		messages := BuildMessages(results, currentTime, 10_000_000)
		if i == 5 && !strings.Contains(messages[0], "已补充，但仍不可用") {
			t.Errorf("负余额增加时必须同时保留用尽状态: %s", messages[0])
		}
		if i == 6 && !strings.Contains(messages[0], "已恢复可用") {
			t.Errorf("从非正余额恢复后应明确标识: %s", messages[0])
		}
		currentTime = currentTime.Add(10 * time.Minute)
	}
}

func TestFailureUsesLastSuccessfulSnapshot(t *testing.T) {
	t.Parallel()
	api := &sequenceAPI{steps: []apiStep{
		{balance: model.Balance{AvailableMicros: 10_000_000}},
		{err: errors.New("HTTP 500")},
		{balance: model.Balance{AvailableMicros: 8_000_000}},
	}}
	service, _ := newIntegrationService(t, api)
	first, err := service.CheckAll(context.Background())
	if err != nil || first[0].Err != nil {
		t.Fatalf("首次查询失败: %v, %v", err, first[0].Err)
	}
	failed, err := service.CheckAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if failed[0].Err == nil || failed[0].Current != nil || failed[0].Change() != model.ChangeUnavailable {
		t.Fatalf("失败轮次不应生成快照或环比: %+v", failed[0])
	}
	third, err := service.CheckAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Previous == nil || third[0].Previous.Balance.AvailableMicros != 10_000_000 {
		t.Fatalf("失败后应与最近一次成功快照比较: %+v", third[0].Previous)
	}
	if third[0].Change() != model.ChangeConsumption {
		t.Fatalf("失败后的成功查询应识别消耗，实际 = %s", third[0].Change())
	}
}

func TestReportSplitsWithoutBreakingTokenBlocks(t *testing.T) {
	t.Parallel()
	results := make([]model.TokenResult, 80)
	for i := range results {
		results[i] = model.TokenResult{
			TokenID:  int64(i + 1),
			Name:     "sk-abcd****0123",
			Current:  &model.Snapshot{Balance: model.Balance{AvailableMicros: 49_588_940, VoucherMicros: 46_588_930, CashMicros: 3_000_010}},
			Previous: &model.Snapshot{CheckedAt: time.Now(), Balance: model.Balance{AvailableMicros: 50_000_000}},
		}
	}
	messages := BuildMessages(results, time.Now(), 10_000_000)
	if len(messages) < 2 {
		t.Fatal("大量令牌应自动拆分企业微信消息")
	}
	for _, message := range messages {
		if len(message) > wecom.MaxMarkdownBytes {
			t.Fatalf("消息长度 %d 超过企业微信限制", len(message))
		}
		if !utf8.ValidString(message) {
			t.Fatal("消息包含被截断的 UTF-8 字符")
		}
	}
	for i := range results {
		heading := fmt.Sprintf("### #%d ", i+1)
		count := 0
		for _, message := range messages {
			count += strings.Count(message, heading)
		}
		if count != 1 {
			t.Fatalf("令牌 #%d 的完整块出现 %d 次，期望 1 次", i+1, count)
		}
	}
}

func TestReportEscapesAPIError(t *testing.T) {
	t.Parallel()
	results := []model.TokenResult{{
		TokenID: 1,
		Name:    "sk-abcd****0123",
		Err:     errors.New("<font color=\"warning\">`bad`\nnext"),
	}}
	message := BuildMessages(results, time.Now(), 10_000_000)[0]
	if strings.Contains(message, "<font color=\"warning\">`bad`") || strings.Contains(message, "`bad`") {
		t.Fatalf("API 错误未转义 Markdown: %s", message)
	}
	if !strings.Contains(message, "&lt;font") {
		t.Fatalf("API 错误中的 HTML 未转义: %s", message)
	}
}
