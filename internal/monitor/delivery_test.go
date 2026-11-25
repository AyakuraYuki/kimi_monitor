package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"kimi_balance_monitor/internal/secrets"
)

type deliveryStoreStub struct {
	failArchivePart int
	override        []byte
	delivered       []string
}

func (s *deliveryStoreStub) WebhookOverride(context.Context) ([]byte, error) { return s.override, nil }
func (s *deliveryStoreStub) RecordNotificationPart(_ context.Context, _ string, part, total int, text string, _ time.Time) error {
	if part == s.failArchivePart {
		return fmt.Errorf("preview temporarily unavailable")
	}
	s.delivered = append(s.delivered, fmt.Sprintf("%d/%d %s", part, total, text))
	return nil
}

type webhookTransport func(*http.Request) (*http.Response, error)

func (f webhookTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliveryResolvesLiveSettingsAndKeepsOnlyAcknowledgedParts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &deliveryStoreStub{}
	var targets []string
	transport := webhookTransport(func(r *http.Request) (*http.Response, error) {
		targets = append(targets, r.URL.Query().Get("key"))
		reply := `{"errcode":0}`
		if len(targets) == 2 {
			reply = `{"errcode":1}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
	})
	delivery := Delivery{Store: store, Cipher: cipher, HTTPClient: &http.Client{Transport: transport}}
	if client, err := delivery.client(ctx); err != nil || client != nil {
		t.Fatalf("无目标时应跳过发送: %v, %v", client, err)
	}
	const url = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key="
	delivery.FallbackURL = url + "default"
	store.override, err = cipher.EncryptWebhook(url + "custom")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Decrypt(store.override); err == nil {
		t.Fatal("Webhook 密文不能作为令牌解密")
	}
	client, err := delivery.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.override = nil // 本轮沿用已选目标，下一轮应使用文件兜底。
	if err := delivery.send(ctx, client, []string{"第一条", "第二条"}); err == nil {
		t.Fatal("第二片推送失败，应报告错误")
	}
	client, err = delivery.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.send(ctx, client, []string{"下一轮"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(targets, []string{"custom", "custom", "default"}) || !slices.Equal(store.delivered, []string{"1/2 第一条", "1/1 下一轮"}) {
		t.Fatalf("targets=%v, recorded=%v", targets, store.delivered)
	}
	store.override, err = cipher.Encrypt("some-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delivery.client(ctx); err == nil {
		t.Fatal("令牌密文不能用作 Webhook，也不能静默回退到其他接收方")
	}
}

func TestArchiveFailureDoesNotInterruptDelivery(t *testing.T) {
	store := &deliveryStoreStub{failArchivePart: 1}
	sent := 0
	delivery := Delivery{Store: store, FallbackURL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test", HTTPClient: &http.Client{Transport: webhookTransport(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"errcode":0}`))}, nil
	})}}
	client, err := delivery.client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = delivery.send(t.Context(), client, []string{"first", "second"})
	if err == nil || sent != 2 || !slices.Equal(store.delivered, []string{"2/2 second"}) {
		t.Fatalf("archive failure: sent=%d, archived=%v, error=%v", sent, store.delivered, err)
	}
}
