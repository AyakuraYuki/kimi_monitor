package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, directory, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := writeConfig(t, directory, `
database:
  path: data/monitor.db
security:
  master_key: "unused-in-this-test"
kimi:
  api_base_url: https://api.moonshot.cn
monitor:
  interval: 10m
  low_balance: 12.5
  concurrency: 3
wecom:
  webhook_url: "https://example.invalid/webhook"
`, 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedDatabase := filepath.Join(directory, "data", "monitor.db")
	if config.Database.Path != expectedDatabase {
		t.Fatalf("数据库路径 = %s，期望 %s", config.Database.Path, expectedDatabase)
	}
	if config.SourcePath != path {
		t.Fatalf("配置源路径 = %s，期望 %s", config.SourcePath, path)
	}
	if config.Monitor.Interval.Duration != 10*time.Minute || config.Monitor.Concurrency != 3 {
		t.Fatalf("监控配置解析错误: %+v", config.Monitor)
	}
	if config.LowBalanceMicros() != 12_500_000 {
		t.Fatalf("预警线 = %d，期望 12500000", config.LowBalanceMicros())
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, t.TempDir(), "security:\n  master_key: test\n", 0o600)
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Monitor.Interval.Duration != 10*time.Minute || config.Monitor.Concurrency != 4 || config.LowBalanceMicros() != 10_000_000 {
		t.Fatalf("默认监控配置错误: %+v", config.Monitor)
	}
	if config.Kimi.APIBaseURL != "https://api.moonshot.cn" {
		t.Fatalf("默认 API 地址错误: %s", config.Kimi.APIBaseURL)
	}
}

func TestLoadRejectsUnsafeOrInvalidConfig(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		body     string
		mode     os.FileMode
		contains string
	}{
		{name: "未知字段", body: "unknown: true\n", mode: 0o600, contains: "field unknown not found"},
		{name: "权限过宽", body: "{}", mode: 0o644, contains: "chmod 600"},
		{name: "无效间隔", body: "monitor:\n  interval: soon\n", mode: 0o600, contains: "无效时间间隔"},
		{name: "负数预警线", body: "monitor:\n  low_balance: -1\n", mode: 0o600, contains: "不能为负数"},
		{name: "零并发", body: "monitor:\n  concurrency: 0\n", mode: 0o600, contains: "必须大于 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfig(t, t.TempDir(), test.body, test.mode)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("Load 错误 = %v，期望包含 %q", err, test.contains)
			}
		})
	}
}
