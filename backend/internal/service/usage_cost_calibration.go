package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

// 仪表盘成本校准：usage_logs 只记录我方完成的请求，而上游对被我方放弃的尝试
// （响应头超时后换号、客户端断开）照常扣费，看板成本因此系统性偏低。
// 对提供按 key 逐日账本（Sub2API 兼容的 GET /v1/usage）的上游，每小时把
// 「上游实扣 − 我方看板成本」按 (日期, 上游 key) 写入 usage_cost_calibrations，
// 看板在 usage_logs 成本之上叠加该差额。无账本的上游保持原公式。
// 同一轮还核对最近几天的预聚合表与 usage_logs，不一致则触发重算，保证「实际」正确。

const (
	usageCostCalibrationInterval       = time.Hour
	usageCostCalibrationStartupDelay   = 2 * time.Minute
	usageCostCalibrationRunTimeout     = 10 * time.Minute
	usageCostCalibrationRequestTimeout = 20 * time.Second
	usageCostCalibrationMaxBodyBytes   = 1 << 20
	usageCostCalibrationLeaderLockKey  = "usage_cost_calibration:leader"
	usageCostCalibrationLeaderLockTTL  = usageCostCalibrationRunTimeout + time.Minute
	// 预聚合自检回看天数（不含今天：今天的桶还在增量聚合中）。
	usageCostCalibrationAggregateCheckDays = 7

	UsageCostCalibrationStatusOK         = "ok"
	UsageCostCalibrationStatusMisaligned = "misaligned"
)

// UsageCostDay 是我方某天某组账号的请求数与看板成本。
type UsageCostDay struct {
	Requests    int64
	AccountCost float64
}

// UsageCostCalibration 是一行校准结果。
type UsageCostCalibration struct {
	BucketDate         string // YYYY-MM-DD（应用时区）
	KeyHash            string
	AccountIDs         []int64
	OurRequests        int64
	UpstreamRequests   int64
	OurAccountCost     float64
	UpstreamActualCost float64
	Adjustment         float64
	Status             string
}

// UsageCostCalibrationRepository 定义校准所需的存储操作。
type UsageCostCalibrationRepository interface {
	// DailyAccountCost 按应用时区逐日汇总这些账号自 since 以来的请求数与看板成本。
	DailyAccountCost(ctx context.Context, accountIDs []int64, since time.Time) (map[string]UsageCostDay, error)
	UpsertCalibrations(ctx context.Context, rows []UsageCostCalibration) error
	// MismatchedAggregateDays 返回 [since, before) 内预聚合日表与 usage_logs 不一致的日期（应用时区零点）。
	MismatchedAggregateDays(ctx context.Context, since, before time.Time) ([]time.Time, error)
}

type usageCostCalibrationAggregator interface {
	TriggerRecomputeRange(start, end time.Time) error
}

// upstreamDailyUsage 是上游 /v1/usage 的 daily_usage 条目。
type upstreamDailyUsage struct {
	Date       string  `json:"date"`
	Requests   int64   `json:"requests"`
	ActualCost float64 `json:"actual_cost"`
}

// UsageCostCalibrationService 周期性地用上游账本校准看板成本。
type UsageCostCalibrationService struct {
	accountRepo        AccountRepository
	repo               UsageCostCalibrationRepository
	accountTestService *AccountTestService
	aggregator         usageCostCalibrationAggregator

	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
	stopped bool
}

func NewUsageCostCalibrationService(
	accountRepo AccountRepository,
	repo UsageCostCalibrationRepository,
	accountTestService *AccountTestService,
	aggregator *DashboardAggregationService,
) *UsageCostCalibrationService {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &UsageCostCalibrationService{
		accountRepo:        accountRepo,
		repo:               repo,
		accountTestService: accountTestService,
		instanceID:         uuid.NewString(),
		ctx:                ctx,
		cancel:             cancel,
	}
	// 避免把 nil *DashboardAggregationService 装进接口造成非 nil 判断失效。
	if aggregator != nil {
		svc.aggregator = aggregator
	}
	return svc
}

// ProvideUsageCostCalibrationService 创建并启动校准服务。
func ProvideUsageCostCalibrationService(
	accountRepo AccountRepository,
	repo UsageCostCalibrationRepository,
	accountTestService *AccountTestService,
	aggregator *DashboardAggregationService,
	lockCache LeaderLockCache,
	db *sql.DB,
) *UsageCostCalibrationService {
	svc := NewUsageCostCalibrationService(accountRepo, repo, accountTestService, aggregator)
	svc.lockCache = lockCache
	svc.db = db
	svc.Start()
	return svc
}

