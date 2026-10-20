package main

import (
	"crypto/md5"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 易支付协议（服务端）实现，与 new-api 所用的 github.com/Calcium-Ion/go-epay
// 客户端保持字节级兼容：
//
//   - 签名串：过滤 sign/sign_type/空值 → 按参数名升序 → k=v&... 拼接
//   - 签名：MD5(签名串 + 商户密钥)，hex 小写
//   - 参数名不得包含 &/=，参数值不得包含 &（go-epay 的防参数歧义校验）
//   - 下单入口 /submit.php，成功回调 trade_status=TRADE_SUCCESS

var (
	errEpayInvalidParams = errors.New("epay: invalid signing parameters")
	errEpayBadSignature  = errors.New("epay: signature mismatch")
)

// epaySign 复刻 go-epay 的 generateSign：对待签参数生成加盐 MD5。
func epaySign(params map[string]string, key string) (string, error) {
	if params == nil {
		return "", fmt.Errorf("%w: nil parameter map", errEpayInvalidParams)
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || k == "sign_type" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var urlString strings.Builder
	for i, k := range keys {
		if k == "" || strings.ContainsAny(k, "&=") {
			return "", fmt.Errorf("%w: invalid parameter name %q", errEpayInvalidParams, k)
		}
		if strings.Contains(params[k], "&") {
			return "", fmt.Errorf("%w: parameter %q contains '&' in its value", errEpayInvalidParams, k)
		}
		if i > 0 {
			urlString.WriteByte('&')
		}
		urlString.WriteString(k)
		urlString.WriteByte('=')
		urlString.WriteString(params[k])
	}
	return epayMD5(urlString.String(), key), nil
}

func epayMD5(urlString string, key string) string {
	digest := md5.Sum([]byte(urlString + key))
	return fmt.Sprintf("%x", digest)
}

// verifyEpaySubmit 校验 new-api（或任何持钥方）提交的 /submit.php 参数。
func verifyEpaySubmit(params map[string]string, key string, pid string) error {
	if params["pid"] != pid {
		return fmt.Errorf("pid 不匹配: %q", params["pid"])
	}
	if params["sign_type"] != "MD5" {
		return fmt.Errorf("sign_type 仅支持 MD5: %q", params["sign_type"])
	}
	expected, err := epaySign(params, key)
	if err != nil {
		return err
	}
	provided := params["sign"]
	if provided == "" {
		return errEpayBadSignature
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return errEpayBadSignature
	}
	return nil
}

// signEpayCallback 为转发给 new-api 的回调参数生成签名，返回带 sign/sign_type
// 的完整参数集。
func signEpayCallback(params map[string]string, key string) (map[string]string, error) {
	signed := make(map[string]string, len(params)+2)
	for k, v := range params {
		signed[k] = v
	}
	sign, err := epaySign(signed, key)
	if err != nil {
		return nil, err
	}
	signed["sign"] = sign
	signed["sign_type"] = "MD5"
	return signed, nil
}
