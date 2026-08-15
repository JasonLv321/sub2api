package quotaalert

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type serviceSource struct {
	subscriptions service.UserSubscriptionRepository
	progress      *service.SubscriptionService
}

func newServiceSource(
	subscriptions service.UserSubscriptionRepository,
	progress *service.SubscriptionService,
) *serviceSource {
	return &serviceSource{subscriptions: subscriptions, progress: progress}
}

func (s *serviceSource) ListActive(
	ctx context.Context,
	page int,
	pageSize int,
) ([]Subscription, int, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	subscriptions, result, err := s.subscriptions.List(
		ctx, params, nil, nil, service.SubscriptionStatusActive, "", "", "",
	)
	if err != nil {
		return nil, 0, err
	}
	items := make([]Subscription, 0, len(subscriptions))
	for i := range subscriptions {
		item := Subscription{
			ID: subscriptions[i].ID, UserID: subscriptions[i].UserID,
		}
		if subscriptions[i].User != nil {
			item.Email = subscriptions[i].User.Email
		}
		items = append(items, item)
	}
	if result == nil {
		return items, page, nil
	}
	return items, result.Pages, nil
}

func (s *serviceSource) GetProgress(
	ctx context.Context,
	subscriptionID int64,
) (*Progress, error) {
	progress, err := s.progress.GetSubscriptionProgress(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	return &Progress{
		GroupName: progress.GroupName,
		Daily:     mapWindowProgress(progress.Daily),
		Weekly:    mapWindowProgress(progress.Weekly),
		Monthly:   mapWindowProgress(progress.Monthly),
	}, nil
}

func mapWindowProgress(
	progress *service.UsageWindowProgress,
) *WindowProgress {
	if progress == nil {
		return nil
	}
	return &WindowProgress{
		LimitUSD:    progress.LimitUSD,
		UsedUSD:     progress.UsedUSD,
		Percentage:  progress.Percentage,
		WindowStart: progress.WindowStart,
		ResetsAt:    progress.ResetsAt,
	}
}

type settingDeliveryStore struct {
	repo service.SettingRepository
}

func (s *settingDeliveryStore) Exists(
	ctx context.Context,
	key string,
) (bool, error) {
	_, err := s.repo.GetValue(ctx, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, service.ErrSettingNotFound) {
		return false, nil
	}
	return false, err
}

func (s *settingDeliveryStore) Mark(ctx context.Context, key string) error {
	return s.repo.Set(ctx, key, time.Now().UTC().Format(time.RFC3339Nano))
}

type serviceLeaderLock struct {
	cache service.LeaderLockCache
	db    *sql.DB
}

func (l *serviceLeaderLock) Acquire(
	ctx context.Context,
	key string,
	owner string,
	ttl time.Duration,
) (func(), bool) {
	if l.cache != nil {
		acquired, err := l.cache.TryAcquireLeaderLock(ctx, key, owner, ttl)
		if err == nil {
			if !acquired {
				return nil, false
			}
			return l.cacheRelease(key, owner), true
		}
	}
	if l.db != nil {
		return acquireAdvisoryLock(ctx, l.db, advisoryLockID(key))
	}
	return func() {}, true
}

func (l *serviceLeaderLock) cacheRelease(key, owner string) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.cache.ReleaseLeaderLock(ctx, key, owner)
	}
}

func acquireAdvisoryLock(
	ctx context.Context,
	db *sql.DB,
	lockID int64,
) (func(), bool) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false
	}
	var acquired bool
	query := "SELECT pg_try_advisory_lock($1)"
	if err = conn.QueryRowContext(ctx, query, lockID).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false
	}
	if !acquired {
		_ = conn.Close()
		return nil, false
	}
	release := func() {
		unlockCtx, cancel := context.WithTimeout(
			context.Background(), 2*time.Second,
		)
		defer cancel()
		_, _ = conn.ExecContext(
			unlockCtx, "SELECT pg_advisory_unlock($1)", lockID,
		)
		_ = conn.Close()
	}
	return release, true
}

func advisoryLockID(key string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key))
	return int64(hash.Sum64())
}