func (s *UsageCostCalibrationService) Start() {
	if s == nil || s.repo == nil || s.accountRepo == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.wg.Add(1)
	go s.runLoop()
}

func (s *UsageCostCalibrationService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *UsageCostCalibrationService) runLoop() {
	defer s.wg.Done()
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(usageCostCalibrationStartupDelay):
	}
	ticker := time.NewTicker(usageCostCalibrationInterval)
	defer ticker.Stop()
	for {
		s.runOnce()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *UsageCostCalibrationService) runOnce() {
	ctx, cancel := context.WithTimeout(s.ctx, usageCostCalibrationRunTimeout)
	defer cancel()
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, usageCostCalibrationLeaderLockKey, s.instanceID, usageCostCalibrationLeaderLockTTL)
	if !ok {
		return
	}
	defer release()

	if err := s.CalibrateCosts(ctx); err != nil {
		logger.LegacyPrintf("service.usage_cost_calibration", "calibrate_costs_failed: err=%v", err)
	}
	s.checkAggregates(ctx)
}

// checkAggregates 核对最近已结束日子的预聚合表，不一致则触发重算。
func (s *UsageCostCalibrationService) checkAggregates(ctx context.Context) {
	if s.aggregator == nil {
		return
	}
	today := timezone.Today()
	days, err := s.repo.MismatchedAggregateDays(ctx, today.AddDate(0, 0, -usageCostCalibrationAggregateCheckDays), today)
	if err != nil {
		logger.LegacyPrintf("service.usage_cost_calibration", "aggregate_check_failed: err=%v", err)
		return
	}
	if len(days) == 0 {
		return
	}
	start, end := days[0], days[len(days)-1].AddDate(0, 0, 1)
	logger.LegacyPrintf("service.usage_cost_calibration", "aggregate_mismatch: days=%d start=%s end=%s, triggering recompute",
		len(days), start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err := s.aggregator.TriggerRecomputeRange(start, end); err != nil {
		logger.LegacyPrintf("service.usage_cost_calibration", "aggregate_recompute_failed: err=%v", err)
	}
}

type usageCostCalibrationKeyGroup struct {
	keyHash    string
	accountIDs []int64
	account    *Account // 用于发请求的代表账号（ID 最小）
}

// CalibrateCosts 对每把有上游账本的 key 写入逐日校准行。
func (s *UsageCostCalibrationService) CalibrateCosts(ctx context.Context) error {
	// 取全部未删除账号（不限状态）：停用号与在用号共用 key 时，上游账本也含停用号的消费。
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, "", "", "", "", 0, "")
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	for _, group := range groupAccountsByUpstreamKey(accounts) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ledger, err := s.fetchUpstreamLedger(ctx, group.account)
		if err != nil || len(ledger) == 0 {
			continue // 无账本（404、非 Sub2API 上游、网络失败）：保持原公式
		}
		since, ok := earliestLedgerDay(ledger)
		if !ok {
			continue
		}
		ours, err := s.repo.DailyAccountCost(ctx, group.accountIDs, since)
		if err != nil {
			return fmt.Errorf("daily account cost: %w", err)
		}
		rows := buildUsageCostCalibrations(group.keyHash, group.accountIDs, ledger, ours)
		if len(rows) == 0 {
			continue
		}
		if err := s.repo.UpsertCalibrations(ctx, rows); err != nil {
			return fmt.Errorf("upsert calibrations: %w", err)
		}
	}
	return nil
}

