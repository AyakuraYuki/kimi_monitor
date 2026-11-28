package monitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/testdb"
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
	database, err := store.Open(context.Background(), testdb.DSN(t))
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

type consumptionStoreStub struct {
	Store
	summaries []map[int64]int64
	calls     [][2]time.Time
}

func (s *consumptionStoreStub) SumConsumptionByToken(_ context.Context, start, end time.Time) (map[int64]int64, error) {
	s.calls = append(s.calls, [2]time.Time{start, end})
	if len(s.summaries) == 0 {
		return nil, errors.New("意外的消耗统计查询")
	}
	summary := s.summaries[0]
	s.summaries = s.summaries[1:]
	return summary, nil
}

type checkStoreStub struct {
	Store
	tokens   []model.Token
	failures int
}

func (s *checkStoreStub) ListTokens(context.Context) ([]model.Token, error) {
	return slices.Clone(s.tokens), nil
}

func (s *checkStoreStub) RecordSuccess(context.Context, int64, time.Time, model.Balance) (*model.Snapshot, error) {
	return nil, nil
}

func (s *checkStoreStub) RecordFailure(context.Context, int64, time.Time, string) error {
	s.failures++
	return nil
}

func TestCheckAllHonorsMonitoringEnabled(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	enabledCiphertext, err := cipher.Encrypt("enabled-token")
	if err != nil {
		t.Fatal(err)
	}
	pausedCiphertext, err := cipher.Encrypt("paused-token")
	if err != nil {
		t.Fatal(err)
	}
	database := &checkStoreStub{tokens: []model.Token{
		{ID: 1, Name: "enabled", Ciphertext: enabledCiphertext, MonitoringEnabled: true},
		{ID: 2, Name: "paused", Ciphertext: pausedCiphertext, MonitoringEnabled: false},
	}}
	api := &recordingAPI{calls: make(map[string]int)}
	service := &Service{Store: database, API: api, Cipher: cipher, MaxParallel: 1}

	results, err := service.CheckAll(t.Context())
	if err != nil || len(results) != 1 || results[0].TokenID != 1 || results[0].Err != nil {
		t.Fatalf("首轮结果错误: %+v, %v", results, err)
	}
	if api.callCount("enabled-token") != 1 || api.callCount("paused-token") != 0 || database.failures != 0 {
		t.Fatalf("暂停令牌不应调用 API 或记录失败: calls=%+v, failures=%d", api.calls, database.failures)
	}

	database.tokens[1].MonitoringEnabled = true
	results, err = service.CheckAll(t.Context())
	if err != nil || len(results) != 2 || results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("恢复后的结果错误: %+v, %v", results, err)
	}
	if api.callCount("enabled-token") != 2 || api.callCount("paused-token") != 1 || database.failures != 0 {
		t.Fatalf("恢复后应重新调用 API: calls=%+v, failures=%d", api.calls, database.failures)
	}
}

