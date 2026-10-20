// Payment Bridge：独立部署的 USDT（StartPay）收款网关。
//
// 对上游以「易支付协议」服务 new-api（new-api 无需任何改动，将其配置为一个
// 易支付平台），对下游调用 StartPay 数字货币收单 API：
//
//	new-api --易支付 MD5--> payment-bridge --HMAC-SHA1--> StartPay
//	new-api <--易支付 MD5-- payment-bridge <--异步通知---- StartPay
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	ListenAddr string
	// 易支付商户参数，必须与 new-api 运营设置里的 EpayId / EpayKey 一致。
	EpayPID string
	EpayKey string
	// 本服务对外可访问的地址（含 scheme，不带路径），用于生成 StartPay
	// 的异步通知地址，例如 https://usdt.example.com。
	BridgePublicURL string
	// StartPay API 域名前缀，同时用于签名字符串。2025-08-21 之后创建的
	// Key 使用 https://api.startpay.ai/api。
	StartPayAPIBase   string
	StartPayAPIKey    string
	StartPayAPISecret string
	// type 为纯 "usdt" 时使用的默认区块链网络。
	StartPayChain    string
	StartPayCurrency string
	// StartPay 订单的客户信息字段（必填项），桥接侧无法获知终端用户，
	// 需要站点运营者自定义。
	CustFirstName string
	CustLastName  string
	// 可选：固定回调到 new-api 的易支付通知地址。为空时使用 new-api 下单
	// 时传来的 notify_url（重启丢失订单状态后无法还原该地址）。
	FallbackNotifyURL string
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:        envOr("BRIDGE_LISTEN", ":8787"),
		EpayPID:           strings.TrimSpace(os.Getenv("EPAY_PID")),
		EpayKey:           os.Getenv("EPAY_KEY"),
		BridgePublicURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("BRIDGE_PUBLIC_URL")), "/"),
		StartPayAPIBase:   strings.TrimRight(strings.TrimSpace(getEnvOr("STARTPAY_API_BASE", "https://api.startpay.ai/api")), "/"),
		StartPayAPIKey:    strings.TrimSpace(os.Getenv("STARTPAY_API_KEY")),
		StartPayAPISecret: os.Getenv("STARTPAY_API_SECRET"),
		StartPayChain:     strings.ToUpper(strings.TrimSpace(getEnvOr("STARTPAY_CHAIN", "TRON"))),
		StartPayCurrency:  strings.ToUpper(strings.TrimSpace(getEnvOr("STARTPAY_CURRENCY", "USDT"))),
		CustFirstName:     strings.TrimSpace(getEnvOr("STARTPAY_CUST_FIRST_NAME", "NewAPI")),
		CustLastName:      strings.TrimSpace(getEnvOr("STARTPAY_CUST_LAST_NAME", "User")),
		FallbackNotifyURL: strings.TrimSpace(os.Getenv("NEWAPI_NOTIFY_URL")),
	}

	var missing []string
	if cfg.EpayPID == "" {
		missing = append(missing, "EPAY_PID")
	}
	if cfg.EpayKey == "" {
		missing = append(missing, "EPAY_KEY")
	}
	if cfg.BridgePublicURL == "" {
		missing = append(missing, "BRIDGE_PUBLIC_URL")
	}
	if cfg.StartPayAPIKey == "" {
		missing = append(missing, "STARTPAY_API_KEY")
	}
	if cfg.StartPayAPISecret == "" {
		missing = append(missing, "STARTPAY_API_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("缺少必需的环境变量: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(cfg.BridgePublicURL, "https://") && !strings.HasPrefix(cfg.BridgePublicURL, "http://") {
		return nil, errors.New("BRIDGE_PUBLIC_URL 必须以 http:// 或 https:// 开头")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvOr(key, fallback string) string {
	return envOr(key, fallback)
}

// newServeMux 注册桥接路由。basePath 来自 BRIDGE_PUBLIC_URL 的路径部分
// （如 /bridge）；健康检查同时注册在根路径与前缀路径下，便于 LB 直连检查。
func newServeMux(cfg *Config, bridge *Bridge, basePath string) *http.ServeMux {
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health)
	if basePath != "" {
		mux.HandleFunc("GET "+basePath+"/healthz", health)
	}
	mux.HandleFunc(basePath+"/submit.php", bridge.HandleSubmit)
	mux.HandleFunc("POST "+basePath+"/notify/startpay", bridge.HandleNotify)
	return mux
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("配置加载失败", "error", err)
		os.Exit(1)
	}

	bridge, err := NewBridge(cfg, logger)
	if err != nil {
		logger.Error("桥接初始化失败", "error", err)
		os.Exit(1)
	}

	// 反向代理/负载均衡通常不做路径重写，因此当 BRIDGE_PUBLIC_URL 带路径
	// 前缀（如 https://api.example.com/bridge）时，路由也注册在同样的前缀下。
	publicURL, err := url.Parse(cfg.BridgePublicURL)
	if err != nil {
		logger.Error("BRIDGE_PUBLIC_URL 解析失败", "error", err)
		os.Exit(1)
	}
	mux := newServeMux(cfg, bridge, strings.TrimRight(publicURL.Path, "/"))

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("payment-bridge 已启动", "listen", cfg.ListenAddr, "startpay_api_base", cfg.StartPayAPIBase)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，正在关闭")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("关闭超时", "error", err)
	}
}
