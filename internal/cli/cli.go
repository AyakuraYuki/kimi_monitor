package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	prettytable "github.com/jedib0t/go-pretty/v6/table"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/kimi"
	"kimi_balance_monitor/internal/monitor"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/web"
)

var errCommandRequired = errors.New("需要指定要执行的命令")

// Run 执行命令行并返回进程退出码。
func Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, logger *zerolog.Logger) int {
	root := newRootCommand(stdin, stdout, logger)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, errCommandRequired) {
			return 2
		}
		logger.Error().Err(err).Msg("命令执行失败")
		return 1
	}
	return 0
}

func newRootCommand(stdin io.Reader, stdout io.Writer, logger *zerolog.Logger) *cobra.Command {
	configPath := config.DefaultPath
	root := &cobra.Command{
		Use:   "kimi-monitor",
		Short: "Kimi 令牌余额监控",
		Long: `Kimi 令牌余额监控

配置项：
  security.master_key   32 字节主密钥的 Base64 或十六进制表示
  database.dsn          PostgreSQL DSN
  kimi.api_base_url     Kimi API 源站，默认 https://api.moonshot.cn
  monitor.interval      查询间隔，默认 10m
  monitor.low_balance   低余额预警线（元），默认 10
  monitor.concurrency   并发查询数，默认 4
  wecom.webhook_url     企业微信群机器人默认 webhook 地址
  web.enabled           运行时启用管理界面，默认关闭

推荐使用 add --stdin，避免明文令牌进入 shell 历史。
配置文件可能包含敏感信息，权限必须为 0600。`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Help(); err != nil {
				return err
			}
			return errCommandRequired
		},
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stdout)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if strings.TrimSpace(configPath) == "" {
			return fmt.Errorf("--config 不能为空")
		}
		return nil
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", config.DefaultPath, "YAML 配置文件")
	root.AddCommand(
		newGenerateKeyCommand(),
		newHashPasswordCommand(),
		newAddCommand(&configPath, logger),
		newListCommand(&configPath),
		newRemarkCommand(&configPath, logger),
		newRemoveCommand(&configPath, logger),
		newRunCommand(&configPath, logger),
	)
	return root
}

func newGenerateKeyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "generate-key",
		Short: "生成 32 字节主密钥",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return generateKey(cmd.OutOrStdout())
		},
	}
}

func newHashPasswordCommand() *cobra.Command {
	var stdinMode bool
	cmd := &cobra.Command{
		Use: "hash-password", Short: "为本地管理员生成 bcrypt 密码哈希",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return errors.New("密码请从终端或标准输入读取")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			input := cmd.InOrStdin()
			var password []byte
			var err error
			if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
				_, _ = fmt.Fprint(cmd.ErrOrStderr(), "请输入管理员密码（输入隐藏）：")
				password, err = term.ReadPassword(int(file.Fd()))
				_, _ = fmt.Fprintln(cmd.ErrOrStderr())
			} else {
				if !stdinMode {
					return errors.New("管道输入请使用 hash-password --stdin")
				}
				var line string
				line, err = bufio.NewReader(io.LimitReader(input, 75)).ReadString('\n')
				if errors.Is(err, io.EOF) {
					err = nil
				}
				password = []byte(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
			}
			if err != nil {
				return errors.New("读取密码失败")
			}
			defer clear(password)
			if len(password) < 12 || len(password) > 72 {
				return errors.New("管理员密码须为 12 至 72 个 UTF-8 字节")
			}
			hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(hash))
			return err
		},
	}
	cmd.Flags().BoolVar(&stdinMode, "stdin", false, "从管道读取一行密码")
	cmd.SetErr(os.Stderr)
	cmd.SetFlagErrorFunc(func(*cobra.Command, error) error {
		return errors.New("hash-password 仅支持 --stdin 参数")
	})
	return cmd
}

func newAddCommand(configPath *string, logger *zerolog.Logger) *cobra.Command {
	var fromStdin bool
	command := &cobra.Command{
		Use:   "add <sk-...>",
		Short: "添加 Kimi 令牌",
		Example: `  kimi-monitor add --stdin
  kimi-monitor add <sk-...>`,
		Args: func(_ *cobra.Command, args []string) error {
			if (fromStdin && len(args) != 0) || (!fromStdin && len(args) != 1) {
				return fmt.Errorf("用法：kimi-monitor add --stdin，或 kimi-monitor add <sk-...>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return addToken(cmd.Context(), *configPath, args, fromStdin, cmd.InOrStdin(), logger)
		},
	}
	command.Flags().BoolVar(&fromStdin, "stdin", false, "从标准输入读取令牌")
	return command
}

func newListCommand(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已保存的令牌",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listTokens(cmd.Context(), *configPath, cmd.OutOrStdout())
		},
	}
}

