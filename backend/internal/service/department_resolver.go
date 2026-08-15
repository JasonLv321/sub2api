package service

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	UnknownDepartmentCode   = "unknown"
	departmentAttributeKey  = "department"
	departmentCacheTTL      = 5 * time.Minute
	departmentFallbackTTL   = 15 * time.Second
	departmentLookupTimeout = 100 * time.Millisecond
)

// DepartmentResolver returns a stable department code without surfacing errors.
type DepartmentResolver interface {
	Resolve(ctx context.Context, userID int64) string
}

type departmentAttributeReader interface {
	GetDefinitionByKey(
		ctx context.Context,
		key string,
	) (*UserAttributeDefinition, error)
	GetUserAttributes(
		ctx context.Context,
		userID int64,
	) ([]UserAttributeValue, error)
}

type departmentCacheEntry struct {
	code      string
	expiresAt time.Time
}

type cachedDepartmentResolver struct {
	reader   departmentAttributeReader
	ttl      time.Duration
	fallback time.Duration
	timeout  time.Duration
	now      func() time.Time

	mu    sync.RWMutex
	cache map[int64]departmentCacheEntry
	group singleflight.Group
}

var (
	departmentCacheHitTotal  atomic.Int64
	departmentCacheMissTotal atomic.Int64
	departmentLoadTotal      atomic.Int64
	departmentErrorTotal     atomic.Int64
)

// GatewayDepartmentResolverStats exposes cache and lookup counters.
func GatewayDepartmentResolverStats() (
	hit, miss, load, lookupError int64,
) {
	return departmentCacheHitTotal.Load(),
		departmentCacheMissTotal.Load(),
		departmentLoadTotal.Load(),
		departmentErrorTotal.Load()
}

// NewDepartmentResolver creates the shared short-TTL department resolver.
func NewDepartmentResolver(
	reader *UserAttributeService,
) DepartmentResolver {
	return newDepartmentResolver(
		reader,
		departmentCacheTTL,
		departmentFallbackTTL,
		departmentLookupTimeout,
	)
}

func newDepartmentResolver(
	reader departmentAttributeReader,
	ttl time.Duration,
	fallback time.Duration,
	timeout time.Duration,
) *cachedDepartmentResolver {
	return &cachedDepartmentResolver{
		reader:   reader,
		ttl:      ttl,
		fallback: fallback,
		timeout:  timeout,
		now:      time.Now,
		cache:    make(map[int64]departmentCacheEntry),
	}
}

func (r *cachedDepartmentResolver) Resolve(
	ctx context.Context,
	userID int64,
) string {
	if r == nil || r.reader == nil || userID <= 0 {
		return UnknownDepartmentCode
	}
	if code, ok := r.cached(userID); ok {
		departmentCacheHitTotal.Add(1)
		return code
	}

	departmentCacheMissTotal.Add(1)
	value, _, _ := r.group.Do(strconv.FormatInt(userID, 10), func() (any, error) {
		if code, ok := r.cached(userID); ok {
			return code, nil
		}
		return r.load(ctx, userID), nil
	})
	code, _ := value.(string)
	return normalizeDepartmentCode(code)
}

func (r *cachedDepartmentResolver) cached(userID int64) (string, bool) {
	r.mu.RLock()
	entry, ok := r.cache[userID]
	r.mu.RUnlock()
	if !ok || !r.now().Before(entry.expiresAt) {
		return "", false
	}
	return entry.code, true
}

func (r *cachedDepartmentResolver) load(
	ctx context.Context,
	userID int64,
) string {
	departmentLoadTotal.Add(1)
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	lookupCtx, cancel := context.WithTimeout(base, r.timeout)
	defer cancel()

	code, err := r.lookup(lookupCtx, userID)
	ttl := r.ttl
	if err != nil {
		departmentErrorTotal.Add(1)
		code = UnknownDepartmentCode
		ttl = r.fallback
	}
	r.store(userID, code, ttl)
	return code
}

func (r *cachedDepartmentResolver) lookup(
	ctx context.Context,
	userID int64,
) (string, error) {
	definition, err := r.reader.GetDefinitionByKey(
		ctx,
		departmentAttributeKey,
	)
	if err != nil || definition == nil {
		return UnknownDepartmentCode, err
	}
	values, err := r.reader.GetUserAttributes(ctx, userID)
	if err != nil {
		return UnknownDepartmentCode, err
	}
	for _, value := range values {
		if value.AttributeID == definition.ID {
			return normalizeDepartmentCode(value.Value), nil
		}
	}
	return UnknownDepartmentCode, nil
}

func (r *cachedDepartmentResolver) store(
	userID int64,
	code string,
	ttl time.Duration,
) {
	r.mu.Lock()
	r.cache[userID] = departmentCacheEntry{
		code:      normalizeDepartmentCode(code),
		expiresAt: r.now().Add(ttl),
	}
	r.mu.Unlock()
}

func normalizeDepartmentCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return UnknownDepartmentCode
	}
	return code
}
