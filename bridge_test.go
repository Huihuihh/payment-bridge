package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testEpayPID = "1001"
	testEpayKey = "test-epay-key"
	testSecret  = "test-startpay-secret"
)

// fakeStartPay 模拟 StartPay：记录创建的订单，提供 create/query 接口。
type fakeStartPay struct {
	t      *testing.T
	server *httptest.Server
	payURL string

	mu           sync.Mutex
	createBodies []map[string]any
	orders       map[string]string // mchOrderNo -> orderAmount
}

func newFakeStartPay(t *testing.T) *fakeStartPay {
	fake := &fakeStartPay{t: t, payURL: "https://pay.startpay.example/checkout?token=abc", orders: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/public/create_dc_order", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		// 校验桥接请求的签名：API 域名前缀 + body 字段 + 时间戳。
		timestamp := r.Header.Get("SP-TIMESTAMP")
		base := fake.server.URL + "/api"
		message := "POST" + base + "/public/create_dc_order?" + startPaySortedParams(body) + timestamp
		assert.Equal(t, startPayHMAC(message, testSecret), r.Header.Get("SP-SIGN"), "桥接对 StartPay 的请求签名不正确")
		assert.Equal(t, "sp-api-key", r.Header.Get("SP-API-KEY"))

		mchOrderNo, _ := body["mchOrderNo"].(string)
		orderAmount, _ := body["orderAmount"].(string)
		fake.mu.Lock()
		fake.createBodies = append(fake.createBodies, body)
		fake.orders[mchOrderNo] = orderAmount
		fake.mu.Unlock()

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "success",
			"data": map[string]any{
				"payUrl":      fake.payURL,
				"platOrderNo": "plat-" + mchOrderNo,
				"mchOrderNo":  mchOrderNo,
			},
		}))
	})
	mux.HandleFunc("GET /api/public/query_dc_order", func(w http.ResponseWriter, r *http.Request) {
		mchOrderNo := r.URL.Query().Get("mchOrderNo")
		timestamp := r.Header.Get("SP-TIMESTAMP")
		base := fake.server.URL + "/api"
		message := "GET" + base + "/public/query_dc_order?mchOrderNo=" + mchOrderNo + timestamp
		assert.Equal(t, startPayHMAC(message, testSecret), r.Header.Get("SP-SIGN"), "桥接查单签名不正确")

		fake.mu.Lock()
		orderAmount := fake.orders[mchOrderNo]
		fake.mu.Unlock()

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "success",
			"data": map[string]any{
				"mchOrderNo":  mchOrderNo,
				"platOrderNo": "plat-" + mchOrderNo,
				"orderAmount": orderAmount,
				"payAmount":   orderAmount,
				"payStatus":   "equal",
				"status":      "success",
			},
		}))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// notifySignature 以 StartPay 平台身份为通知生成签名。
func (f *fakeStartPay) notifySignature(notifyURL string, params map[string]any, timestamp string) string {
	return startPayHMAC(startPaySignedString(http.MethodPost, notifyURL, params, timestamp), testSecret)
}

// testEnv 组装完整桥接 + 模拟 StartPay + 模拟 new-api 回调收集端。
type testEnv struct {
	bridge       *Bridge
	bridgeURL    string
	startPay     *fakeStartPay
	notifyURL    string
	newAPINotify chan map[string]string
}

func newTestEnv(t *testing.T, fallbackNotifyURL string) *testEnv {
	t.Helper()

	env := &testEnv{newAPINotify: make(chan map[string]string, 8)}
	env.startPay = newFakeStartPay(t)

	notifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		params := map[string]string{}
		for k := range r.Form {
			params[k] = r.Form.Get(k)
		}
		env.newAPINotify <- params
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	}))
	t.Cleanup(notifyServer.Close)
	env.notifyURL = notifyServer.URL + "/api/user/epay/notify"

	cfg := &Config{
		EpayPID:           testEpayPID,
		EpayKey:           testEpayKey,
		StartPayAPIBase:   env.startPay.server.URL + "/api",
		StartPayAPIKey:    "sp-api-key",
		StartPayAPISecret: testSecret,
		StartPayChain:     "TRON",
		StartPayCurrency:  "USDT",
		CustFirstName:     "NewAPI",
		CustLastName:      "User",
		FallbackNotifyURL: fallbackNotifyURL,
	}
	bridge, err := NewBridge(cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	bridgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/notify/startpay" {
			bridge.HandleNotify(w, r)
			return
		}
		bridge.HandleSubmit(w, r)
	}))
	t.Cleanup(bridgeServer.Close)
	// 修正通知地址为本测试实例的真实地址。
	bridge.notifyURL = bridgeServer.URL + "/notify/startpay"

	env.bridge = bridge
	env.bridgeURL = bridgeServer.URL
	return env
}

