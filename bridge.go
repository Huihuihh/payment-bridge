package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// notifyBodyLimit 限制通知请求体大小。
	notifyBodyLimit = 1 << 20
	// 订单状态保留时长：StartPay 订单最长 3 小时有效，晚到通知留足余量。
	orderStateTTL = 48 * time.Hour
	// 回调 new-api 的重试次数。
	epayCallbackAttempts = 3
	// 单笔金额上限（美分），防御异常金额进入 StartPay。
	maxMoneyCents int64 = 10_000_000 * 100
)

// orderState 保存 /submit.php 时的下单信息；StartPay 异步通知到达时用于
// 组装易支付回调参数。进程内状态仅是优化：重启后靠 StartPay 查单和
// NEWAPI_NOTIFY_URL 兑底。
type orderState struct {
	Money     string
	Name      string
	Type      string
	NotifyURL string
	CreatedAt time.Time
	Forwarded bool // 已成功回调 new-api，重复通知直接幂等应答
}

// Bridge 连接易支付协议与 StartPay 收单 API。
type Bridge struct {
	cfg       *Config
	logger    *slog.Logger
	sp        *startPayClient
	notifyURL string

	mu     sync.Mutex
	orders map[string]*orderState
}

// NewBridge 创建桥接实例。notifyURL 为本服务暴露给 StartPay 的异步通知地址，
// 必须与创建订单时传入的值完全一致，StartPay 会将其纳入验签串。
func NewBridge(cfg *Config, logger *slog.Logger) (*Bridge, error) {
	if cfg.StartPayCurrency == "" {
		cfg.StartPayCurrency = "USDT"
	}
	if cfg.StartPayChain == "" {
		cfg.StartPayChain = "TRON"
	}
	return &Bridge{
		cfg:       cfg,
		logger:    logger,
		sp:        newStartPayClient(cfg.StartPayAPIBase, cfg.StartPayAPIKey, cfg.StartPayAPISecret),
		notifyURL: cfg.BridgePublicURL + "/notify/startpay",
		orders:    make(map[string]*orderState),
	}, nil
}

// HandleSubmit 处理 new-api 前端表单提交的易支付下单请求，成功后 303 跳转
// 到 StartPay 支付页。
func (b *Bridge) HandleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "无法解析请求参数", http.StatusBadRequest)
		return
	}
	params := make(map[string]string, len(r.Form))
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}

	if err := verifyEpaySubmit(params, b.cfg.EpayKey, b.cfg.EpayPID); err != nil {
		b.logger.Warn("易支付下单验签失败", "ip", r.RemoteAddr, "error", err)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	chain, err := chainFromPayType(params["type"], b.cfg.StartPayChain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cents, err := moneyToCents(params["money"])
	if err != nil {
		http.Error(w, "金额格式非法: "+err.Error(), http.StatusBadRequest)
		return
	}
	orderAmount := centsToMoney(cents)

	notifyURL, err := validCallbackURL(params["notify_url"], "notify_url")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	returnURL, err := validCallbackURL(params["return_url"], "return_url")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tradeNo := params["out_trade_no"]
	if len(tradeNo) == 0 || len(tradeNo) > 100 {
		http.Error(w, "订单号长度非法", http.StatusBadRequest)
		return
	}

	order, err := b.sp.createOrder(r.Context(), map[string]any{
		"mchOrderNo":      tradeNo,
		"digitalCurrency": b.cfg.StartPayCurrency,
		"chain":           chain,
		"orderAmount":     orderAmount,
		"subject":         params["name"],
		"detail":          "Order " + tradeNo,
		"custId":          tradeNo,
		"custFirstName":   b.cfg.CustFirstName,
		"custLastName":    b.cfg.CustLastName,
		"notifyUrl":       b.notifyURL,
		"returnUrl":       returnURL,
	})
	if err != nil {
		b.logger.Error("创建 StartPay 订单失败", "trade_no", tradeNo, "money", orderAmount, "error", err)
		http.Error(w, "创建收款订单失败", http.StatusBadGateway)
		return
	}

	b.storeOrder(tradeNo, &orderState{
		Money:     orderAmount,
		Name:      params["name"],
		Type:      params["type"],
		NotifyURL: notifyURL,
		CreatedAt: time.Now(),
	})

	b.logger.Info("订单创建成功",
		"trade_no", tradeNo,
		"plat_order_no", order.PlatOrderNo,
		"type", params["type"],
		"money", orderAmount,
		"chain", chain,
	)
	http.Redirect(w, r, order.PayURL, http.StatusSeeOther)
}

// HandleNotify 处理 StartPay 的异步通知：验签 → 金额守卫 → 以易支付协议
// 回调 new-api 完成入账。
func (b *Bridge) HandleNotify(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, notifyBodyLimit))
	if err != nil {
		http.Error(w, "无法读取请求体", http.StatusBadRequest)
		return
	}

	timestamp := r.Header.Get("SP-TIMESTAMP")
	if timestamp == "" {
		timestamp = r.Header.Get("SP-TIMESTAM")
	}
	sign := r.Header.Get("SP-SIGN")

	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil || params == nil {
		b.logger.Warn("StartPay 通知请求体解析失败", "ip", r.RemoteAddr, "error", err)
		http.Error(w, "请求体必须是 JSON 对象", http.StatusBadRequest)
		return
	}

	if !verifyStartPayNotify(params, sign, timestamp, b.notifyURL, b.cfg.StartPayAPISecret) {
		b.logger.Warn("StartPay 通知验签失败", "ip", r.RemoteAddr, "sign", sign, "timestamp", timestamp)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	tradeNo := startPayStringField(params, "mchOrderNo")
	status := startPayStringField(params, "status")
	payStatus := startPayStringField(params, "payStatus")

	switch status {
	case "success":
		if b.markForwardedIfFirst(tradeNo) {
			b.logger.Info("重复通知幂等忽略", "trade_no", tradeNo)
			writePlain(w, http.StatusOK, "success")
			return
		}
		if !paidCoversOrder(params) {
			// 少付（less）不能按原额入账：不转发成功回调，保留订单供人工处理。
			b.logger.Warn("StartPay 到账金额低于订单金额，已拒绝自动入账",
				"trade_no", tradeNo,
				"pay_status", payStatus,
				"pay_amount", params["payAmount"],
				"order_amount", params["orderAmount"],
			)
			writePlain(w, http.StatusOK, "success")
			return
		}
		if err := b.forwardEpayCallback(r.Context(), tradeNo, params); err != nil {
			b.logger.Error("转发易支付回调失败", "trade_no", tradeNo, "error", err)
			// 返回 500 让 StartPay 稍后重推本通知。
			http.Error(w, "回调商户失败", http.StatusInternalServerError)
			return
		}
		b.logger.Info("充值回调完成", "trade_no", tradeNo, "pay_status", payStatus)
	default:
		// expired / 中间状态：new-api 侧无对应处理，仅记录。
		b.logger.Info("忽略 StartPay 通知", "trade_no", tradeNo, "status", status, "pay_status", payStatus)
	}
	writePlain(w, http.StatusOK, "success")
}

