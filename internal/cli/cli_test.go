package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/testdb"
)

func TestCLIEndToEnd(t *testing.T) {
	dsn := testdb.DSN(t)
	const plaintext = "sk-abcdefghijklmnop12345678"
	var mutex sync.Mutex
	available := "50"
	kimiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+plaintext {
			t.Errorf("Kimi 请求未使用已保存的令牌")
		}
		mutex.Lock()
		value := available
		mutex.Unlock()
		_, _ = fmt.Fprintf(writer, `{"code":0,"status":true,"scode":"0x0","data":{"available_balance":%s,"voucher_balance":0,"cash_balance":%s}}`, value, value)
	}))
	defer kimiServer.Close()

	messages := make(chan string, 2)
	wecomServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Msgtype  string `json:"msgtype"`
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Msgtype != "markdown" {
			t.Errorf("msgtype = %s", payload.Msgtype)
		}
		messages <- payload.Markdown.Content
		_, _ = fmt.Fprint(writer, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer wecomServer.Close()

	masterKey, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	configBody := fmt.Sprintf(`
database:
  dsn: %q
security:
  master_key: %q
kimi:
  api_base_url: %q
monitor:
  interval: 10m
  low_balance: 10
  concurrency: 4
wecom:
  webhook_url: %q
`, dsn, masterKey, kimiServer.URL, wecomServer.URL+"?key=test-secret")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) []string {
		return append([]string{"--config", configPath}, args...)
	}

	var stdout bytes.Buffer
	logger := zerolog.New(&stdout).With().Timestamp().Logger()
	if code := Run(context.Background(), command("add", "--stdin"), strings.NewReader(plaintext+"\n"), &stdout, &logger); code != 0 {
		t.Fatalf("add 退出码 = %d，错误 = %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "sk-abcd****5678") || strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("add 输出打码错误: %s", stdout.String())
	}
	var addLog struct {
		Level     string `json:"level"`
		TokenID   int64  `json:"token_id"`
		TokenName string `json:"token_name"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &addLog); err != nil {
		t.Fatalf("add 未输出有效的 JSON 日志: %v，输出 = %s", err, stdout.String())
	}
	if addLog.Level != "info" || addLog.TokenID != 1 || addLog.TokenName != "sk-abcd****5678" || addLog.Message != "已添加令牌" {
		t.Fatalf("add 结构化日志错误: %+v", addLog)
	}
	stdout.Reset()
	if code := Run(context.Background(), command("remark", "1", "生产环境主账号"), nil, &stdout, &logger); code != 0 {
		t.Fatalf("remark 退出码 = %d，错误 = %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "已更新令牌备注") || strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("remark 输出错误: %s", stdout.String())
	}
	stdout.Reset()
	if code := Run(context.Background(), command("add", "--stdin"), strings.NewReader(plaintext), &stdout, &logger); code != 1 {
		t.Fatalf("重复 add 退出码 = %d，期望 1", code)
	}
	var errorLog struct {
		Level   string `json:"level"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &errorLog); err != nil {
		t.Fatalf("重复 add 未输出有效的 JSON 日志: %v，输出 = %s", err, stdout.String())
	}
	if errorLog.Level != "error" || errorLog.Message != "命令执行失败" || !strings.Contains(errorLog.Error, "不可重复添加") {
		t.Fatalf("重复添加错误日志不明确: %+v", errorLog)
	}

	for index, expected := range []string{"首次记录", "消耗"} {
		if index == 1 {
			mutex.Lock()
			available = "5"
			mutex.Unlock()
		}
		stdout.Reset()
		if code := Run(context.Background(), command("run", "--once"), nil, &stdout, &logger); code != 0 {
			t.Fatalf("第 %d 轮 run 退出码 = %d，错误 = %s", index+1, code, stdout.String())
		}
		var roundLog struct {
			Level        string `json:"level"`
			TokenCount   int    `json:"token_count"`
			FailureCount int    `json:"failure_count"`
			MessageCount int    `json:"message_count"`
			Message      string `json:"message"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &roundLog); err != nil {
			t.Fatalf("第 %d 轮未输出有效的 JSON 日志: %v，输出 = %s", index+1, err, stdout.String())
		}
		if roundLog.Level != "info" || roundLog.TokenCount != 1 || roundLog.FailureCount != 0 || roundLog.MessageCount != 1 || roundLog.Message != "本轮完成" {
			t.Fatalf("第 %d 轮结构化日志错误: %+v", index+1, roundLog)
		}
		message := <-messages
		if !strings.Contains(message, expected) || !strings.Contains(message, "sk-abcd****5678") {
			t.Fatalf("第 %d 轮报告缺少预期内容: %s", index+1, message)
		}
		if !strings.Contains(message, "备注：生产环境主账号") {
			t.Fatalf("第 %d 轮报告缺少令牌备注: %s", index+1, message)
		}
		if strings.Contains(message, plaintext) {
			t.Fatal("报告泄露了令牌明文")
		}
		if index == 1 && (!strings.Contains(message, "余额不足") || !strings.Contains(message, "¥5.000000")) {
			t.Fatalf("低余额报告错误: %s", message)
		}
	}

	stdout.Reset()
	if code := Run(context.Background(), command("list"), nil, &stdout, &logger); code != 0 {
		t.Fatalf("list 失败: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), plaintext) || !strings.Contains(stdout.String(), "sk-abcd****5678") || !strings.Contains(stdout.String(), "生产环境主账号") {
		t.Fatalf("list 输出错误: %s", stdout.String())
	}
	stdout.Reset()
	if code := Run(context.Background(), command("remove", "sk-abcd****5678"), nil, &stdout, &logger); code != 0 {
		t.Fatalf("remove 失败: %s", stdout.String())
	}
}

func TestRootCommandConfigFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "默认配置", args: []string{"generate-key"}, want: "config.yaml"},
		{name: "长参数", args: []string{"--config=/tmp/test.yaml", "generate-key"}, want: "/tmp/test.yaml"},
		{name: "短参数", args: []string{"-c", "/tmp/test.yaml", "generate-key"}, want: "/tmp/test.yaml"},
		{name: "子命令后", args: []string{"generate-key", "--config", "/tmp/test.yaml"}, want: "/tmp/test.yaml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			logger := zerolog.Nop()
			root := newRootCommand(strings.NewReader(""), &stdout, &logger)
			root.SetArgs(test.args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("执行 generate-key 失败: %v", err)
			}
			got, err := root.PersistentFlags().GetString("config")
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("config = %q，期望 %q", got, test.want)
			}
			if stdout.Len() == 0 {
				t.Fatal("generate-key 未输出主密钥")
			}
		})
	}
}

func TestCLIHelpAndMissingCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "无命令", wantCode: 2},
		{name: "help 命令", args: []string{"help"}, wantCode: 0},
		{name: "长帮助参数", args: []string{"--help"}, wantCode: 0},
		{name: "短帮助参数", args: []string{"-h"}, wantCode: 0},
		{name: "子命令帮助", args: []string{"add", "--help"}, wantCode: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			logger := zerolog.New(&stdout)
			if code := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &logger); code != test.wantCode {
				t.Fatalf("退出码 = %d，期望 %d，输出 = %s", code, test.wantCode, stdout.String())
			}
			if !strings.Contains(stdout.String(), "Usage:") {
				t.Fatalf("帮助输出缺少 Usage: %s", stdout.String())
			}
		})
	}
}

func TestCLIArgumentValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "generate-key 多余参数", args: []string{"generate-key", "extra"}},
		{name: "add 缺少令牌", args: []string{"add"}},
		{name: "add stdin 与位置参数互斥", args: []string{"add", "--stdin", "extra"}},
		{name: "list 多余参数", args: []string{"list", "extra"}},
		{name: "remark 缺少备注", args: []string{"remark", "1"}},
		{name: "remark 参数过多", args: []string{"remark", "1", "备注", "extra"}},
		{name: "remove 缺少标识", args: []string{"remove"}},
		{name: "remove 参数过多", args: []string{"remove", "1", "2"}},
		{name: "run 多余参数", args: []string{"run", "extra"}},
		{name: "未知 flag", args: []string{"run", "--unknown"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			logger := zerolog.New(&stdout)
			if code := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &logger); code != 1 {
				t.Fatalf("退出码 = %d，期望 1，输出 = %s", code, stdout.String())
			}
			if !strings.Contains(stdout.String(), "命令执行失败") {
				t.Fatalf("缺少参数错误日志: %s", stdout.String())
			}
		})
	}
}
