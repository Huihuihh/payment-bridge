package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StartPay 数字货币收单 API 客户端（商户侧），签名规则与官方 SDK 一致：
// 待签串 = {HTTP方法}{API地址}?{参数按名升序 k=v&...}{秒级时间戳}，
// HMAC-SHA1 + Base64，放入 SP-SIGN 头。空值字段不参与签名与请求体。

const (
	startPayCreateOrderPath = "/public/create_dc_order"
	startPayQueryOrderPath  = "/public/query_dc_order"
	// 通知时间戳有效期：对齐 StartPay 自身的 30 分钟签名有效期，过期视为重放。
	startPayTimestampTolerance = 30 * time.Minute
	startPayMaxResponseBytes   = 1 << 20
)

type startPayClient struct {
	apiBase   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

func newStartPayClient(apiBase, apiKey, apiSecret string) *startPayClient {
	return &startPayClient{
		apiBase:   strings.TrimRight(apiBase, "/"),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

type startPayOrder struct {
	PayURL      string
	PlatOrderNo string
	MchOrderNo  string
}

type startPayCreateResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		PayURL      string `json:"payUrl"`
		PlatOrderNo string `json:"platOrderNo"`
		MchOrderNo  string `json:"mchOrderNo"`
	} `json:"data"`
}

type startPayQueryResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		MchOrderNo  string `json:"mchOrderNo"`
		PlatOrderNo string `json:"platOrderNo"`
		OrderAmount string `json:"orderAmount"`
		PayAmount   string `json:"payAmount"`
		PayStatus   string `json:"payStatus"`
		Status      string `json:"status"`
	} `json:"data"`
}

// startPaySignedString 构造 StartPay 待签名字符串。
func startPaySignedString(method, requestURL string, params map[string]any, timestamp string) string {
	return method + requestURL + "?" + startPaySortedParams(params) + timestamp
}

// startPaySortedParams 按 key 字典序拼接 k=v&...，值用 %v 格式化（与官方 SDK 一致）。
func startPaySortedParams(params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		fmt.Fprintf(&sb, "%v", params[k])
	}
	return sb.String()
}

func startPayHMAC(message, secret string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// startPayHeaderTimestamp 归一化通知头时间戳为秒级：13 位按毫秒处理，其余按秒。
func startPayHeaderTimestamp(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	if len(raw) >= 13 {
		value /= 1000
	}
	return value, true
}

// verifyStartPayNotify 校验 StartPay 异步通知：签名 + 时间戳新鲜度。
// notifyURL 必须与创建订单时传入的 notifyUrl 完全一致。
func verifyStartPayNotify(params map[string]any, sign, timestamp, notifyURL, secret string) bool {
	if secret == "" || strings.TrimSpace(sign) == "" {
		return false
	}
	notifyTime, ok := startPayHeaderTimestamp(timestamp)
	if !ok {
		return false
	}
	if drift := time.Since(time.Unix(notifyTime, 0)); drift > startPayTimestampTolerance || drift < -startPayTimestampTolerance {
		return false
	}
	message := startPaySignedString(http.MethodPost, notifyURL, params, strings.TrimSpace(timestamp))
	expected := startPayHMAC(message, secret)
	return hmac.Equal([]byte(expected), []byte(sign))
}

func startPayStringField(params map[string]any, key string) string {
	if value, ok := params[key].(string); ok {
		return value
	}
	return ""
}

// do 发送已签名的 StartPay API 请求。签名串统一为：
// {method}{apiBase}{path}?{params升序}{时间戳}；GET 请求把 params 编码为
// 实际 query（签名串使用原始值，与官方 SDK 一致）。
func (c *startPayClient) do(ctx context.Context, method, path string, params map[string]any, body []byte) ([]byte, error) {
	signURL := c.apiBase + path
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sign := startPayHMAC(startPaySignedString(method, signURL, params, timestamp), c.apiSecret)

	requestURL := signURL
	if method == http.MethodGet && len(params) > 0 {
		values := url.Values{}
		for k, v := range params {
			values.Set(k, fmt.Sprintf("%v", v))
		}
		requestURL += "?" + values.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("SP-API-KEY", c.apiKey)
	req.Header.Set("SP-SIGN", sign)
	req.Header.Set("SP-TIMESTAMP", timestamp)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 StartPay 失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, startPayMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 StartPay 响应失败: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("StartPay HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// createOrder 创建数字货币收单订单，返回支付链接。空值字段不参与签名，
// 因此请求体中也不允许出现空字符串值。
func (c *startPayClient) createOrder(ctx context.Context, params map[string]any) (*startPayOrder, error) {
	for k, v := range params {
		if s, ok := v.(string); ok && s == "" {
			return nil, fmt.Errorf("StartPay 请求字段 %s 不能为空", k)
		}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	respBody, err := c.do(ctx, http.MethodPost, startPayCreateOrderPath, params, body)
	if err != nil {
		return nil, err
	}

	var result startPayCreateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析 StartPay 响应失败: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("StartPay 错误 %d: %s", result.Code, result.Msg)
	}
	if result.Data.PayURL == "" {
		return nil, errors.New("StartPay 未返回支付链接")
	}
	return &startPayOrder{
		PayURL:      result.Data.PayURL,
		PlatOrderNo: result.Data.PlatOrderNo,
		MchOrderNo:  result.Data.MchOrderNo,
	}, nil
}

// queryOrder 主动查单（通知丢失或重启后状态还原的兑底）。
func (c *startPayClient) queryOrder(ctx context.Context, mchOrderNo string) (*startPayQueryResponse, error) {
	params := map[string]any{"mchOrderNo": mchOrderNo}

	respBody, err := c.do(ctx, http.MethodGet, startPayQueryOrderPath, params, nil)
	if err != nil {
		return nil, err
	}

	var result startPayQueryResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析 StartPay 响应失败: %w", err)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("StartPay 错误 %d: %s", result.Code, result.Msg)
	}
	return &result, nil
}