// markForwardedIfFirst 原子标记订单已转发；返回 true 表示此前已转发过
// （重复通知），本次应幂等跳过。
func (b *Bridge) markForwardedIfFirst(tradeNo string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.orders[tradeNo]
	if !ok {
		return false
	}
	if state.Forwarded {
		return true
	}
	state.Forwarded = true
	return false
}

// forwardEpayCallback 以易支付协议回调 new-api 的 notify_url。
func (b *Bridge) forwardEpayCallback(ctx context.Context, tradeNo string, notify map[string]any) error {
	state := b.takeOrder(tradeNo)
	if state == nil {
		// 重启导致状态丢失：查单核对后用兑底通知地址重建回调参数。
		var err error
		state, err = b.rebuildStateFromStartPay(ctx, tradeNo)
		if err != nil {
			return err
		}
		b.logger.Warn("订单状态缺失，已通过 StartPay 查单重建回调参数", "trade_no", tradeNo)
		// 重建的状态不回写内存（含兜底语义），仅本次使用。
	} else {
		// 回填状态，重复通知命中 markForwardedIfFirst 幂等分支。
		b.mu.Lock()
		b.orders[tradeNo] = state
		b.mu.Unlock()
	}

	platOrderNo := startPayStringField(notify, "platOrderNo")
	callbackParams, err := signEpayCallback(map[string]string{
		"pid":          b.cfg.EpayPID,
		"trade_no":     platOrderNo,
		"out_trade_no": tradeNo,
		"type":         state.Type,
		"name":         state.Name,
		"money":        state.Money,
		"trade_status": "TRADE_SUCCESS",
	}, b.cfg.EpayKey)
	if err != nil {
		return fmt.Errorf("生成回调签名失败: %w", err)
	}

	callbackURL := state.NotifyURL + "?" + encodeQuery(callbackParams)
	var lastErr error
	for attempt := 1; attempt <= epayCallbackAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		lastErr = b.pingNotify(ctx, callbackURL, tradeNo)
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", epayCallbackAttempts, lastErr)
}

// pingNotify 请求 new-api 的易支付通知地址，返回 "success" 视为成功。
func (b *Bridge) pingNotify(ctx context.Context, callbackURL, tradeNo string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callbackURL, nil)
	if err != nil {
		return err
	}
	resp, err := b.sp.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(respBody), "success") {
		return fmt.Errorf("HTTP %d body=%q", resp.StatusCode, string(respBody))
	}
	b.logger.Info("new-api 已确认入账", "trade_no", tradeNo)
	return nil
}

