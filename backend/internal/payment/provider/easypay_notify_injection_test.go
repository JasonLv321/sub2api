package provider

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// 2026-09-14 生产事故回归测试。
//
// 攻击手法：easyPaySign 把参数的**原始值**按 k=v&k=v 拼起来签名，而 VerifyNotification
// 用 url.ParseQuery 解码并按 & 切分。两边对"参数边界"的理解不一致，于是攻击者只要能
// 让某个可控值里带上 &，就能把额外的参数夹带进一串**由我们自己的密钥签好名**的数据里，
// 再把它回放给 notify 端点。当时唯一可控的值是 return_url（由客户端请求体传入）。
//
// 实际后果：用户 313 凭空充值 ¥1011（订单 109/110/115），审计日志里 tradeNo 全为空。

func newInjectionTestEasyPay(t *testing.T) *EasyPay {
	t.Helper()
	provider, err := NewEasyPay("test-instance", map[string]string{
		"pid":         "1102",
		"pkey":        "0123456789abcdef0123456789abcdef",
		"apiBase":     "https://pay.example.com",
		"notifyUrl":   "https://shop.example.com/api/v1/payment/webhook/easypay",
		"returnUrl":   "https://shop.example.com/payment/result",
		"paymentMode": paymentModePopup,
	})
	if err != nil {
		t.Fatalf("NewEasyPay: %v", err)
	}
	return provider
}

// signingStringFromPayURL 还原出我方生成 sign 时实际拼出来的那串待签名数据
// —— 也就是攻击者能从自己的 pay_url 里直接读到并拿去回放的东西。
func signingStringFromPayURL(t *testing.T, payURL string) string {
	t.Helper()
	q, err := url.Parse(payURL)
	if err != nil {
		t.Fatalf("parse payURL: %v", err)
	}
	params := map[string]string{}
	for k, v := range q.Query() {
		params[k] = v[0]
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || k == "sign_type" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	// easyPaySign 内部是排序后拼接，这里复刻同样的顺序。
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&") + "&sign=" + params["sign"] + "&sign_type=" + params["sign_type"]
}

// 端到端复刻：把我方自己签好名的那串原样回放给 notify 端点，必须被拒。
func TestEasyPayVerifyNotificationRejectsReplayedSigningString(t *testing.T) {
	t.Parallel()

	provider := newInjectionTestEasyPay(t)
	resp, err := provider.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:     "sub2_20260914JbRJw2os",
		Amount:      "1000.00",
		PaymentType: "alipay",
		Subject:     "Sub2API 1000.00 CNY",
		ReturnURL:   "https://shop.example.com/payment/result?&trade_status=TRADE_SUCCESS",
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	replay := signingStringFromPayURL(t, resp.PayURL)
	notification, err := provider.VerifyNotification(context.Background(), replay, nil)
	if err == nil && notification != nil && notification.Status == payment.ProviderStatusSuccess {
		t.Fatalf("回放我方自签串被当成了成功付款（金额 %v，单号 %s）—— 这正是 2026-09-14 凭空充值 ¥1011 的路径",
			notification.Amount, notification.OrderID)
	}
}

// 结构性兜底：成功回调必须带上游流水号。历史 92 笔已完成订单无一例外，
// 而三笔伪造订单的 trade_no 全是空的。
func TestEasyPayVerifyNotificationRequiresTradeNoOnSuccess(t *testing.T) {
	t.Parallel()

	provider := newInjectionTestEasyPay(t)
	pkey := "0123456789abcdef0123456789abcdef"

	build := func(params map[string]string) string {
		params["sign"] = easyPaySign(params, pkey)
		params["sign_type"] = signTypeMD5
		values := url.Values{}
		for k, v := range params {
			values.Set(k, v)
		}
		return values.Encode()
	}

	// 签名合法但缺 trade_no —— 拒。
	missing := build(map[string]string{
		"pid":          "1102",
		"type":         "alipay",
		"out_trade_no": "sub2_20260914JbRJw2os",
		"name":         "Sub2API 1000.00 CNY",
		"money":        "1000.00",
		"trade_status": tradeStatusSuccess,
	})
	if _, err := provider.VerifyNotification(context.Background(), missing, nil); err == nil {
		t.Fatal("缺少 trade_no 的成功回调应当被拒绝")
	}

	// 正常回调带 trade_no —— 必须继续放行，别把真实付款误杀了。
	genuine := build(map[string]string{
		"pid":          "1102",
		"type":         "alipay",
		"out_trade_no": "sub2_20260914WfhUyO1U",
		"name":         "Sub2API 1.00 CNY",
		"money":        "1.00",
		"trade_no":     "2026091404544256567",
		"trade_status": tradeStatusSuccess,
	})
	notification, err := provider.VerifyNotification(context.Background(), genuine, nil)
	if err != nil {
		t.Fatalf("真实回调被误杀: %v", err)
	}
	if notification.Status != payment.ProviderStatusSuccess {
		t.Fatalf("真实回调状态应为成功，实际 %q", notification.Status)
	}
	if notification.TradeNo != "2026091404544256567" {
		t.Fatalf("上游流水号丢失: %q", notification.TradeNo)
	}
}
