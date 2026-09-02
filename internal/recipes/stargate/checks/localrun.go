package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/soulteary/ci-recipes/internal/cli"
)

const (
	green  = "\x1b[0;32m"
	yellow = "\x1b[1;33m"
	blue   = "\x1b[0;34m"
	reset  = "\x1b[0m"
)

type localConfig struct {
	port                  string
	hostPort              string
	authHost              string
	passwords             string
	debug                 string
	language              string
	cookieSecure          string
	callbackAllowedHosts  string
	sessionExchangeSecret string
	wardenEnabled         string
	wardenURL             string
	wardenAPIKey          string
	wardenCacheTTL        string
}

func executeLocalRun(ctx context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer, opts options) error {
	customPort, help, err := parseLocalArgs(args)
	if err != nil {
		return cli.Exit(2, "%v", err)
	}
	if help {
		return writeLocalHelp(stdout)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, statErr := os.Stat(filepath.Join(root, "src")); statErr != nil || !info.IsDir() {
		return errors.New("请在 Stargate 仓库根目录运行此程序: stargate src directory is missing")
	}
	if _, err := opts.runner.LookPath("go"); err != nil {
		return errors.New("未找到 Go，请先安装 Go 1.27.0+: Go executable was not found")
	}

	config, err := buildLocalConfig(customPort, opts.getenv)
	if err != nil {
		return cli.Exit(1, "%v", err)
	}
	if err := writeLocalSummary(stdout, config, customPort != ""); err != nil {
		return fmt.Errorf("write local launcher summary: %w", err)
	}

	shortCommit := gitValue(ctx, opts.runner, root, []string{"rev-parse", "--short", "HEAD"}, "local")
	commit := gitValue(ctx, opts.runner, root, []string{"rev-parse", "HEAD"}, "unknown")
	buildDate := opts.now().Format("2006-01-02T15:04:05-0700")
	ldflags := fmt.Sprintf("-X 'github.com/soulteary/version-kit/v2.Version=dev-%s' -X 'github.com/soulteary/version-kit/v2.Commit=%s' -X 'github.com/soulteary/version-kit/v2.BuildDate=%s'", shortCommit, commit, buildDate)

	environment := mergeEnvironment(opts.environ(), map[string]string{
		"AUTH_HOST":               config.authHost,
		"PASSWORDS":               config.passwords,
		"DEBUG":                   config.debug,
		"LANGUAGE":                config.language,
		"COOKIE_SECURE":           config.cookieSecure,
		"CALLBACK_ALLOWED_HOSTS":  config.callbackAllowedHosts,
		"SESSION_EXCHANGE_SECRET": config.sessionExchangeSecret,
		"PORT":                    config.port,
		"WARDEN_ENABLED":          config.wardenEnabled,
		"WARDEN_URL":              config.wardenURL,
		"WARDEN_API_KEY":          config.wardenAPIKey,
		"WARDEN_CACHE_TTL":        config.wardenCacheTTL,
	})
	if err := opts.runner.Run(ctx, Command{
		Name:   "go",
		Args:   []string{"run", "-ldflags", ldflags, "./cmd/stargate"},
		Dir:    filepath.Join(root, "src"),
		Env:    environment,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("run Stargate locally: %w", err)
	}
	return nil
}

func parseLocalArgs(args []string) (customPort string, help bool, err error) {
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-port", "--port":
			if index+1 >= len(args) || args[index+1] == "" {
				return "", false, fmt.Errorf("%s 需要端口值（1-65535，可选冒号前缀）", args[index])
			}
			customPort = args[index+1]
			index++
		case "-h", "--help":
			return "", true, nil
		default:
			return "", false, fmt.Errorf("未知参数: %s", args[index])
		}
	}
	return customPort, false, nil
}

func buildLocalConfig(customPort string, getenv func(string) string) (localConfig, error) {
	port := customPort
	if port == "" {
		port = valueOrDefault(getenv("PORT"), "8080")
	}
	hostPort, err := normalizePort(port)
	if err != nil {
		return localConfig{}, err
	}
	return localConfig{
		port:                  port,
		hostPort:              hostPort,
		authHost:              valueOrDefault(getenv("AUTH_HOST"), "localhost:"+hostPort),
		passwords:             valueOrDefault(getenv("PASSWORDS"), "plaintext:test123|admin123"),
		debug:                 valueOrDefault(getenv("DEBUG"), "true"),
		language:              valueOrDefault(getenv("LANGUAGE"), "zh"),
		cookieSecure:          valueOrDefault(getenv("COOKIE_SECURE"), "false"),
		callbackAllowedHosts:  valueOrDefault(getenv("CALLBACK_ALLOWED_HOSTS"), "localhost:"+hostPort),
		sessionExchangeSecret: valueOrDefault(getenv("SESSION_EXCHANGE_SECRET"), "local-development-session-secret-change-me"),
		wardenEnabled:         valueOrDefault(getenv("WARDEN_ENABLED"), "false"),
		wardenURL:             getenv("WARDEN_URL"),
		wardenAPIKey:          getenv("WARDEN_API_KEY"),
		wardenCacheTTL:        valueOrDefault(getenv("WARDEN_CACHE_TTL"), "300"),
	}, nil
}