// rebuildStateFromStartPay 在进程状态丢失后，通过 StartPay 查单还原回调所需
// 的金额与状态信息。
func (b *Bridge) rebuildStateFromStartPay(ctx context.Context, tradeNo string) (*orderState, error) {
	if b.cfg.FallbackNotifyURL == "" {
		return nil, errors.New("订单状态缺失且未配置 NEWAPI_NOTIFY_URL，无法回调 new-api")
	}
	result, err := b.sp.queryOrder(ctx, tradeNo)
	if err != nil {
		return nil, fmt.Errorf("StartPay 查单失败: %w", err)
	}
	if result.Data.Status != "success" {
		return nil, fmt.Errorf("StartPay 订单状态为 %q，与通知不符", result.Data.Status)
	}
	return &orderState{
		Money:     centsToMoney(mustMoneyCents(result.Data.OrderAmount)),
		Name:      "TUC",
		Type:      "usdt",
		NotifyURL: b.cfg.FallbackNotifyURL,
	}, nil
}

func (b *Bridge) storeOrder(tradeNo string, state *orderState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 惰性清理过期状态。
	now := time.Now()
	for k, v := range b.orders {
		if now.Sub(v.CreatedAt) > orderStateTTL {
			delete(b.orders, k)
		}
	}
	b.orders[tradeNo] = state
}

func (b *Bridge) takeOrder(tradeNo string) *orderState {
	b.mu.Lock()
	defer b.mu.Unlock()
	state, ok := b.orders[tradeNo]
	if !ok {
		return nil
	}
	delete(b.orders, tradeNo)
	return state
}

// chainFromPayType 解析易支付 type 为 StartPay 区块链网络：
// usdt → 默认网络；usdt-tron/usdt-trx/usdt-trc20 → TRON；
// usdt-eth/usdt-erc20 → ETH；usdt-bsc/usdt-bep20 → BSC；usdt-sol/usdt-solana → SOL。
func chainFromPayType(payType string, defaultChain string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(payType)) {
	case "usdt":
		return defaultChain, nil
	case "usdt-tron", "usdt-trx", "usdt-trc20":
		return "TRON", nil
	case "usdt-eth", "usdt-erc20":
		return "ETH", nil
	case "usdt-bsc", "usdt-bep20":
		return "BSC", nil
	case "usdt-sol", "usdt-solana":
		return "SOL", nil
	default:
		return "", fmt.Errorf("不支持的支付类型 %q，本网关仅支持 usdt[-tron|trx|trc20|eth|erc20|bsc|bep20|sol|solana]", payType)
	}
}

// moneyToCents 把最多 2 位小数的金额转为美分整数（精确比较与重格式化）。
func moneyToCents(money string) (int64, error) {
	money = strings.TrimSpace(money)
	dollars, frac, found := strings.Cut(money, ".")
	if found && (len(frac) == 0 || len(frac) > 2) {
		return 0, fmt.Errorf("金额 %q 小数位不能超过 2 位", money)
	}
	if dollars == "" || len(dollars) > 12 {
		return 0, fmt.Errorf("金额 %q 非法", money)
	}
	var whole int64
	for _, ch := range dollars {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("金额 %q 非法", money)
		}
		whole = whole*10 + int64(ch-'0')
	}
	cents := whole * 100
	if found {
		switch len(frac) {
		case 1:
			if frac[0] < '0' || frac[0] > '9' {
				return 0, fmt.Errorf("金额 %q 非法", money)
			}
			cents += int64(frac[0]-'0') * 10
		case 2:
			if frac[0] < '0' || frac[0] > '9' || frac[1] < '0' || frac[1] > '9' {
				return 0, fmt.Errorf("金额 %q 非法", money)
			}
			cents += int64(frac[0]-'0')*10 + int64(frac[1]-'0')
		}
	}
	if cents <= 0 || cents > maxMoneyCents {
		return 0, fmt.Errorf("金额 %q 超出允许范围", money)
	}
	return cents, nil
}

func mustMoneyCents(money string) int64 {
	cents, err := moneyToCents(money)
	if err != nil {
		return 0
	}
	return cents
}

func centsToMoney(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// paidCoversOrder 判断实际到账是否覆盖订单金额（等额或超额）。
func paidCoversOrder(params map[string]any) bool {
	payCents, err := moneyToCents(startPayStringField(params, "payAmount"))
	if err != nil {
		return false
	}
	orderCents, err := moneyToCents(startPayStringField(params, "orderAmount"))
	if err != nil {
		return false
	}
	return payCents >= orderCents
}

// validCallbackURL 校验回调/跳转地址必须是 http(s) 绝对地址。
func validCallbackURL(raw, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s 不能为空", field)
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("%s 必须是 http(s) 绝对地址", field)
	}
	return raw, nil
}

func encodeQuery(params map[string]string) string {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values.Encode()
}

func writePlain(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message))
}
