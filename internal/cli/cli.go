package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	prettytable "github.com/jedib0t/go-pretty/v6/table"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/kimi"
	"kimi_balance_monitor/internal/monitor"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/wecom"
)

var cst *time.Location

func init() {
	var err error
	cst, err = time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
}

// Run 执行命令行并返回进程退出码。
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	configPath, remaining, err := parseGlobalArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误：%v\n", err)
		return 2
	}
	if len(remaining) == 0 {
		usage(stderr)
		return 2
	}
	switch remaining[0] {
	case "generate-key":
		err = generateKey(remaining[1:], stdout)
	case "add":
		err = addToken(ctx, configPath, remaining[1:], stdin, stdout)
	case "list":
		err = listTokens(ctx, configPath, remaining[1:], stdout)
	case "remove":
		err = removeToken(ctx, configPath, remaining[1:], stdout)
	case "run":
		err = runMonitor(ctx, configPath, remaining[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "未知命令：%s\n\n", remaining[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "错误：%v\n", err)
		return 1
	}
	return 0
}

func parseGlobalArgs(args []string) (string, []string, error) {
	configPath := config.DefaultPath
	for len(args) > 0 {
		switch {
		case args[0] == "--config" || args[0] == "-c":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", nil, fmt.Errorf("%s 后需要指定 YAML 配置文件", args[0])
			}
			configPath = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--config="):
			configPath = strings.TrimPrefix(args[0], "--config=")
			if strings.TrimSpace(configPath) == "" {
				return "", nil, fmt.Errorf("--config 不能为空")
			}
			args = args[1:]
		default:
			return configPath, args, nil
		}
	}
	return configPath, args, nil
}

func usage(writer io.Writer) {
	fmt.Fprint(writer, `Kimi 令牌余额监控

用法：
  kimi-monitor [--config config.yaml] generate-key
  kimi-monitor [--config config.yaml] add --stdin
  kimi-monitor [--config config.yaml] add <sk-...>
  kimi-monitor [--config config.yaml] list
  kimi-monitor [--config config.yaml] remove <ID|打码名称>
  kimi-monitor [--config config.yaml] run [--once]

全局参数：
  -c, --config <路径>  YAML 配置文件，默认 config.yaml

配置项：
  security.master_key   32 字节主密钥的 Base64 或十六进制表示
  database.path         SQLite 路径，相对路径锚定到配置文件目录
  kimi.api_base_url     Kimi API 源站，默认 https://api.moonshot.cn
  monitor.interval      查询间隔，默认 10m
  monitor.low_balance   低余额预警线（元），默认 10
  monitor.concurrency   并发查询数，默认 4
  wecom.webhook_url     企业微信群机器人 webhook 地址

推荐使用 add --stdin，避免明文令牌进入 shell 历史。
配置文件可能包含敏感信息，权限必须为 0600。
`)
}

func generateKey(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("generate-key 不接受参数")
	}
	key, err := secrets.GenerateKey()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, key)
	return err
}

func addToken(ctx context.Context, configPath string, args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("add", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fromStdin := flags.Bool("stdin", false, "从标准输入读取令牌")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("解析 add 参数失败: %w", err)
	}
	if (*fromStdin && flags.NArg() != 0) || (!*fromStdin && flags.NArg() != 1) {
		return fmt.Errorf("用法：kimi-monitor add --stdin，或 kimi-monitor add <sk-...>")
	}

	var token string
	if *fromStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, 4097))
		if err != nil {
			return fmt.Errorf("从标准输入读取令牌失败: %w", err)
		}
		if len(data) > 4096 {
			return fmt.Errorf("令牌输入超过 4096 字节")
		}
		token = strings.TrimSpace(string(data))
	} else {
		token = strings.TrimSpace(flags.Arg(0))
	}
	if token == "" {
		return fmt.Errorf("令牌不能为空")
	}
	name, err := secrets.MaskToken(token)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	cipher, err := cipherFromConfig(cfg)
	if err != nil {
		return err
	}
	ciphertext, err := cipher.Encrypt(token)
	if err != nil {
		return fmt.Errorf("加密令牌失败: %w", err)
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	tokenRecord, err := database.AddToken(ctx, name, cipher.Digest(token), ciphertext, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已添加令牌：ID=%d，名称=%s\n", tokenRecord.ID, tokenRecord.Name)
	return nil
}

func listTokens(ctx context.Context, configPath string, args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("list 不接受参数")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	tokens, err := database.ListTokens(ctx)
	if err != nil {
		return err
	}
	tb := prettytable.NewWriter()
	tb.SetOutputMirror(os.Stdout)
	tb.AppendHeader(prettytable.Row{"ID", "Name", "Registered Time"})
	for _, token := range tokens {
		tb.AppendRow(prettytable.Row{token.ID, token.Name, token.CreatedAt.In(cst).Format(time.DateTime)})
	}
	tb.Render()
	return nil
}

func removeToken(ctx context.Context, configPath string, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("用法：kimi-monitor remove <ID|打码名称>")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	name, err := database.RemoveToken(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已删除令牌：%s\n", name)
	return nil
}

func runMonitor(ctx context.Context, configPath string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	once := flags.Bool("once", false, "只执行一轮")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("解析 run 参数失败: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("run 不接受位置参数")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	cipher, err := cipherFromConfig(cfg)
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	api, err := kimi.NewClient(cfg.Kimi.APIBaseURL, httpClient)
	if err != nil {
		return err
	}
	notifier, err := wecom.NewClient(cfg.WeCom.WebhookURL, httpClient)
	if err != nil {
		return err
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	service := &monitor.Service{
		Store:       database,
		API:         api,
		Cipher:      cipher,
		MaxParallel: cfg.Monitor.Concurrency,
	}

	if *once {
		results, err := service.CheckAll(ctx)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Fprintln(stdout, "没有已配置的令牌，已跳过查询和推送")
			return nil
		}
		messages := monitor.BuildMessages(results, time.Now(), cfg.LowBalanceMicros())
		for index, message := range messages {
			if err := notifier.SendMarkdown(ctx, message); err != nil {
				return fmt.Errorf("推送第 %d/%d 条消息失败: %w", index+1, len(messages), err)
			}
		}
		failures := 0
		for _, result := range results {
			if result.Err != nil {
				failures++
			}
		}
		fmt.Fprintf(stdout, "本轮完成：%d 个令牌，%d 个查询异常，%d 条企业微信消息\n", len(results), failures, len(messages))
		return nil
	}

	logger := log.New(stderr, "", log.LstdFlags)
	return monitor.Run(ctx, service, notifier, cfg.Monitor.Interval.Duration, cfg.LowBalanceMicros(), logger.Printf)
}

func cipherFromConfig(cfg config.Config) (*secrets.Cipher, error) {
	key, err := secrets.ParseKey(cfg.Security.MasterKey)
	if err != nil {
		return nil, err
	}
	return secrets.NewCipher(key)
}

func openDatabase(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, cfg.Database.Path)
}