// groupAccountsByUpstreamKey 把共用同一把上游 key 的 API Key 账号归为一组：
// 上游账本按 key 记，必须拿这一组的合计来比。官方 API 与无自定义 base_url 的账号跳过。
func groupAccountsByUpstreamKey(accounts []Account) []usageCostCalibrationKeyGroup {
	byKey := map[string]*usageCostCalibrationKeyGroup{}
	for i := range accounts {
		account := &accounts[i]
		if account.Type != AccountTypeAPIKey {
			continue
		}
		apiKey := account.GetCredential("api_key")
		baseURL := account.GetCredential("base_url")
		if apiKey == "" || baseURL == "" || upstreamBillingProbeTargetIsOfficialAPI(baseURL) {
			continue
		}
		sum := sha256.Sum256([]byte(apiKey))
		keyHash := hex.EncodeToString(sum[:])
		group, ok := byKey[keyHash]
		if !ok {
			group = &usageCostCalibrationKeyGroup{keyHash: keyHash, account: account}
			byKey[keyHash] = group
		}
		group.accountIDs = append(group.accountIDs, account.ID)
		if account.ID < group.account.ID {
			group.account = account
		}
	}
	groups := make([]usageCostCalibrationKeyGroup, 0, len(byKey))
	for _, group := range byKey {
		sort.Slice(group.accountIDs, func(i, j int) bool { return group.accountIDs[i] < group.accountIDs[j] })
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].accountIDs[0] < groups[j].accountIDs[0] })
	return groups
}

func earliestLedgerDay(ledger []upstreamDailyUsage) (time.Time, bool) {
	var earliest time.Time
	for _, day := range ledger {
		t, err := time.ParseInLocation("2006-01-02", day.Date, timezone.Location())
		if err != nil {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest, !earliest.IsZero()
}

// buildUsageCostCalibrations 逐日计算差额。规则：
//   - 上游当天没有请求记录的日子不校准（账本可能尚未覆盖，不能把成本清零）；
//   - 上游请求数明显少于我方（< 90% 且差 > 5 笔）视为不可比，记 misaligned、差额 0；
//   - 其余日子差额 = 上游实扣 − 我方看板成本（上游多扣的是我方未记账的尝试）。
func buildUsageCostCalibrations(keyHash string, accountIDs []int64, ledger []upstreamDailyUsage, ours map[string]UsageCostDay) []UsageCostCalibration {
	rows := make([]UsageCostCalibration, 0, len(ledger))
	for _, day := range ledger {
		if day.Requests <= 0 {
			continue
		}
		if _, err := time.Parse("2006-01-02", day.Date); err != nil {
			continue
		}
		our := ours[day.Date]
		row := UsageCostCalibration{
			BucketDate:         day.Date,
			KeyHash:            keyHash,
			AccountIDs:         accountIDs,
			OurRequests:        our.Requests,
			UpstreamRequests:   day.Requests,
			OurAccountCost:     our.AccountCost,
			UpstreamActualCost: day.ActualCost,
			Status:             UsageCostCalibrationStatusOK,
		}
		if float64(day.Requests) < 0.9*float64(our.Requests) && our.Requests-day.Requests > 5 {
			row.Status = UsageCostCalibrationStatusMisaligned
		} else {
			row.Adjustment = day.ActualCost - our.AccountCost
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *UsageCostCalibrationService) fetchUpstreamLedger(ctx context.Context, account *Account) ([]upstreamDailyUsage, error) {
	if s.accountTestService == nil || s.accountTestService.httpUpstream == nil {
		return nil, fmt.Errorf("transport unavailable")
	}
	baseURL, err := s.accountTestService.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.ProxyID != nil {
		if account.Proxy == nil {
			return nil, fmt.Errorf("proxy unavailable")
		}
		proxyURL = account.Proxy.URL()
	}
	reqCtx, cancel := context.WithTimeout(ctx, usageCostCalibrationRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, buildOpenAIEndpointURL(baseURL, "/v1/usage"), nil)
	if err != nil {
		return nil, err
	}
	profile := HTTPUpstreamProfileDefault
	if account.Platform == PlatformOpenAI {
		profile = HTTPUpstreamProfileOpenAI
	}
	req = req.WithContext(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(req.Context(), profile)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+account.GetCredential("api_key"))
	account.ApplyHeaderOverrides(req.Header)
	var tlsProfile *tlsfingerprint.Profile
	if s.accountTestService.tlsFPProfileService != nil {
		tlsProfile = s.accountTestService.tlsFPProfileService.ResolveTLSProfile(account)
	}
	resp, err := s.accountTestService.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageCostCalibrationMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	return parseUpstreamDailyUsage(body)
}

func parseUpstreamDailyUsage(body []byte) ([]upstreamDailyUsage, error) {
	var payload struct {
		DailyUsage []upstreamDailyUsage `json:"daily_usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload.DailyUsage, nil
}

// FormatAccountIDs 把账号 ID 列表格式化为逗号分隔字符串（存储用）。
func FormatAccountIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
