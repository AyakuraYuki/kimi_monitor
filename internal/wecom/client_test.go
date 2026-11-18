package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendMarkdown(t *testing.T) {
	t.Parallel()
	const content = "## Kimi 余额监控\n\n> 可用余额：**¥49.588940**"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("请求方法 = %s", request.Method)
		}
		var payload struct {
			Msgtype  string `json:"msgtype"`
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Msgtype != "markdown" || payload.Markdown.Content != content {
			t.Errorf("webhook 请求体错误: %+v", payload)
		}
		_, _ = fmt.Fprint(writer, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"?key=super-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendMarkdown(context.Background(), content); err != nil {
		t.Fatal(err)
	}
	if err := client.SendMarkdown(context.Background(), strings.Repeat("a", MaxMarkdownBytes+1)); err == nil {
		t.Fatal("超长消息应在发送前被拒绝")
	}
}

func TestSendMarkdownChecksBusinessError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = fmt.Fprint(writer, `{"errcode":93000,"errmsg":"invalid webhook"}`)
	}))
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMarkdown(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "93000") {
		t.Fatalf("企业微信业务错误未被识别: %v", err)
	}
	server.Close()
	// 已关闭服务会触发网络错误，错误文本不得包含 webhook 密钥。
	client, _ = NewClient(server.URL+"?key=super-secret", server.Client())
	if err := client.SendMarkdown(context.Background(), "test"); err != nil && strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("网络错误泄露了 webhook 密钥: %v", err)
	}
}
