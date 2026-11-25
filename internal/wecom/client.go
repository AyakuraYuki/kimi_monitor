package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxMarkdownBytes 是企业微信 Markdown 消息的内容上限。
	MaxMarkdownBytes = 4096
	maxResponseBytes = 1 << 20
)

// Client 向企业微信群机器人发送消息。
type Client struct {
	webhookURL string
	httpClient *http.Client
}

var robotKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// ValidateManagedURL 只接受企业微信群机器人的官方发送地址，避免界面配置任意出站请求。
func ValidateManagedURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err == nil && u.Scheme == "https" && u.Host == "qyapi.weixin.qq.com" &&
		u.User == nil && u.EscapedPath() == "/cgi-bin/webhook/send" && u.Fragment == "" {
		params, err := url.ParseQuery(u.RawQuery)
		if err == nil && len(params) == 1 && len(params["key"]) == 1 && robotKey.MatchString(params.Get("key")) {
			return nil
		}
	}
	return fmt.Errorf("请输入企业微信官方 HTTPS 群机器人 webhook 地址，并包含有效的 key")
}

// NewClient 创建企业微信 webhook 客户端。
func NewClient(webhookURL string, httpClient *http.Client) (*Client, error) {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return nil, fmt.Errorf("未设置 wecom.webhook_url")
	}
	parsed, err := url.ParseRequestURI(webhookURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("wecom.webhook_url 必须是有效的 HTTP(S) 地址")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{webhookURL: webhookURL, httpClient: &client}, nil
}

// SendMarkdown 发送一条 Markdown 消息，并检查企业微信业务响应码。
func (c *Client) SendMarkdown(ctx context.Context, content string) error {
	if content == "" {
		return fmt.Errorf("企业微信消息内容不能为空")
	}
	if len(content) > MaxMarkdownBytes {
		return fmt.Errorf("企业微信 Markdown 消息为 %d 字节，超过 %d 字节限制", len(content), MaxMarkdownBytes)
	}
	payload, err := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": content,
		},
	})
	if err != nil {
		return fmt.Errorf("编码企业微信消息失败: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("创建企业微信请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "kimi-balance-monitor/1.0")

	response, err := c.httpClient.Do(request)
	if err != nil {
		// url.Error 会带完整 webhook URL；只保留底层错误，避免泄露机器人密钥。
		if urlError, ok := errors.AsType[*url.Error](err); ok {
			err = urlError.Err
		}
		return fmt.Errorf("请求企业微信 webhook 失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("企业微信 webhook 返回 HTTP %d", response.StatusCode)
	}

	var result struct {
		Errcode *int   `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&result); err != nil {
		return fmt.Errorf("解析企业微信响应失败: %w", err)
	}
	if result.Errcode == nil {
		return fmt.Errorf("企业微信响应缺少 errcode")
	}
	if *result.Errcode != 0 {
		msg := strings.ReplaceAll(result.Errmsg, c.webhookURL, "[已隐藏]")
		u, _ := url.Parse(c.webhookURL)
		if key := u.Query().Get("key"); key != "" {
			msg = strings.ReplaceAll(msg, key, "[已隐藏]")
		}
		return fmt.Errorf("企业微信返回错误(errcode=%d): %s", *result.Errcode, msg)
	}
	return nil
}
