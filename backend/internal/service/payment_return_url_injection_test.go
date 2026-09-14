//go:build unit

package service

import (
	"strings"
	"testing"
)

// 2026-09-14 生产事故回归测试：客户端传入的 return_url 若保留 query，
// 就会成为易支付签名串里唯一攻击者可控的值，可用来夹带 &trade_status=TRADE_SUCCESS。
func TestCanonicalizeReturnURLDropsCallerQuery(t *testing.T) {
	t.Parallel()

	const host = "znbcode.com"
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "生产上实际打进来的注入载荷",
			raw:  "https://znbcode.com/payment/result?&trade_status=TRADE_SUCCESS",
		},
		{
			name: "夹带多个参数",
			raw:  "https://znbcode.com/payment/result?money=0.01&trade_status=TRADE_SUCCESS&trade_no=x",
		},
		{
			name: "看似无害的 query 同样丢弃",
			raw:  "https://znbcode.com/payment/result?from=mobile",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalizeReturnURL(tc.raw, host, "")
			if err != nil {
				t.Fatalf("CanonicalizeReturnURL(%q): %v", tc.raw, err)
			}
			if strings.ContainsAny(got, "?&") {
				t.Fatalf("返回值仍带 query，可用于签名串注入: %q", got)
			}
			if got != "https://znbcode.com/payment/result" {
				t.Fatalf("期望 %q，实际 %q", "https://znbcode.com/payment/result", got)
			}
		})
	}
}

// 清 query 不能影响正常流程：buildPaymentReturnURL 仍要补齐全部回跳参数。
func TestBuildPaymentReturnURLStillCarriesOwnParams(t *testing.T) {
	t.Parallel()

	canonical, err := CanonicalizeReturnURL(
		"https://znbcode.com/payment/result?&trade_status=TRADE_SUCCESS", "znbcode.com", "")
	if err != nil {
		t.Fatalf("CanonicalizeReturnURL: %v", err)
	}

	got, err := buildPaymentReturnURL(canonical, 110, "sub2_20260914JbRJw2os", "resume-token-abc")
	if err != nil {
		t.Fatalf("buildPaymentReturnURL: %v", err)
	}
	for _, want := range []string{
		"order_id=110",
		"out_trade_no=sub2_20260914JbRJw2os",
		"resume_token=resume-token-abc",
		"status=success",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("回跳地址缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "trade_status") {
		t.Fatalf("注入的 trade_status 存活到了最终回跳地址: %s", got)
	}
}
