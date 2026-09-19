package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type usageCostCalibrationRepository struct {
	sql sqlExecutor
}

// NewUsageCostCalibrationRepository 创建看板成本校准仓储（仅 PostgreSQL）。
func NewUsageCostCalibrationRepository(sqlDB *sql.DB) service.UsageCostCalibrationRepository {
	if sqlDB == nil || !isPostgresDriver(sqlDB) {
		return nil
	}
	return &usageCostCalibrationRepository{sql: sqlDB}
}

func (r *usageCostCalibrationRepository) DailyAccountCost(ctx context.Context, accountIDs []int64, since time.Time) (map[string]service.UsageCostDay, error) {
	// 成本口径与看板一致：COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)
	query := `
		SELECT
			to_char((created_at AT TIME ZONE $3)::date, 'YYYY-MM-DD') AS day,
			COUNT(*),
			COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0)
		FROM usage_logs
		WHERE account_id = ANY($1) AND created_at >= $2
		GROUP BY 1
	`
	rows, err := r.sql.QueryContext(ctx, query, pq.Array(accountIDs), since, timezone.Name())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]service.UsageCostDay)
	for rows.Next() {
		var day string
		var item service.UsageCostDay
		if err := rows.Scan(&day, &item.Requests, &item.AccountCost); err != nil {
			return nil, err
		}
		out[day] = item
	}
	return out, rows.Err()
}

func (r *usageCostCalibrationRepository) UpsertCalibrations(ctx context.Context, rows []service.UsageCostCalibration) error {
	query := `
		INSERT INTO usage_cost_calibrations (
			bucket_date, key_hash, account_ids, our_requests, upstream_requests,
			our_account_cost, upstream_actual_cost, adjustment, status, computed_at
		) VALUES ($1::date, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (bucket_date, key_hash) DO UPDATE SET
			account_ids = EXCLUDED.account_ids,
			our_requests = EXCLUDED.our_requests,
			upstream_requests = EXCLUDED.upstream_requests,
			our_account_cost = EXCLUDED.our_account_cost,
			upstream_actual_cost = EXCLUDED.upstream_actual_cost,
			adjustment = EXCLUDED.adjustment,
			status = EXCLUDED.status,
			computed_at = EXCLUDED.computed_at
	`
	for _, row := range rows {
		if _, err := r.sql.ExecContext(ctx, query,
			row.BucketDate, row.KeyHash, service.FormatAccountIDs(row.AccountIDs), row.OurRequests, row.UpstreamRequests,
			row.OurAccountCost, row.UpstreamActualCost, row.Adjustment, row.Status,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *usageCostCalibrationRepository) MismatchedAggregateDays(ctx context.Context, since, before time.Time) ([]time.Time, error) {
	// 与 usage_dashboard_daily 的聚合口径逐项对比：请求数、实际、成本。
	query := `
		WITH raw AS (
			SELECT
				(created_at AT TIME ZONE $3)::date AS d,
				COUNT(*) AS n,
				COALESCE(SUM(actual_cost), 0) AS actual,
				COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0) AS cost
			FROM usage_logs
			WHERE created_at >= $1 AND created_at < $2
			GROUP BY 1
		),
		agg AS (
			SELECT bucket_date AS d, total_requests AS n, actual_cost AS actual, account_cost AS cost
			FROM usage_dashboard_daily
			WHERE bucket_date >= ($1 AT TIME ZONE $3)::date AND bucket_date < ($2 AT TIME ZONE $3)::date
		)
		SELECT to_char(COALESCE(raw.d, agg.d), 'YYYY-MM-DD')
		FROM raw FULL JOIN agg ON raw.d = agg.d
		WHERE COALESCE(raw.n, 0) <> COALESCE(agg.n, 0)
			OR ABS(COALESCE(raw.actual, 0) - COALESCE(agg.actual, 0)) > 0.000001
			OR ABS(COALESCE(raw.cost, 0) - COALESCE(agg.cost, 0)) > 0.000001
		ORDER BY 1
	`
	rows, err := r.sql.QueryContext(ctx, query, since, before, timezone.Name())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var days []time.Time
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		t, err := time.ParseInLocation("2006-01-02", day, timezone.Location())
		if err != nil {
			return nil, err
		}
		days = append(days, t)
	}
	return days, rows.Err()
}