func (env *testEnv) submit(t *testing.T, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	signed := epay.GenerateParams(params, testEpayKey)
	require.NotEmpty(t, signed["sign"])

	request := httptest.NewRequest(http.MethodPost, "/submit.php", strings.NewReader(mapToValues(signed).Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	env.bridge.HandleSubmit(recorder, request)
	return recorder
}

func (env *testEnv) sendNotify(t *testing.T, params map[string]any, timestampOffset time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Add(timestampOffset).Unix(), 10)
	body, err := json.Marshal(params)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, "/notify/startpay", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("SP-SIGN", env.startPay.notifySignature(env.bridge.notifyURL, params, timestamp))
	request.Header.Set("SP-TIMESTAMP", timestamp)
	recorder := httptest.NewRecorder()
	env.bridge.HandleNotify(recorder, request)
	return recorder
}

func mapToValues(params map[string]string) url.Values {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values
}

func submitParams() map[string]string {
	return map[string]string{
		"pid":          testEpayPID,
		"type":         "usdt-tron",
		"out_trade_no": "USR1NOabc123",
		"notify_url":   "https://newapi.example.com/api/user/epay/notify",
		"name":         "TUC100",
		"money":        "100.00",
		"device":       "pc",
		"return_url":   "https://newapi.example.com/usage-logs",
	}
}

func paidNotify(tradeNo string) map[string]any {
	return map[string]any{
		"mchOrderNo":   tradeNo,
		"platOrderNo":  "plat-" + tradeNo,
		"orderAmount":  "100.00",
		"payAmount":    "100.00",
		"feeAmount":    "1.00",
		"settleAmount": "99.00",
		"txId":         "tx-1",
		"payStatus":    "equal",
		"status":       "success",
		"updateTime":   "2026-05-29 12:00:00",
	}
}

func TestBridgeEndToEnd(t *testing.T) {
	env := newTestEnv(t, "")

	params := submitParams()
	params["notify_url"] = env.notifyURL

	t.Run("submit creates StartPay order and redirects", func(t *testing.T) {
		recorder := env.submit(t, params)
		require.Equal(t, http.StatusSeeOther, recorder.Code)
		assert.Equal(t, env.startPay.payURL, recorder.Header().Get("Location"))

		require.Len(t, env.startPay.createBodies, 1)
		body := env.startPay.createBodies[0]
		assert.Equal(t, "USR1NOabc123", body["mchOrderNo"])
		assert.Equal(t, "100.00", body["orderAmount"])
		assert.Equal(t, "TRON", body["chain"])
		assert.Equal(t, "USDT", body["digitalCurrency"])
		assert.Equal(t, env.bridge.notifyURL, body["notifyUrl"])
		assert.Equal(t, params["return_url"], body["returnUrl"])
	})

	t.Run("paid notify forwards TRADE_SUCCESS callback", func(t *testing.T) {
		recorder := env.sendNotify(t, paidNotify("USR1NOabc123"), 0)
		require.Equal(t, http.StatusOK, recorder.Code)
		assert.Equal(t, "success", recorder.Body.String())

		callback := <-env.newAPINotify
		assert.Equal(t, "USR1NOabc123", callback["out_trade_no"])
		assert.Equal(t, "TRADE_SUCCESS", callback["trade_status"])
		assert.Equal(t, "100.00", callback["money"])
		assert.Equal(t, "usdt-tron", callback["type"])
		assert.Equal(t, testEpayPID, callback["pid"])

		// 回调签名必须能通过 new-api 实际使用的 go-epay 客户端验签。
		client, err := epay.NewClient(&epay.Config{PartnerID: testEpayPID, Key: testEpayKey}, "https://any")
		require.NoError(t, err)
		result, err := client.Verify(callback)
		require.NoError(t, err)
		assert.True(t, result.VerifyStatus)
	})

	t.Run("duplicate notify is not forwarded twice", func(t *testing.T) {
		recorder := env.sendNotify(t, paidNotify("USR1NOabc123"), 0)
		require.Equal(t, http.StatusOK, recorder.Code)
		select {
		case callback := <-env.newAPINotify:
			t.Fatalf("重复通知不应再次回调 new-api: %v", callback)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("underpaid notify is rejected", func(t *testing.T) {
		underpaid := paidNotify("USR1NOabc123")
		underpaid["payAmount"] = "5.00"
		underpaid["payStatus"] = "less"
		recorder := env.sendNotify(t, underpaid, 0)
		require.Equal(t, http.StatusOK, recorder.Code)
		select {
		case callback := <-env.newAPINotify:
			t.Fatalf("少付通知不应回调 new-api: %v", callback)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("expired notify is ignored", func(t *testing.T) {
		expired := paidNotify("USR1NOabc123")
		expired["status"] = "expired"
		expired["payStatus"] = "unpaid"
		expired["payAmount"] = "0"
		recorder := env.sendNotify(t, expired, 0)
		require.Equal(t, http.StatusOK, recorder.Code)
		select {
		case callback := <-env.newAPINotify:
			t.Fatalf("过期通知不应回调 new-api: %v", callback)
		case <-time.After(100 * time.Millisecond):
		}
	})
}

func TestBridgeNotifySecurity(t *testing.T) {
	env := newTestEnv(t, "")

	// 先真实下单，保证订单状态存在（隔离篡改场景与重建场景）。
	params := submitParams()
	params["notify_url"] = env.notifyURL
	require.Equal(t, http.StatusSeeOther, env.submit(t, params).Code)

	t.Run("tampered body with original sign rejected", func(t *testing.T) {
		// 攻击者持有有效签名串（例如截获合法通知），篡改金额后重放：
		// 原签名必须失效。
		original := paidNotify("USR1NOabc123")
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		validSign := env.startPay.notifySignature(env.bridge.notifyURL, original, timestamp)

		tampered := paidNotify("USR1NOabc123")
		tampered["orderAmount"] = "0.01"
		body, err := json.Marshal(tampered)
		require.NoError(t, err)

		request := httptest.NewRequest(http.MethodPost, "/notify/startpay", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("SP-SIGN", validSign)
		request.Header.Set("SP-TIMESTAMP", timestamp)
		recorder := httptest.NewRecorder()
		env.bridge.HandleNotify(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("tampered trade_no rejected", func(t *testing.T) {
		original := paidNotify("USR1NOabc123")
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		validSign := env.startPay.notifySignature(env.bridge.notifyURL, original, timestamp)

		forged := paidNotify("USR2NOother456")
		body, err := json.Marshal(forged)
		require.NoError(t, err)

		request := httptest.NewRequest(http.MethodPost, "/notify/startpay", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("SP-SIGN", validSign)
		request.Header.Set("SP-TIMESTAMP", timestamp)
		recorder := httptest.NewRecorder()
		env.bridge.HandleNotify(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("stale timestamp rejected", func(t *testing.T) {
		recorder := env.sendNotify(t, paidNotify("USR1NOabc123"), -time.Hour)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("missing sign rejected", func(t *testing.T) {
		body, err := json.Marshal(paidNotify("USR1NOabc123"))
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/notify/startpay", bytes.NewReader(body))
		request.Header.Set("SP-TIMESTAMP", strconv.FormatInt(time.Now().Unix(), 10))
		recorder := httptest.NewRecorder()
		env.bridge.HandleNotify(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})
}

func TestBridgeSubmitRejects(t *testing.T) {
	env := newTestEnv(t, "")

	t.Run("bad signature", func(t *testing.T) {
		signed := epay.GenerateParams(submitParams(), testEpayKey)
		signed["money"] = "0.01"
		request := httptest.NewRequest(http.MethodPost, "/submit.php", strings.NewReader(mapToValues(signed).Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		env.bridge.HandleSubmit(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	})

	t.Run("unsupported pay type", func(t *testing.T) {
		params := submitParams()
		params["type"] = "alipay"
		recorder := env.submit(t, params)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("invalid money", func(t *testing.T) {
		params := submitParams()
		params["money"] = "100.000"
		recorder := env.submit(t, params)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})

	t.Run("non-http notify_url", func(t *testing.T) {
		params := submitParams()
		params["notify_url"] = "file:///etc/passwd"
		recorder := env.submit(t, params)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})
}

func TestChainFromPayType(t *testing.T) {
	cases := []struct {
		payType   string
		wantChain string
		wantErr   bool
	}{
		{"usdt", "TRON", false},
		{"usdt-erc20", "ETH", false},
		{"usdt-bep20", "BSC", false},
		{"usdt-solana", "SOL", false},
		{"alipay", "", true},
		{"usdt-ltc", "", true},
	}
	for _, tc := range cases {
		chain, err := chainFromPayType(tc.payType, "TRON")
		if tc.wantErr {
			require.Error(t, err, tc.payType)
			continue
		}
		require.NoError(t, err, tc.payType)
		assert.Equal(t, tc.wantChain, chain, tc.payType)
	}
}

func TestMoneyHelpers(t *testing.T) {
	cents, err := moneyToCents("100.00")
	require.NoError(t, err)
	assert.Equal(t, int64(10000), cents)
	assert.Equal(t, "100.00", centsToMoney(cents))

	// 最多 2 位小数：0-2 位均可接受，重格式化为 2 位。
	cents, err = moneyToCents("100")
	require.NoError(t, err)
	assert.Equal(t, int64(10000), cents)
	cents, err = moneyToCents("100.5")
	require.NoError(t, err)
	assert.Equal(t, int64(10050), cents)
	assert.Equal(t, "100.50", centsToMoney(cents))

	_, err = moneyToCents("100.000")
	require.Error(t, err)
	_, err = moneyToCents("0.00")
	require.Error(t, err)
	_, err = moneyToCents("-1.00")
	require.Error(t, err)
	_, err = moneyToCents("abc.00")
	require.Error(t, err)
	_, err = moneyToCents("1.0a")
	require.Error(t, err)

	assert.False(t, paidCoversOrder(map[string]any{"payAmount": "99.99", "orderAmount": "100.00"}))
	assert.True(t, paidCoversOrder(map[string]any{"payAmount": "100.00", "orderAmount": "100.00"}))
	assert.True(t, paidCoversOrder(map[string]any{"payAmount": "100.01", "orderAmount": "100.00"}))
}

// TestRebuildStateFromStartPay 模拟桥接重启后状态丢失：通知到达时通过
// StartPay 查单重建回调参数，回调指向 NEWAPI_NOTIFY_URL。
// TestServeMuxRouting 验证 BRIDGE_PUBLIC_URL 带路径前缀时（ALB/反代不重写
// 路径的场景），路由注册在前缀之下；健康检查在根路径始终可用。
func TestServeMuxRouting(t *testing.T) {
	cfg := &Config{
		EpayPID:           testEpayPID,
		EpayKey:           testEpayKey,
		StartPayAPIKey:    "sp-api-key",
		StartPayAPISecret: testSecret,
		StartPayChain:     "TRON",
		StartPayCurrency:  "USDT",
		CustFirstName:     "NewAPI",
		CustLastName:      "User",
	}
	bridge, err := NewBridge(cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	t.Run("routes under /bridge prefix", func(t *testing.T) {
		mux := newServeMux(cfg, bridge, "/bridge")
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)

		// 健康检查：前缀路径（ALB 转发路径不重写）与根路径（LB 直连后端）都可用。
		healthzStatus := func(path string) int {
			resp, err := http.Get(server.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			return resp.StatusCode
		}
		assert.Equal(t, http.StatusOK, healthzStatus("/bridge/healthz"))
		assert.Equal(t, http.StatusOK, healthzStatus("/healthz"))

		// 通知路由在前缀下：无前缀 404，带前缀非 POST 也 405。
		resp, err := http.Post(server.URL+"/notify/startpay", "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "通知路由必须挂在前缀下")
	})

	t.Run("routes at root without prefix", func(t *testing.T) {
		mux := newServeMux(cfg, bridge, "")
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)

		resp, err := http.Get(server.URL + "/healthz")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestRebuildStateFromStartPay(t *testing.T) {
	env := newTestEnv(t, "")

	// 先正常下单（notify_url 指向本地收集端），使 fake StartPay 有订单记录。
	params := submitParams()
	params["notify_url"] = env.notifyURL
	recorder := env.submit(t, params)
	require.Equal(t, http.StatusSeeOther, recorder.Code)

	// 模拟重启：清空进程内订单状态，此时 submit 时的 notify_url 已丢失。
	env.bridge.orders = map[string]*orderState{}

	// 未配置 NEWAPI_NOTIFY_URL：拒绝转发并返回 500，让 StartPay 稍后重推。
	recorder = env.sendNotify(t, paidNotify("USR1NOabc123"), 0)
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	select {
	case callback := <-env.newAPINotify:
		t.Fatalf("未配置兑底通知地址时不应回调: %v", callback)
	case <-time.After(100 * time.Millisecond):
	}

	// 配置 NEWAPI_NOTIFY_URL 后：查单重建成功并完成回调。
	env.bridge.cfg.FallbackNotifyURL = env.notifyURL
	recorder = env.sendNotify(t, paidNotify("USR1NOabc123"), 0)
	assert.Equal(t, http.StatusOK, recorder.Code)

	callback := <-env.newAPINotify
	assert.Equal(t, "USR1NOabc123", callback["out_trade_no"])
	assert.Equal(t, "TRADE_SUCCESS", callback["trade_status"])
	assert.Equal(t, "100.00", callback["money"])
}
