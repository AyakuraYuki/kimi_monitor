package kimi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchBalance(t *testing.T) {
	t.Parallel()
	const token = "sk-abcdefghijklmnop12345678"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/users/me/balance" {
			t.Errorf("请求路径 = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Error("Authorization 请求头不正确")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{
			"code": 0,
			"data": {"available_balance": 49.58894, "voucher_balance": 46.58893, "cash_balance": 3.00001},
			"scode": "0x0",
			"status": true
		}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	balance, err := client.FetchBalance(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableMicros != 49_588_940 || balance.VoucherMicros != 46_588_930 || balance.CashMicros != 3_000_010 {
		t.Fatalf("余额转换错误: %+v", balance)
	}
}

func TestFetchBalanceErrors(t *testing.T) {
	t.Parallel()
	const echoToken = "sk-echotoken12345678"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer sk-unauthorized123456":
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(writer, `{"error":{"message":"API Key 无效"}}`)
		case "Bearer sk-businesserror123456":
			_, _ = fmt.Fprint(writer, `{"code":5,"status":false,"scode":"0x5","error":{"message":"余额服务不可用"}}`)
		case "Bearer " + echoToken:
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(writer, `{"error":{"message":"错误的令牌 %s"}}`, echoToken)
		default:
			_, _ = fmt.Fprint(writer, `{"code":0,"status":true,"data":{"available_balance":0.0000001,"voucher_balance":0,"cash_balance":0}}`)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token   string
		message string
	}{
		{token: "sk-unauthorized123456", message: "API Key 无效"},
		{token: "sk-businesserror123456", message: "余额服务不可用"},
		{token: "sk-precisionerror123456", message: "小数精度超过 6 位"},
	} {
		_, err := client.FetchBalance(context.Background(), test.token)
		if err == nil || !strings.Contains(err.Error(), test.message) {
			t.Errorf("令牌 %s 的错误 = %v，期望包含 %q", test.token, err, test.message)
		}
	}

	_, err = client.FetchBalance(context.Background(), echoToken)
	if err == nil || strings.Contains(err.Error(), echoToken) || !strings.Contains(err.Error(), "[已隐藏]") {
		t.Fatalf("上游错误响应不应回显令牌: %v", err)
	}
}
