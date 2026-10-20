package main

import (
	"testing"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEpaySignCompatibleWithGoEpay 用 go-epay 官方库做兼容性基准：
// 本实现生成的签名必须与 go-epay 逐字节一致，且能验证 go-epay 生成的签名。
func TestEpaySignCompatibleWithGoEpay(t *testing.T) {
	params := map[string]string{
		"pid":          "1001",
		"type":         "usdt-tron",
		"out_trade_no": "USR1NOabc123",
		"notify_url":   "https://newapi.example.com/api/user/epay/notify",
		"name":         "TUC100",
		"money":        "100.00",
		"device":       "pc",
		"return_url":   "https://newapi.example.com/usage-logs",
	}

	mine, err := epaySign(params, "test-key")
	require.NoError(t, err)

	// go-epay 基准：MD5(排序参数串 + key)。
	filtered := epay.ParamsFilter(params)
	keys, values := epay.ParamsSort(filtered)
	canonical, err := epay.CreateUrlStringWithError(keys, values)
	require.NoError(t, err)
	assert.Equal(t, epay.MD5String(canonical, "test-key"), mine)

	// go-epay 生成的完整签名参数必须通过本实现的验签。
	signed := epay.GenerateParams(cloneParams(params), "test-key")
	require.NotEmpty(t, signed["sign"])
	require.NoError(t, verifyEpaySubmit(signed, "test-key", "1001"))
}

// TestVerifyEpaySubmitRejects 覆盖拒绝路径：篡改金额、错 key、错 pid、
// 非法参数名、参数值含 &。
func TestVerifyEpaySubmitRejects(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"pid":          "1001",
			"type":         "usdt",
			"out_trade_no": "USR1NOabc123",
			"notify_url":   "https://newapi.example.com/api/user/epay/notify",
			"name":         "TUC100",
			"money":        "100.00",
			"sign_type":    "MD5",
		}
	}
	sign := func(t *testing.T, params map[string]string, key string) string {
		t.Helper()
		signer, err := epaySign(params, key)
		require.NoError(t, err)
		return signer
	}

	t.Run("tampered money", func(t *testing.T) {
		params := base()
		params["sign"] = sign(t, params, "test-key")
		params["money"] = "0.01"
		require.ErrorIs(t, verifyEpaySubmit(params, "test-key", "1001"), errEpayBadSignature)
	})

	t.Run("wrong key", func(t *testing.T) {
		params := base()
		params["sign"] = sign(t, params, "other-key")
		require.ErrorIs(t, verifyEpaySubmit(params, "test-key", "1001"), errEpayBadSignature)
	})

	t.Run("wrong pid", func(t *testing.T) {
		params := base()
		params["sign"] = sign(t, params, "test-key")
		require.Error(t, verifyEpaySubmit(params, "test-key", "2002"))
	})

	t.Run("missing sign", func(t *testing.T) {
		params := base()
		require.ErrorIs(t, verifyEpaySubmit(params, "test-key", "1001"), errEpayBadSignature)
	})

	t.Run("ambiguous parameter name", func(t *testing.T) {
		params := base()
		params["out_trade_no=other"] = "x"
		params["sign"] = "whatever"
		_, err := epaySign(params, "test-key")
		require.Error(t, err)
	})

	t.Run("value contains ampersand", func(t *testing.T) {
		params := base()
		params["name"] = "TUC&out_trade_no=other"
		params["sign"] = "whatever"
		_, err := epaySign(params, "test-key")
		require.Error(t, err)
	})

	t.Run("empty values excluded from signing", func(t *testing.T) {
		params := base()
		params["device"] = ""
		params["sign"] = sign(t, params, "test-key")
		// 与 go-epay 的 ParamsFilter 一致：空值不参与签名。
		reference := epay.GenerateParams(cloneParams(params), "test-key")
		assert.Equal(t, reference["sign"], params["sign"])
		require.NoError(t, verifyEpaySubmit(params, "test-key", "1001"))
	})
}

// TestSignEpayCallback 覆盖桥接 → new-api 方向：签名的回调必须能被 go-epay
// 客户端 Verify 通过（即 new-api 真实使用的验证代码）。
func TestSignEpayCallback(t *testing.T) {
	signed, err := signEpayCallback(map[string]string{
		"pid":          "1001",
		"trade_no":     "plat-1",
		"out_trade_no": "USR1NOabc123",
		"type":         "usdt-tron",
		"name":         "TUC100",
		"money":        "100.00",
		"trade_status": "TRADE_SUCCESS",
	}, "test-key")
	require.NoError(t, err)

	client, err := epay.NewClient(&epay.Config{PartnerID: "1001", Key: "test-key"}, "https://newapi.example.com")
	require.NoError(t, err)
	result, err := client.Verify(signed)
	require.NoError(t, err)
	assert.True(t, result.VerifyStatus)
	assert.Equal(t, "USR1NOabc123", result.ServiceTradeNo)
	assert.Equal(t, "TRADE_SUCCESS", result.TradeStatus)
	assert.Equal(t, "100.00", result.Money)

	// 篡改金额后原签名必须失效。
	tampered := cloneParams(signed)
	tampered["money"] = "999.00"
	tamperedResult, err := client.Verify(tampered)
	require.NoError(t, err)
	assert.False(t, tamperedResult.VerifyStatus)
}

func cloneParams(params map[string]string) map[string]string {
	cloned := make(map[string]string, len(params))
	for k, v := range params {
		cloned[k] = v
	}
	return cloned
}
