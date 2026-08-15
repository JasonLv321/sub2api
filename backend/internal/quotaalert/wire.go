package quotaalert

import (
	"database/sql"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/wire"
)

var ProviderSet = wire.NewSet(ProvideScanner)

func ProvideScanner(
	cfg *config.Config,
	subscriptions service.UserSubscriptionRepository,
	progress *service.SubscriptionService,
	settings service.SettingRepository,
	email *service.EmailService,
	lockCache service.LeaderLockCache,
	db *sql.DB,
) (*Scanner, error) {
	alertConfig := scannerConfig(cfg.QuotaAlert)
	slog.Info(
		"quota alert configuration loaded",
		"enabled", alertConfig.Enabled,
		"interval_seconds", cfg.QuotaAlert.IntervalSec,
		"thresholds", alertConfig.Thresholds,
		"endpoints", len(cfg.QuotaAlert.Endpoints),
		"allowlist", len(cfg.QuotaAlert.Allowlist),
	)
	source := newServiceSource(subscriptions, progress)
	store := &settingDeliveryStore{repo: settings}
	leader := &serviceLeaderLock{cache: lockCache, db: db}
	sinks := []Sink{NewEmailSink(email)}
	if alertConfig.Enabled && len(cfg.QuotaAlert.Endpoints) > 0 {
		webhook, err := configuredWebhookSink(cfg.QuotaAlert)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, webhook)
	}
	scanner := NewScanner(alertConfig, source, store, leader, sinks)
	scanner.Start()
	return scanner, nil
}

func scannerConfig(cfg config.QuotaAlertConfig) Config {
	return Config{
		Enabled:     cfg.Enabled,
		Interval:    time.Duration(cfg.IntervalSec) * time.Second,
		ScanTimeout: time.Duration(cfg.ScanTimeoutSec) * time.Second,
		BatchSize:   cfg.BatchSize,
		Thresholds:  cfg.Thresholds,
	}
}

func configuredWebhookSink(
	cfg config.QuotaAlertConfig,
) (*WebhookSink, error) {
	endpoints := make([]WebhookEndpoint, 0, len(cfg.Endpoints))
	for _, endpoint := range cfg.Endpoints {
		endpoints = append(endpoints, WebhookEndpoint{
			URL: endpoint.URL, Adapter: endpoint.Adapter,
		})
	}
	return NewWebhookSink(WebhookConfig{
		Endpoints:    endpoints,
		Allowlist:    cfg.Allowlist,
		Timeout:      time.Duration(cfg.Timeout) * time.Second,
		MaxAttempts:  cfg.MaxAttempts,
		Backoff:      time.Duration(cfg.BackoffMS) * time.Millisecond,
		MaxBodyBytes: cfg.MaxRespBytes,
	}, nil)
}
