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

	"kimi_balance_monitor/internal/secrets"
)

func TestCLIEndToEnd(t *testing.T) {
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
  path: data/cli.db
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
`, masterKey, kimiServer.URL, wecomServer.URL+"?key=test-secret")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) []string {
		return append([]string{"--config", configPath}, args...)
	}

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), command("add", "--stdin"), strings.NewReader(plaintext+"\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("add 退出码 = %d，错误 = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sk-abcd****5678") || strings.Contains(stdout.String(), plaintext) {
		t.Fatalf("add 输出打码错误: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(directory, "data", "cli.db")); err != nil {
		t.Fatalf("数据库未按配置文件目录解析: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), command("add", "--stdin"), strings.NewReader(plaintext), &stdout, &stderr); code != 1 {
		t.Fatalf("重复 add 退出码 = %d，期望 1", code)
	}
	if !strings.Contains(stderr.String(), "不可重复添加") {
		t.Fatalf("重复添加错误不明确: %s", stderr.String())
	}

	for index, expected := range []string{"首次记录", "消耗"} {
		if index == 1 {
			mutex.Lock()
			available = "5"
			mutex.Unlock()
		}
		stdout.Reset()
		stderr.Reset()
		if code := Run(context.Background(), command("run", "--once"), nil, &stdout, &stderr); code != 0 {
			t.Fatalf("第 %d 轮 run 退出码 = %d，错误 = %s", index+1, code, stderr.String())
		}
		message := <-messages
		if !strings.Contains(message, expected) || !strings.Contains(message, "sk-abcd****5678") {
			t.Fatalf("第 %d 轮报告缺少预期内容: %s", index+1, message)
		}
		if strings.Contains(message, plaintext) {
			t.Fatal("报告泄露了令牌明文")
		}
		if index == 1 && (!strings.Contains(message, "余额不足") || !strings.Contains(message, "¥5.000000")) {
			t.Fatalf("低余额报告错误: %s", message)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), command("list"), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("list 失败: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), plaintext) || !strings.Contains(stdout.String(), "sk-abcd****5678") {
		t.Fatalf("list 输出错误: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), command("remove", "sk-abcd****5678"), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("remove 失败: %s", stderr.String())
	}
}

func TestParseGlobalArgs(t *testing.T) {
	t.Parallel()
	configPath, remaining, err := parseGlobalArgs([]string{"--config=/tmp/test.yaml", "list"})
	if err != nil || configPath != "/tmp/test.yaml" || len(remaining) != 1 || remaining[0] != "list" {
		t.Fatalf("全局参数解析错误: path=%q args=%v err=%v", configPath, remaining, err)
	}
	configPath, remaining, err = parseGlobalArgs([]string{"list"})
	if err != nil || configPath != "config.yaml" || remaining[0] != "list" {
		t.Fatalf("默认配置参数解析错误: path=%q args=%v err=%v", configPath, remaining, err)
	}
}
