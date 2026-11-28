package store

import (
	"bytes"
	"testing"
	"time"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
)

func TestConsoleIncludesMissingSamplesAndLastSuccessfulBalance(t *testing.T) {
	t.Parallel()
	db, ctx := openTestStore(t), t.Context()
	day := time.Date(2025, 1, 3, 0, 0, 0, 0, config.CST())
	first, err := db.AddTokenWithRemark(ctx, "sk-demo****1234", "  主账号 ", []byte{1}, []byte{1}, day.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.AddToken(ctx, first.Name, []byte{2}, []byte{2}, day.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sample := func(at time.Time, balance int64) {
		t.Helper()
		if _, err := db.RecordSuccess(ctx, first.ID, at, model.Balance{AvailableMicros: balance, CashMicros: balance}); err != nil {
			t.Fatal(err)
		}
	}
	sample(day.Add(-time.Minute), 100)
	sample(day, 90)
	sample(day.Add(time.Hour), 120)
	sample(day.Add(2*time.Hour), 115)
	firstSuccessAt := day.AddDate(0, 0, 1)
	sample(firstSuccessAt, 115)
	failureAt := firstSuccessAt.Add(time.Minute)
	if err := db.RecordFailure(ctx, first.ID, failureAt, "HTTP 503"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DailyConsumption(ctx, day, day.AddDate(0, 0, 3))
	if err != nil || len(rows) != 6 {
		t.Fatalf("daily rows = %+v, err = %v", rows, err)
	}
	if rows[0].Date != "2025-01-05" || rows[0].ConsumptionMicros != nil || rows[1].ConsumptionMicros != nil {
		t.Fatalf("无采样日不应展示为已知零消耗: %+v", rows[:2])
	}
	if rows[2].Date != "2025-01-04" || rows[2].ConsumptionMicros == nil || *rows[2].ConsumptionMicros != 0 {
		t.Fatalf("已采样而余额未变的日期应为零: %+v", rows[2])
	}
	if rows[4].TokenID != first.ID || rows[4].Remark != "主账号" || rows[4].ConsumptionMicros == nil || *rows[4].ConsumptionMicros != 15 {
		t.Fatalf("跨午夜与补充后的消耗统计错误: %+v", rows[4])
	}
	if rows[5].TokenID != second.ID || rows[5].ConsumptionMicros != nil {
		t.Fatalf("同名待采集令牌不应合并: %+v", rows[5])
	}
	before, err := db.DailyConsumption(ctx, day.AddDate(0, 0, -2), day)
	if err != nil || len(before) != 1 || before[0].TokenID != first.ID {
		t.Fatalf("不应列出注册日之前的令牌: %+v, %v", before, err)
	}
	latest, err := db.LatestTokens(ctx)
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest = %+v, err = %v", latest, err)
	}
	got := latest[0]
	if got.CheckedAt == nil || !got.CheckedAt.Equal(failureAt) || got.Error != "HTTP 503" || got.BalanceAt == nil || !got.BalanceAt.Equal(firstSuccessAt) || got.AvailableMicros == nil || *got.AvailableMicros != 115 {
		t.Fatalf("失败尝试应保留上次成功余额及对应时间: %+v", got)
	}
	if latest[1].CheckedAt != nil || latest[1].AvailableMicros != nil {
		t.Fatalf("新令牌应待采集: %+v", latest[1])
	}
	if !latest[0].MonitoringEnabled || !latest[1].MonitoringEnabled {
		t.Fatalf("概览应包含默认开启的查询状态: %+v", latest)
	}
}

func TestNotificationPreviewAndWebhookSetting(t *testing.T) {
	t.Parallel()
	db, ctx := openTestStore(t), t.Context()
	preview, err := db.LatestNotification(ctx)
	if err != nil || preview != nil {
		t.Fatalf("初始预览 = %+v, %v", preview, err)
	}
	for _, value := range [][]byte{{1, 2}, {3, 4}, nil} {
		if err := db.SetWebhookOverride(ctx, value); err != nil {
			t.Fatal(err)
		}
		got, err := db.WebhookOverride(ctx)
		if err != nil || !bytes.Equal(got, value) {
			t.Fatalf("WebHook 覆盖配置 = %v, %v", got, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, input := range []struct {
		batch       string
		part, total int
		text        string
	}{
		{"one", 1, 1, "旧报告"},
		{"two", 1, 3, "# 第一条"},
		{"two", 2, 3, "第二条"},
	} {
		if err := db.RecordNotificationPart(ctx, input.batch, input.part, input.total, input.text, now); err != nil {
			t.Fatal(err)
		}
	}
	preview, err = db.LatestNotification(ctx)
	if err != nil || preview == nil || preview.Text != "# 第一条\n\n第二条" || preview.Total != 3 || preview.Delivered != 2 || !preview.SentAt.Equal(now) {
		t.Fatalf("部分送达后的预览 = %+v, %v", preview, err)
	}
}
