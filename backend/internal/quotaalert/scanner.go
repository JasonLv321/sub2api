package quotaalert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	leaderLockKey  = "quota_alert:scanner:leader"
	deliveryPrefix = "quota_alert_delivery:v2:"
)

type Scanner struct {
	cfg      Config
	source   Source
	store    DeliveryStore
	lock     LeaderLock
	sinks    []Sink
	owner    string
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewScanner(
	cfg Config,
	source Source,
	store DeliveryStore,
	lock LeaderLock,
	sinks []Sink,
) *Scanner {
	cfg = normalizeConfig(cfg)
	return &Scanner{
		cfg: cfg, source: source, store: store, lock: lock,
		sinks: sinks, owner: uuid.NewString(), stopCh: make(chan struct{}),
	}
}

func (s *Scanner) Start() {
	if s == nil || !s.cfg.Enabled || s.source == nil || s.store == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.cfg.Interval)
		defer ticker.Stop()
		s.runScheduledScan()
		for {
			select {
			case <-ticker.C:
				s.runScheduledScan()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *Scanner) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *Scanner) runScheduledScan() {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ScanTimeout)
	defer cancel()
	if err := s.scanOnce(ctx); err != nil {
		slog.Error("quota alert scan failed", "error", err)
	}
}

func (s *Scanner) scanOnce(ctx context.Context) error {
	release, acquired := func() (func(), bool) {
		if s.lock == nil {
			return func() {}, true
		}
		return s.lock.Acquire(
			ctx, leaderLockKey, s.owner, s.cfg.ScanTimeout+s.cfg.Interval,
		)
	}()
	if !acquired {
		return nil
	}
	defer release()

	for page := 1; ; page++ {
		subscriptions, pages, err := s.source.ListActive(
			ctx, page, s.cfg.BatchSize,
		)
		if err != nil {
			return fmt.Errorf("list active subscriptions page %d: %w", page, err)
		}
		for i := range subscriptions {
			if err := s.scanSubscription(ctx, subscriptions[i]); err != nil {
				slog.Warn(
					"quota alert subscription scan failed",
					"subscription_id", subscriptions[i].ID,
					"error", err,
				)
			}
		}
		if page >= pages || len(subscriptions) == 0 {
			return nil
		}
	}
}

func (s *Scanner) scanSubscription(
	ctx context.Context,
	subscription Subscription,
) error {
	progress, err := s.source.GetProgress(ctx, subscription.ID)
	if err != nil {
		return fmt.Errorf("get progress: %w", err)
	}
	if progress == nil {
		return errors.New("progress is nil")
	}
	windows := []struct {
		name     string
		progress *WindowProgress
	}{
		{WindowDaily, progress.Daily},
		{WindowWeekly, progress.Weekly},
		{WindowMonthly, progress.Monthly},
	}
	for _, window := range windows {
		if window.progress == nil || window.progress.WindowStart.IsZero() {
			continue
		}
		if err := s.scanWindow(
			ctx, subscription, progress.GroupName,
			window.name, window.progress,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) scanWindow(
	ctx context.Context,
	subscription Subscription,
	groupName string,
	windowName string,
	progress *WindowProgress,
) error {
	for _, threshold := range s.cfg.Thresholds {
		if progress.Percentage < threshold {
			continue
		}
		event := Event{
			SubscriptionID: subscription.ID,
			UserID:         subscription.UserID,
			Recipient:      subscription.Email,
			GroupName:      groupName,
			Window:         windowName,
			WindowStart:    progress.WindowStart,
			ResetsAt:       progress.ResetsAt,
			Threshold:      threshold,
			Percentage:     progress.Percentage,
			LimitUSD:       progress.LimitUSD,
			UsedUSD:        progress.UsedUSD,
		}
		if err := s.deliverOnce(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) deliverOnce(ctx context.Context, event Event) error {
	for _, sink := range s.sinks {
		if sink == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(sink.Name()))
		key := deliveryKey(name, event)
		exists, err := s.store.Exists(ctx, key)
		if err != nil {
			logSinkFailure(name, "check delivery", err)
			continue
		}
		if exists {
			continue
		}
		if err := sink.Deliver(ctx, event); err != nil {
			logSinkFailure(name, "deliver quota alert", err)
			continue
		}
		if err := s.store.Mark(ctx, key); err != nil {
			logSinkFailure(name, "mark delivery", err)
		}
	}
	return nil
}

func logSinkFailure(name, operation string, err error) {
	slog.Warn(
		"quota alert sink delivery failed",
		"sink", name,
		"operation", operation,
		"error", err,
	)
}

func deliveryKey(sinkName string, event Event) string {
	identity := strings.Join([]string{
		sinkName,
		strconv.FormatInt(event.SubscriptionID, 10),
		event.WindowStart.UTC().Format(time.RFC3339Nano),
		strconv.FormatFloat(event.Threshold, 'f', -1, 64),
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return deliveryPrefix + sinkName + ":" + hex.EncodeToString(sum[:])
}

func normalizeConfig(cfg Config) Config {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.ScanTimeout <= 0 {
		cfg.ScanTimeout = 2 * time.Minute
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 1000 {
		cfg.BatchSize = 200
	}
	thresholds := make([]float64, 0, len(cfg.Thresholds))
	for _, threshold := range cfg.Thresholds {
		if threshold > 0 && threshold <= 100 {
			thresholds = append(thresholds, threshold)
		}
	}
	if len(thresholds) == 0 {
		thresholds = []float64{80, 100}
	}
	sort.Float64s(thresholds)
	cfg.Thresholds = thresholds
	return cfg
}
