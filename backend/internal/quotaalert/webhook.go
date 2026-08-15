package quotaalert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	AdapterGeneric = "generic"
	AdapterFeishu  = "feishu"
	AdapterWeCom   = "wecom"
)

type WebhookEndpoint struct {
	URL     string
	Adapter string
}

type WebhookConfig struct {
	Endpoints    []WebhookEndpoint
	Allowlist    []string
	Timeout      time.Duration
	MaxAttempts  int
	Backoff      time.Duration
	MaxBodyBytes int64
}

type WebhookSink struct {
	cfg    WebhookConfig
	client *http.Client
}

func NewWebhookSink(
	cfg WebhookConfig,
	client *http.Client,
) (*WebhookSink, error) {
	cfg = normalizeWebhookConfig(cfg)
	allowed := make(map[string]struct{}, len(cfg.Allowlist))
	for _, rawURL := range cfg.Allowlist {
		normalized, err := normalizeWebhookURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid webhook allowlist entry: %w", err)
		}
		allowed[normalized] = struct{}{}
	}
	for i := range cfg.Endpoints {
		normalized, err := normalizeWebhookURL(cfg.Endpoints[i].URL)
		if err != nil {
			return nil, fmt.Errorf("invalid webhook endpoint: %w", err)
		}
		if _, ok := allowed[normalized]; !ok {
			return nil, errors.New("webhook endpoint is not allowlisted")
		}
		cfg.Endpoints[i].URL = normalized
		if !validAdapter(cfg.Endpoints[i].Adapter) {
			return nil, errors.New("unsupported webhook adapter")
		}
	}
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.Timeout = cfg.Timeout
	copied.CheckRedirect = func(
		request *http.Request,
		_ []*http.Request,
	) error {
		normalized, err := normalizeWebhookURL(request.URL.String())
		if err != nil {
			return err
		}
		if _, ok := allowed[normalized]; !ok {
			return errors.New("webhook redirect is not allowlisted")
		}
		return nil
	}
	return &WebhookSink{cfg: cfg, client: &copied}, nil
}

func (s *WebhookSink) Name() string {
	return "webhook"
}

func (s *WebhookSink) Deliver(ctx context.Context, event Event) error {
	if s == nil {
		return nil
	}
	for _, endpoint := range s.cfg.Endpoints {
		payload, err := webhookPayload(endpoint.Adapter, event)
		if err != nil {
			return err
		}
		if err := s.deliverEndpoint(ctx, endpoint.URL, payload); err != nil {
			return err
		}
	}
	return nil
}

func (s *WebhookSink) deliverEndpoint(
	ctx context.Context,
	endpoint string,
	payload []byte,
) error {
	var lastErr error
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		lastErr = s.attempt(ctx, endpoint, payload)
		if lastErr == nil {
			return nil
		}
		if attempt < s.cfg.MaxAttempts {
			delay := s.cfg.Backoff * time.Duration(attempt)
			if err := waitForRetry(ctx, delay); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf(
		"webhook delivery failed after %d attempts: %w",
		s.cfg.MaxAttempts,
		lastErr,
	)
}

func (s *WebhookSink) attempt(
	ctx context.Context,
	endpoint string,
	payload []byte,
) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(payload),
	)
	if err != nil {
		return errors.New("build webhook request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("webhook request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(
		response.Body, s.cfg.MaxBodyBytes+1,
	))
	if err != nil {
		return fmt.Errorf("read webhook response: %w", err)
	}
	if int64(len(body)) > s.cfg.MaxBodyBytes {
		return errors.New("webhook response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", response.StatusCode)
	}
	return nil
}

func webhookPayload(adapter string, event Event) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case "", AdapterGeneric:
		return json.Marshal(event)
	case AdapterFeishu:
		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": eventText(event)},
		})
	case AdapterWeCom:
		return json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": eventText(event)},
		})
	default:
		return nil, errors.New("unsupported webhook adapter")
	}
}

func eventText(event Event) string {
	return fmt.Sprintf(
		"Quota alert: %s %s usage %.2f%% reached %.0f%%",
		event.GroupName,
		event.Window,
		event.Percentage,
		event.Threshold,
	)
}

func normalizeWebhookConfig(cfg WebhookConfig) WebhookConfig {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxAttempts <= 0 || cfg.MaxAttempts > 5 {
		cfg.MaxAttempts = 3
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 200 * time.Millisecond
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 4096
	}
	return cfg
}

func normalizeWebhookURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("URL must be absolute")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("URL scheme must be http or https")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validAdapter(adapter string) bool {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case "", AdapterGeneric, AdapterFeishu, AdapterWeCom:
		return true
	default:
		return false
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
