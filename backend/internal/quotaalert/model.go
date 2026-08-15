package quotaalert

import (
	"context"
	"time"
)

const (
	WindowDaily   = "daily"
	WindowWeekly  = "weekly"
	WindowMonthly = "monthly"
)

type Config struct {
	Enabled     bool
	Interval    time.Duration
	ScanTimeout time.Duration
	BatchSize   int
	Thresholds  []float64
}

type Subscription struct {
	ID     int64
	UserID int64
	Email  string
}

type Progress struct {
	GroupName string
	Daily     *WindowProgress
	Weekly    *WindowProgress
	Monthly   *WindowProgress
}

type WindowProgress struct {
	LimitUSD    float64
	UsedUSD     float64
	Percentage  float64
	WindowStart time.Time
	ResetsAt    time.Time
}

type Event struct {
	SubscriptionID int64     `json:"subscription_id"`
	UserID         int64     `json:"user_id"`
	Recipient      string    `json:"recipient,omitempty"`
	GroupName      string    `json:"group_name"`
	Window         string    `json:"window"`
	WindowStart    time.Time `json:"window_start"`
	ResetsAt       time.Time `json:"resets_at"`
	Threshold      float64   `json:"threshold"`
	Percentage     float64   `json:"percentage"`
	LimitUSD       float64   `json:"limit_usd"`
	UsedUSD        float64   `json:"used_usd"`
}

type Source interface {
	ListActive(
		ctx context.Context,
		page int,
		pageSize int,
	) ([]Subscription, int, error)
	GetProgress(ctx context.Context, subscriptionID int64) (*Progress, error)
}

type DeliveryStore interface {
	Exists(ctx context.Context, key string) (bool, error)
	Mark(ctx context.Context, key string) error
}

type Sink interface {
	Name() string
	Deliver(ctx context.Context, event Event) error
}

type LeaderLock interface {
	Acquire(
		ctx context.Context,
		key string,
		owner string,
		ttl time.Duration,
	) (func(), bool)
}