func normalizePort(port string) (string, error) {
	hostPort := strings.TrimPrefix(port, ":")
	if hostPort == "" || len(hostPort) > 5 {
		return "", fmt.Errorf("无效端口: %s（应为 1-65535，可选冒号前缀）", port)
	}
	for _, character := range hostPort {
		if character < '0' || character > '9' {
			return "", fmt.Errorf("无效端口: %s（应为 1-65535，可选冒号前缀）", port)
		}
	}
	if strings.Count(port, ":") > 1 || (strings.Contains(port, ":") && !strings.HasPrefix(port, ":")) {
		return "", fmt.Errorf("无效端口: %s（应为 1-65535，可选冒号前缀）", port)
	}
	number, err := strconv.ParseUint(hostPort, 10, 16)
	if err != nil || number == 0 {
		return "", fmt.Errorf("无效端口: %s（应为 1-65535，可选冒号前缀）", port)
	}
	return hostPort, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func writeLocalHelp(stdout io.Writer) error {
	return writef(stdout, "StarGate 本地启动程序\n\n使用方式: ci-recipes stargate local-run [选项]\n\n选项:\n  -port, --port PORT     设置服务端口（默认: 8080）\n  -h, --help             显示帮助信息\n\n环境变量:\n  可以通过环境变量设置配置，命令行参数会覆盖环境变量\n  PORT                   服务端口\n  AUTH_HOST              认证服务主机名\n  PASSWORDS              密码配置\n  COOKIE_SECURE          Cookie 仅限 HTTPS（本地默认 false）\n  CALLBACK_ALLOWED_HOSTS 允许的回调主机\n  SESSION_EXCHANGE_SECRET 会话交换密钥（至少 32 字符）\n  DEBUG                  调试模式（true/false）\n  LANGUAGE               界面语言（zh/en）\n  WARDEN_ENABLED         启用 Warden 集成（true/false）\n  WARDEN_URL             Warden 服务地址\n  WARDEN_API_KEY         Warden API 密钥\n  WARDEN_CACHE_TTL       Warden 缓存 TTL（秒）\n")
}

func writeLocalSummary(stdout io.Writer, config localConfig, custom bool) error {
	var output strings.Builder
	fmt.Fprintf(&output, "%s=== StarGate 本地启动程序 ===%s\n\n", green, reset)
	fmt.Fprintf(&output, "%s配置信息:%s\n", blue, reset)
	fmt.Fprintf(&output, "  AUTH_HOST: %s\n", config.authHost)
	output.WriteString("  PASSWORDS: 已设置（已隐藏）\n")
	fmt.Fprintf(&output, "  DEBUG: %s\n  LANGUAGE: %s\n  COOKIE_SECURE: %s\n  端口: %s\n", config.debug, config.language, config.cookieSecure, config.port)
	if custom {
		fmt.Fprintf(&output, "  %s✓ 端口通过命令行参数设置: %s%s\n", green, config.port, reset)
	}
	fmt.Fprintf(&output, "\n%sWarden 配置:%s\n  WARDEN_ENABLED: %s\n", blue, reset, config.wardenEnabled)
	if config.wardenEnabled == "true" {
		wardenURL := config.wardenURL
		if wardenURL == "" {
			wardenURL = "未设置"
		}
		apiKey := ""
		if config.wardenAPIKey != "" {
			apiKey = "已设置"
		}
		fmt.Fprintf(&output, "  WARDEN_URL: %s\n  WARDEN_API_KEY: %s\n  WARDEN_CACHE_TTL: %s (秒)\n", wardenURL, apiKey, config.wardenCacheTTL)
	}
	fmt.Fprintf(&output, "\n%s提示:%s\n  1. 访问登录页面: http://localhost:%s/_login?callback=localhost:%s\n  2. 测试密码: test123 或 admin123\n", yellow, reset, config.hostPort, config.hostPort)
	if config.wardenEnabled == "true" {
		output.WriteString("  3. Warden 模式已启用，可以使用用户列表认证\n  4. 确保 WARDEN_URL 和 WARDEN_API_KEY 已正确配置\n")
	}
	output.WriteString("  按 Ctrl+C 停止服务\n\n")
	fmt.Fprintf(&output, "%s正在启动服务器...%s\n\n", green, reset)
	_, err := io.WriteString(stdout, output.String())
	return err
}

func gitValue(ctx context.Context, runner commandRunner, root string, args []string, fallback string) string {
	var output bytes.Buffer
	invocationArgs := append([]string{"-C", root}, args...)
	if err := runner.Run(ctx, Command{Name: "git", Args: invocationArgs, Stdout: &output, Stderr: io.Discard}); err != nil {
		return fallback
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return fallback
	}
	return value
}

func mergeEnvironment(base []string, replacements map[string]string) []string {
	result := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := replacements[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	keys := []string{
		"AUTH_HOST", "PASSWORDS", "DEBUG", "LANGUAGE", "COOKIE_SECURE",
		"CALLBACK_ALLOWED_HOSTS", "SESSION_EXCHANGE_SECRET", "PORT",
		"WARDEN_ENABLED", "WARDEN_URL", "WARDEN_API_KEY", "WARDEN_CACHE_TTL",
	}
	for _, key := range keys {
		result = append(result, key+"="+replacements[key])
	}
	return result
}
