//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildUsageCostCalibrations(t *testing.T) {
	ledger := []upstreamDailyUsage{
		{Date: "2026-09-14", Requests: 1223, ActualCost: 118.854}, // 上游多扣：被放弃的尝试
		{Date: "2026-09-15", Requests: 0, ActualCost: 0},          // 上游当天无记录：不能把成本清零
		{Date: "2026-09-16", Requests: 8145, ActualCost: 65.3},    // 请求数明显少于我方：不可比
		{Date: "2026-09-17", Requests: 2282, ActualCost: 72.4052}, // 我方略多 2 笔：仍可比
		{Date: "bad-date", Requests: 5, ActualCost: 1},            // 非法日期跳过
		{Date: "2026-09-18", Requests: 3, ActualCost: 0.02},       // 我方当天无用量
	}
	ours := map[string]UsageCostDay{
		"2026-09-14": {Requests: 951, AccountCost: 99.6471},
		"2026-09-15": {Requests: 322, AccountCost: 26.3574},
		"2026-09-16": {Requests: 13938, AccountCost: 109.39},
		"2026-09-17": {Requests: 2284, AccountCost: 71.7386},
	}
	rows := buildUsageCostCalibrations("h", []int64{33}, ledger, ours)
	require.Len(t, rows, 4)

	require.Equal(t, "2026-09-14", rows[0].BucketDate)
	require.Equal(t, UsageCostCalibrationStatusOK, rows[0].Status)
	require.InDelta(t, 118.854-99.6471, rows[0].Adjustment, 1e-9)

	require.Equal(t, "2026-09-16", rows[1].BucketDate)
	require.Equal(t, UsageCostCalibrationStatusMisaligned, rows[1].Status)
	require.Zero(t, rows[1].Adjustment)

	require.Equal(t, "2026-09-17", rows[2].BucketDate)
	require.Equal(t, UsageCostCalibrationStatusOK, rows[2].Status)
	require.InDelta(t, 72.4052-71.7386, rows[2].Adjustment, 1e-9)

	require.Equal(t, "2026-09-18", rows[3].BucketDate)
	require.InDelta(t, 0.02, rows[3].Adjustment, 1e-9)
}

func TestGroupAccountsByUpstreamKey(t *testing.T) {
	accounts := []Account{
		{ID: 27, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-shared", "base_url": "https://up.example"}},
		{ID: 24, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-shared", "base_url": "https://up.example"}},
		{ID: 38, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-own", "base_url": "https://other.example"}},
		{ID: 1, Type: AccountTypeOAuth, Credentials: map[string]any{"api_key": "sk-x", "base_url": "https://up.example"}},
		{ID: 2, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-y", "base_url": "https://api.openai.com"}},
		{ID: 3, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-z"}},
	}
	groups := groupAccountsByUpstreamKey(accounts)
	require.Len(t, groups, 2)
	require.Equal(t, []int64{24, 27}, groups[0].accountIDs)
	require.Equal(t, int64(24), groups[0].account.ID)
	require.Len(t, groups[0].keyHash, 64)
	require.NotContains(t, groups[0].keyHash, "sk-shared")
	require.Equal(t, []int64{38}, groups[1].accountIDs)
}

func TestParseUpstreamDailyUsage(t *testing.T) {
	days, err := parseUpstreamDailyUsage([]byte(`{"balance":1,"daily_usage":[{"date":"2026-09-18","requests":57,"cost":3.4,"actual_cost":0.51}]}`))
	require.NoError(t, err)
	require.Equal(t, []upstreamDailyUsage{{Date: "2026-09-18", Requests: 57, ActualCost: 0.51}}, days)

	days, err = parseUpstreamDailyUsage([]byte(`{"balance":1}`))
	require.NoError(t, err)
	require.Empty(t, days)
}