func TestBuildReportUsesBeijingConsumptionWindows(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		checkedAt     time.Time
		wantYesterday bool
	}{
		{name: "零点不再包含昨日", checkedAt: time.Date(2026, 11, 24, 0, 30, 45, 0, config.CST())},
		{name: "八点前不包含昨日", checkedAt: time.Date(2026, 11, 24, 7, 59, 59, 0, config.CST())},
		{name: "八点整包含昨日", checkedAt: time.Date(2026, 11, 24, 8, 0, 0, 0, config.CST()), wantYesterday: true},
		{name: "八点时段末尾包含昨日", checkedAt: time.Date(2026, 11, 24, 8, 59, 59, 0, config.CST()), wantYesterday: true},
		{name: "九点整不包含昨日", checkedAt: time.Date(2026, 11, 24, 9, 0, 0, 0, config.CST())},
		{name: "十一点整包含昨日", checkedAt: time.Date(2026, 11, 24, 11, 0, 0, 0, config.CST()), wantYesterday: true},
		{name: "十一点时段末尾包含昨日", checkedAt: time.Date(2026, 11, 24, 11, 59, 59, 0, config.CST()), wantYesterday: true},
		{name: "十二点整不包含昨日", checkedAt: time.Date(2026, 11, 24, 12, 0, 0, 0, config.CST())},
		{name: "十五点整包含昨日", checkedAt: time.Date(2026, 11, 24, 15, 0, 0, 0, config.CST()), wantYesterday: true},
		{name: "十五点时段末尾包含昨日", checkedAt: time.Date(2026, 11, 24, 15, 59, 59, 0, config.CST()), wantYesterday: true},
		{name: "十六点整不包含昨日", checkedAt: time.Date(2026, 11, 24, 16, 0, 0, 0, config.CST())},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := &consumptionStoreStub{summaries: []map[int64]int64{
				{1: 25_000_000},
				{1: 10_000_000, 2: 5_000_000},
			}}
			service := &Service{Store: database}
			results := []model.TokenResult{
				{
					TokenID: 1,
					Name:    "sk-abcd****0123",
					Current: &model.Snapshot{Balance: model.Balance{AvailableMicros: 50_000_000}},
				},
				{
					TokenID: 2,
					Name:    "sk-wxyz****9012",
					Current: &model.Snapshot{Balance: model.Balance{AvailableMicros: 30_000_000}},
				},
			}

			messages, err := service.BuildReport(t.Context(), results, testCase.checkedAt, 10_000_000)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 {
				t.Fatalf("消息数 = %d，期望 1", len(messages))
			}
			firstTokenReport, remainingReport, found := strings.Cut(messages[0], "### #2")
			if !found {
				t.Fatalf("报告缺少第二个令牌区块：%s", messages[0])
			}
			if !strings.Contains(firstTokenReport, "> 当日总消耗：**¥25.000000**") {
				t.Fatalf("有当日消耗的令牌未展示金额：%s", firstTokenReport)
			}
			if strings.Contains(remainingReport, "当日总消耗") {
				t.Fatalf("没有当日消耗的令牌不应展示该统计行：%s", remainingReport)
			}
			if testCase.wantYesterday {
				if !strings.Contains(firstTokenReport, "> 昨日总消耗：**¥10.000000**") {
					t.Fatalf("第一个令牌未展示自身昨日消耗：%s", firstTokenReport)
				}
				if !strings.Contains(remainingReport, "> 昨日总消耗：**¥5.000000**") {
					t.Fatalf("第二个令牌未展示自身昨日消耗：%s", remainingReport)
				}
			}
			expectedFooter := fmt.Sprintf(
				"### 消耗统计（北京时间）\n> 今日累计消耗（00:00 至 %s）：**¥25.000000**",
				testCase.checkedAt.Format("15:04:05"),
			)
			if testCase.wantYesterday {
				expectedFooter += "\n> 昨日总消耗（2026-11-23）：**¥15.000000**"
			}
			if !strings.HasSuffix(messages[0], expectedFooter) {
				t.Fatalf("消耗统计未作为独立区块放在通知底部：\n%s\n期望后缀：\n%s", messages[0], expectedFooter)
			}
			if !testCase.wantYesterday && strings.Contains(messages[0], "昨日总消耗") {
				t.Fatalf("非昨日统计时段不应显示昨日总消耗：%s", messages[0])
			}

			todayStart := time.Date(2026, 11, 24, 0, 0, 0, 0, config.CST())
			expectedCalls := 1
			if testCase.wantYesterday {
				expectedCalls = 2
			}
			if len(database.calls) != expectedCalls {
				t.Fatalf("统计查询次数 = %d，期望 %d", len(database.calls), expectedCalls)
			}
			if !database.calls[0][0].Equal(todayStart) || !database.calls[0][1].Equal(testCase.checkedAt) {
				t.Fatalf("今日统计区间错误：%+v", database.calls[0])
			}
			if testCase.wantYesterday {
				yesterdayStart := todayStart.AddDate(0, 0, -1)
				if !database.calls[1][0].Equal(yesterdayStart) || !database.calls[1][1].Equal(todayStart.Add(-time.Millisecond)) {
					t.Fatalf("昨日统计区间错误：%+v", database.calls[1])
				}
			}
		})
	}
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
		messages := BuildMessages(results, currentTime, 10_000_000, model.ConsumptionSummary{})
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
	messages := BuildMessages(results, time.Now(), 10_000_000, model.ConsumptionSummary{})
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
	if !strings.Contains(messages[len(messages)-1], "### 消耗统计（北京时间）") {
		t.Fatal("拆分后的最后一条消息必须包含独立消耗统计")
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

func TestReportUsesCompactBlockForUnchangedBalance(t *testing.T) {
	t.Parallel()
	result := model.TokenResult{
		TokenID: 2,
		Name:    "sk-abcd****0123",
		Remark:  "生产环境 <主账号>",
		Current: &model.Snapshot{Balance: model.Balance{
			AvailableMicros: 50_000_000,
			VoucherMicros:   46_000_000,
			CashMicros:      4_000_000,
		}},
		Previous: &model.Snapshot{Balance: model.Balance{AvailableMicros: 50_000_000}},
	}

	block := buildTokenBlock(result, 10_000_000)
	want := "### #2 `sk-abcd****0123`\n" +
		"> 备注：生产环境 &lt;主账号&gt;\n" +
		"<font color=\"info\">🟢 正常</font>　可用余额：**¥50.000000**\n\n"
	if block != want {
		t.Fatalf("无变化令牌应在可用余额前同行展示状态：\n实际：\n%s\n期望：\n%s", block, want)
	}

	result.Remark = "  "
	block = buildTokenBlock(result, 10_000_000)
	want = "### #2 `sk-abcd****0123`\n" +
		"<font color=\"info\">🟢 正常</font>　可用余额：**¥50.000000**\n\n"
	if block != want {
		t.Fatalf("无变化且无备注的令牌应省略备注：\n实际：\n%s\n期望：\n%s", block, want)
	}

	result.TodayConsumptionMicros = 2_000_000
	result.YesterdayConsumptionMicros = 3_000_000
	block = buildTokenBlock(result, 10_000_000)
	want = "### #2 `sk-abcd****0123`\n" +
		"<font color=\"info\">🟢 正常</font>　可用余额：**¥50.000000**\n" +
		"> 当日总消耗：**¥2.000000**\n" +
		"> 昨日总消耗：**¥3.000000**\n\n"
	if block != want {
		t.Fatalf("有消耗统计时应增加独立统计行：\n实际：\n%s\n期望：\n%s", block, want)
	}

	result.TodayConsumptionMicros = 0
	result.YesterdayConsumptionMicros = 0
	result.Previous.Balance.AvailableMicros = 49_000_000
	block = buildTokenBlock(result, 10_000_000)
	for _, expected := range []string{"🟢 正常", "📈 补充", "上次可用：", "现金：", "代金券："} {
		if !strings.Contains(block, expected) {
			t.Errorf("余额有变化时应保留完整内容，缺少 %q：%s", expected, block)
		}
	}
}

func TestReportDisplaysEscapedRemark(t *testing.T) {
	t.Parallel()
	result := model.TokenResult{
		TokenID: 1,
		Name:    "sk-abcd****0123",
		Remark:  "生产环境 <主账号> `A`\n第二行",
		Current: &model.Snapshot{Balance: model.Balance{AvailableMicros: 50_000_000}},
	}
	message := BuildMessages([]model.TokenResult{result}, time.Now(), 10_000_000, model.ConsumptionSummary{})[0]
	if !strings.Contains(message, "备注：生产环境 &lt;主账号&gt; 'A' 第二行") {
		t.Fatalf("报告未展示并转义令牌备注: %s", message)
	}
	if strings.Contains(message, "<主账号>") || strings.Contains(message, "`A`") {
		t.Fatalf("令牌备注中的 HTML 或 Markdown 未转义: %s", message)
	}

	result.Remark = ""
	message = BuildMessages([]model.TokenResult{result}, time.Now(), 10_000_000, model.ConsumptionSummary{})[0]
	if strings.Contains(message, "备注：") {
		t.Fatalf("空备注不应占用报告内容: %s", message)
	}
}

func TestReportEscapesAPIError(t *testing.T) {
	t.Parallel()
	results := []model.TokenResult{{
		TokenID:                1,
		Name:                   "sk-abcd****0123",
		TodayConsumptionMicros: 1_230_000,
		Err:                    errors.New("<font color=\"warning\">`bad`\nnext"),
	}}
	message := BuildMessages(results, time.Now(), 10_000_000, model.ConsumptionSummary{})[0]
	if strings.Contains(message, "<font color=\"warning\">`bad`") || strings.Contains(message, "`bad`") {
		t.Fatalf("API 错误未转义 Markdown: %s", message)
	}
	if !strings.Contains(message, "&lt;font") {
		t.Fatalf("API 错误中的 HTML 未转义: %s", message)
	}
	if !strings.Contains(message, "> 当日总消耗：**¥1.230000**") {
		t.Fatalf("本轮查询失败不应隐藏当天已经产生的消耗: %s", message)
	}
}
