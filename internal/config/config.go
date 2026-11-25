package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kimi_balance_monitor/internal/model"

	"gopkg.in/yaml.v3"
)

var cst *time.Location

func init() {
	var err error
	cst, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
}

func CST() *time.Location { return cst }

const DefaultPath = "config.yaml"

// Duration 支持将 YAML 中的 10m、30s 等字符串解析为时间间隔。
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("时间间隔必须是字符串")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("无效时间间隔 %q: %w", node.Value, err)
	}
	d.Duration = parsed
	return nil
}

// Amount 同时接受 YAML 中未加引号的数字和加引号的金额字符串。
type Amount string

func (a *Amount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("金额必须是数字或字符串")
	}
	*a = Amount(strings.TrimSpace(node.Value))
	return nil
}

// Config 是服务的完整 YAML 配置。
type Config struct {
	Database struct {
		DSN string `yaml:"dsn"`
	} `yaml:"database"`
	Security struct {
		MasterKey string `yaml:"master_key"`
	} `yaml:"security"`
	Kimi struct {
		APIBaseURL string `yaml:"api_base_url"`
	} `yaml:"kimi"`
	Monitor struct {
		Interval    Duration `yaml:"interval"`
		LowBalance  Amount   `yaml:"low_balance"`
		Concurrency int      `yaml:"concurrency"`
	} `yaml:"monitor"`
	WeCom struct {
		WebhookURL string `yaml:"webhook_url"`
	} `yaml:"wecom"`
	Web WebConfig `yaml:"web"`

	SourcePath       string `yaml:"-"`
	lowBalanceMicros int64
}

// Default 返回所有可选配置的默认值。
func Default() Config {
	var config Config
	config.Kimi.APIBaseURL = "https://api.moonshot.cn"
	config.Monitor.Interval = Duration{Duration: 10 * time.Minute}
	config.Monitor.LowBalance = "10"
	config.Monitor.Concurrency = 4
	config.Web.Listen = "127.0.0.1:8080"
	return config
}

// LowBalanceMicros 返回加载时已验证的低余额预警线。
func (c Config) LowBalanceMicros() int64 {
	return c.lowBalanceMicros
}

// Load 严格解析 YAML，拒绝未知字段，并验证 DSN 和监控参数。
func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件信息失败: %w", err)
	}
	if info.IsDir() {
		return Config{}, fmt.Errorf("配置路径 %s 是目录", absolutePath)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return Config{}, fmt.Errorf("配置文件 %s 权限为 %04o，可能包含数据库 DSN、主密钥和 webhook，请执行 chmod 600 后再使用", absolutePath, permissions)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}

	config := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("解析 YAML 配置失败: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("配置文件只能包含一个 YAML 文档")
		}
		return Config{}, fmt.Errorf("解析额外 YAML 文档失败: %w", err)
	}

	config.Database.DSN = strings.TrimSpace(config.Database.DSN)
	if config.Database.DSN == "" {
		return Config{}, fmt.Errorf("database.dsn 不能为空")
	}
	if config.Monitor.Interval.Duration <= 0 {
		return Config{}, fmt.Errorf("monitor.interval 必须大于 0")
	}
	if config.Monitor.Concurrency <= 0 {
		return Config{}, fmt.Errorf("monitor.concurrency 必须大于 0")
	}
	lowBalanceMicros, err := model.ParseMicros(string(config.Monitor.LowBalance))
	if err != nil {
		return Config{}, fmt.Errorf("解析 monitor.low_balance 失败: %w", err)
	}
	if lowBalanceMicros < 0 {
		return Config{}, fmt.Errorf("monitor.low_balance 不能为负数")
	}
	config.lowBalanceMicros = lowBalanceMicros
	if config.Web.Enabled {
		if err := config.Web.Validate(); err != nil {
			return Config{}, err
		}
	}
	config.SourcePath = absolutePath
	return config, nil
}
