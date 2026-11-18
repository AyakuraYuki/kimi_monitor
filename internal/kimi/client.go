package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kimi_balance_monitor/internal/model"
)

const maxResponseBytes = 1 << 20

// Client 调用 Kimi 开放平台的余额接口。
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// NewClient 创建余额查询客户端，baseURL 应为 API 源站而非完整余额路径。
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.ParseRequestURI(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("kimi.api_base_url 必须是有效的 HTTP(S) 地址")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{endpoint: baseURL + "/v1/users/me/balance", httpClient: httpClient}, nil
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type balanceResponse struct {
	Code   *int         `json:"code"`
	Status *bool        `json:"status"`
	Scode  string       `json:"scode"`
	Error  *errorDetail `json:"error"`
	Data   *struct {
		Available json.Number `json:"available_balance"`
		Voucher   json.Number `json:"voucher_balance"`
		Cash      json.Number `json:"cash_balance"`
	} `json:"data"`
}

// FetchBalance 查询余额，并将 API 浮点数字精确转换为整数微元。
func (c *Client) FetchBalance(ctx context.Context, token string) (model.Balance, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return model.Balance{}, fmt.Errorf("创建余额请求失败: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "kimi-balance-monitor/1.0")

	response, err := c.httpClient.Do(request)
	if err != nil {
		if urlError, ok := errors.AsType[*url.Error](err); ok {
			err = urlError.Err
		}
		return model.Balance{}, fmt.Errorf("请求 Kimi 余额接口失败: %w", err)
	}
	defer response.Body.Close()

	body := io.LimitReader(response.Body, maxResponseBytes)
	if response.StatusCode != http.StatusOK {
		limited, _ := io.ReadAll(io.LimitReader(body, 64<<10))
		var payload struct {
			Error *errorDetail `json:"error"`
		}
		_ = json.Unmarshal(limited, &payload)
		message := "无错误详情"
		if payload.Error != nil && payload.Error.Message != "" {
			message = payload.Error.Message
		} else if text := strings.TrimSpace(string(limited)); text != "" {
			runes := []rune(text)
			if len(runes) > 256 {
				text = string(runes[:256])
			}
			message = text
		}
		return model.Balance{}, fmt.Errorf("Kimi API 返回 %s: %s", response.Status, redactToken(message, token))
	}

	var payload balanceResponse
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&payload); err != nil {
		return model.Balance{}, fmt.Errorf("解析 Kimi 余额响应失败: %w", err)
	}
	if payload.Code == nil || payload.Status == nil {
		return model.Balance{}, fmt.Errorf("Kimi 余额响应缺少 code 或 status 字段")
	}
	if *payload.Code != 0 || !*payload.Status {
		message := "未知业务错误"
		if payload.Error != nil && payload.Error.Message != "" {
			message = payload.Error.Message
		}
		return model.Balance{}, fmt.Errorf("Kimi API 业务错误(code=%d, scode=%s): %s",
			*payload.Code, redactToken(payload.Scode, token), redactToken(message, token))
	}
	if payload.Data == nil {
		return model.Balance{}, fmt.Errorf("Kimi 余额响应缺少 data 字段")
	}

	available, err := model.ParseMicros(payload.Data.Available.String())
	if err != nil {
		return model.Balance{}, fmt.Errorf("解析 available_balance 失败: %w", err)
	}
	voucher, err := model.ParseMicros(payload.Data.Voucher.String())
	if err != nil {
		return model.Balance{}, fmt.Errorf("解析 voucher_balance 失败: %w", err)
	}
	cash, err := model.ParseMicros(payload.Data.Cash.String())
	if err != nil {
		return model.Balance{}, fmt.Errorf("解析 cash_balance 失败: %w", err)
	}
	return model.Balance{AvailableMicros: available, VoucherMicros: voucher, CashMicros: cash}, nil
}

// redactToken 防止异常的上游错误响应把 Authorization 令牌回显到日志或报告。
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[已隐藏]")
}
