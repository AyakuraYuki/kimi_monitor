package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/wecom"
)

// Delivery 在每轮推送前选取目标，整轮的所有分片都发送到同一个机器人。
type Delivery struct {
	Store interface {
		WebhookOverride(ctx context.Context) ([]byte, error)
		RecordNotificationPart(ctx context.Context, batch string, part, total int, text string, sentAt time.Time) error
	}
	Cipher      *secrets.Cipher
	FallbackURL string
	HTTPClient  *http.Client
}

func (d *Delivery) client(ctx context.Context) (*wecom.Client, error) {
	ciphertext, err := d.Store.WebhookOverride(ctx)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(d.FallbackURL)
	if len(ciphertext) > 0 {
		url, err = d.Cipher.DecryptWebhook(ciphertext)
		if err != nil {
			return nil, fmt.Errorf("解密企业微信配置失败: %w", err)
		}
	}
	if url == "" {
		return nil, nil
	}
	return wecom.NewClient(url, d.HTTPClient)
}

func (d *Delivery) send(ctx context.Context, client *wecom.Client, messages []string) error {
	batch := uuid.New().String()
	var archiveErr error
	for index, text := range messages {
		if err := client.SendMarkdown(ctx, text); err != nil {
			return errors.Join(archiveErr, fmt.Errorf("推送第 %d/%d 条消息失败: %w", index+1, len(messages), err))
		}
		// 机器人已经确认接收，即使此时进程开始退出，也要给确认记录一次限时提交机会。
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := d.Store.RecordNotificationPart(recordCtx, batch, index+1, len(messages), text, time.Now().UTC())
		cancel()
		if err != nil && archiveErr == nil {
			// 预览暂时无法写入时，仍继续向机器人发送本轮剩余的分片。
			archiveErr = fmt.Errorf("保存第 %d/%d 条已推送消息失败: %w", index+1, len(messages), err)
		}
	}
	return archiveErr
}