func newRemarkCommand(configPath *string, logger *zerolog.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "remark <ID|打码名称> <备注>",
		Short: "设置或清除令牌备注",
		Example: `  kimi-monitor remark 1 '生产环境主账号'
  kimi-monitor remark 1 ''`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setTokenRemark(cmd.Context(), *configPath, args[0], args[1], logger)
		},
	}
}

func newRemoveCommand(configPath *string, logger *zerolog.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <ID|打码名称>",
		Short: "删除令牌及其余额历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeToken(cmd.Context(), *configPath, args[0], logger)
		},
	}
}

func newRunCommand(configPath *string, logger *zerolog.Logger) *cobra.Command {
	var once bool
	command := &cobra.Command{
		Use:   "run",
		Short: "查询令牌余额并推送企业微信报告",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMonitor(cmd.Context(), *configPath, once, logger)
		},
	}
	command.Flags().BoolVar(&once, "once", false, "只执行一轮")
	return command
}

func generateKey(stdout io.Writer) error {
	key, err := secrets.GenerateKey()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, key)
	return err
}

const maxTokenBytes = 4096

func readTokenFromStdin(stdin io.Reader) (string, error) {
	// 最多读取一个令牌及 LF/CRLF；读到首个换行即返回，不等待 EOF。
	reader := bufio.NewReader(io.LimitReader(stdin, maxTokenBytes+2))
	data, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("从标准输入读取令牌失败: %w", err)
	}
	token := strings.TrimSpace(data)
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("令牌输入超过 %d 字节", maxTokenBytes)
	}
	return token, nil
}

func addToken(ctx context.Context, configPath string, args []string, fromStdin bool, stdin io.Reader, logger *zerolog.Logger) error {
	var token string
	if fromStdin {
		input, err := readTokenFromStdin(stdin)
		if err != nil {
			return err
		}
		token = input
	} else {
		token = strings.TrimSpace(args[0])
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
	logger.Info().
		Int64("token_id", tokenRecord.ID).
		Str("token_name", tokenRecord.Name).
		Msg("已添加令牌")
	return nil
}

func listTokens(ctx context.Context, configPath string, stdout io.Writer) error {
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
	tb.SetOutputMirror(stdout)
	tb.AppendHeader(prettytable.Row{"ID", "Name", "Remark", "Registered Time"})
	for _, token := range tokens {
		tb.AppendRow(prettytable.Row{token.ID, token.Name, token.Remark, token.CreatedAt.In(config.CST()).Format(time.DateTime)})
	}
	tb.Render()
	return nil
}

func setTokenRemark(ctx context.Context, configPath, selector, remark string, logger *zerolog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	token, err := database.SetTokenRemark(ctx, selector, remark)
	if err != nil {
		return err
	}
	logger.Info().
		Int64("token_id", token.ID).
		Str("token_name", token.Name).
		Msg("已更新令牌备注")
	return nil
}

func removeToken(ctx context.Context, configPath string, identifier string, logger *zerolog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	name, err := database.RemoveToken(ctx, identifier)
	if err != nil {
		return err
	}
	logger.Info().Str("token_name", name).Msg("已删除令牌")
	return nil
}

func runMonitor(ctx context.Context, configPath string, once bool, logger *zerolog.Logger) error {
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

	delivery := &monitor.Delivery{
		Store:       database,
		Cipher:      cipher,
		FallbackURL: cfg.WeCom.WebhookURL,
		HTTPClient:  httpClient,
	}

	if once {
		return monitor.RunOnce(ctx, service, delivery, cfg.LowBalanceMicros(), logger)
	}

	if !cfg.Web.Enabled {
		return monitor.Run(ctx, service, delivery, cfg.Monitor.Interval.Duration, cfg.LowBalanceMicros(), logger)
	}

	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return web.Run(runCtx, cfg, database, cipher, logger)
	})
	group.Go(func() error {
		return monitor.Run(runCtx, service, delivery, cfg.Monitor.Interval.Duration, cfg.LowBalanceMicros(), logger)
	})
	return group.Wait()
}

func cipherFromConfig(cfg config.Config) (*secrets.Cipher, error) {
	key, err := secrets.ParseKey(cfg.Security.MasterKey)
	if err != nil {
		return nil, err
	}
	return secrets.NewCipher(key)
}

func openDatabase(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, cfg.Database.DSN)
}
