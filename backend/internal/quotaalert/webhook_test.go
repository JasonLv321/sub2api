package quotaalert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookPayloadAdapters(t *testing.T) {
	event := Event{GroupName: "Internal", Window: WindowMonthly}
	tests := []struct {
		adapter string
		key     string
	}{
		{AdapterGeneric, "subscription_id"},
		{AdapterFeishu, "msg_type"},
		{AdapterWeCom, "msgtype"},
	}
	for _, test := range tests {
		t.Run(test.adapter, func(t *testing.T) {
			payload, err := webhookPayload(test.adapter, event)
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if _, ok := decoded[test.key]; !ok {
				t.Fatalf("payload missing %q", test.key)
			}
		})
	}
}

func TestWebhookSinkRejectsURLOutsideAllowlist(t *testing.T) {
	sink, err := NewWebhookSink(WebhookConfig{
		Endpoints: []WebhookEndpoint{{
			URL: "https://hooks.example.test/secret", Adapter: AdapterGeneric,
		}},
		Allowlist: []string{"https://allowed.example.test/hook"},
	}, nil)
	if err == nil || sink != nil {
		t.Fatal("expected non-allowlisted URL to be rejected")
	}
}

func TestWebhookSinkRetriesBoundedly(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte("upstream failure must stay bounded"))
	}))
	defer server.Close()

	sink, err := NewWebhookSink(WebhookConfig{
		Endpoints: []WebhookEndpoint{{
			URL:     server.URL + "/hook?token=do-not-log",
			Adapter: AdapterGeneric,
		}},
		Allowlist:    []string{server.URL + "/hook?token=do-not-log"},
		Timeout:      100 * time.Millisecond,
		MaxAttempts:  3,
		Backoff:      time.Millisecond,
		MaxBodyBytes: 32,
	}, server.Client())
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	err = sink.Deliver(context.Background(), Event{SubscriptionID: 1})
	if err == nil {
		t.Fatal("expected delivery failure")
	}
	if strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("delivery error leaked webhook token: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestWebhookSinkTimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		time.Sleep(50 * time.Millisecond)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sink, err := NewWebhookSink(WebhookConfig{
		Endpoints: []WebhookEndpoint{{
			URL:     server.URL + "/hook?token=timeout-secret",
			Adapter: AdapterWeCom,
		}},
		Allowlist:   []string{server.URL + "/hook?token=timeout-secret"},
		Timeout:     5 * time.Millisecond,
		MaxAttempts: 2,
		Backoff:     time.Millisecond,
	}, server.Client())
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	started := time.Now()
	deliveryErr := sink.Deliver(context.Background(), Event{})
	if deliveryErr == nil {
		t.Fatal("expected timeout failure")
	}
	if strings.Contains(deliveryErr.Error(), "timeout-secret") {
		t.Fatalf("timeout error leaked webhook token: %v", deliveryErr)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("bounded timeout took %v", elapsed)
	}
}
