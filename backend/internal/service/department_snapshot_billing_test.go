package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type departmentStaticResolver struct {
	code string
}

func (r *departmentStaticResolver) Resolve(context.Context, int64) string {
	return r.code
}

type departmentUsageRepoStub struct {
	UsageLogRepository
	created int
	lastLog *UsageLog
}

func (r *departmentUsageRepoStub) Create(
	_ context.Context,
	log *UsageLog,
) (bool, error) {
	r.created++
	r.lastLog = log
	return true, nil
}

type departmentUserRepoStub struct {
	UserRepository
	deducted int
}

func (r *departmentUserRepoStub) DeductBalance(
	context.Context,
	int64,
	float64,
) error {
	r.deducted++
	return nil
}

type departmentBillingRepoStub struct {
	UsageBillingRepository
	applied int
}

func (r *departmentBillingRepoStub) Apply(
	context.Context,
	*UsageBillingCommand,
) (*UsageBillingApplyResult, error) {
	r.applied++
	return &UsageBillingApplyResult{Applied: true}, nil
}

func departmentBillingParams(cost float64) *postUsageBillingParams {
	return &postUsageBillingParams{
		Cost:    &CostBreakdown{ActualCost: cost},
		User:    &User{ID: 42},
		APIKey:  &APIKey{ID: 51},
		Account: &Account{ID: 61},
	}
}

func TestApplyUsageBillingLegacyWritesDepartmentSnapshot(t *testing.T) {
	usageLog := &UsageLog{RequestID: "legacy-department"}
	userRepo := &departmentUserRepoStub{}
	usageRepo := &departmentUsageRepoStub{}
	deps := &billingDeps{
		userRepo:           userRepo,
		departmentResolver: &departmentStaticResolver{code: "finance"},
	}

	applied, err := applyUsageBilling(
		context.Background(),
		usageLog.RequestID,
		usageLog,
		departmentBillingParams(1),
		deps,
		nil,
	)
	require.NoError(t, err)
	require.True(t, applied)
	writeUsageLogBestEffort(
		context.Background(),
		usageRepo,
		usageLog,
		"department.snapshot.legacy",
	)
	require.Equal(t, 1, userRepo.deducted)
	require.Equal(t, 1, usageRepo.created)
	require.Equal(t, "finance", usageLog.DepartmentCode)
}

func TestApplyUsageBillingAtomicWritesDepartmentSnapshot(t *testing.T) {
	usageLog := &UsageLog{RequestID: "atomic-department"}
	billingRepo := &departmentBillingRepoStub{}
	usageRepo := &departmentUsageRepoStub{}
	deps := &billingDeps{
		billingCacheService: &BillingCacheService{},
		deferredService:     &DeferredService{},
		departmentResolver:  &departmentStaticResolver{code: "engineering"},
	}

	applied, err := applyUsageBilling(
		context.Background(),
		usageLog.RequestID,
		usageLog,
		departmentBillingParams(0),
		deps,
		billingRepo,
	)
	require.NoError(t, err)
	require.True(t, applied)
	writeUsageLogBestEffort(
		context.Background(),
		usageRepo,
		usageLog,
		"department.snapshot.atomic",
	)
	require.Equal(t, 1, billingRepo.applied)
	require.Equal(t, 1, usageRepo.created)
	require.Equal(t, "engineering", usageLog.DepartmentCode)
}

func TestDepartmentResolverErrorDoesNotBlockBillingOrUsageWrite(t *testing.T) {
	reader := &departmentReaderStub{
		definitionError: errors.New("database unavailable"),
	}
	resolver := newDepartmentResolver(
		reader,
		time.Minute,
		time.Second,
		time.Second,
	)
	usageLog := &UsageLog{RequestID: "resolver-error"}
	userRepo := &departmentUserRepoStub{}
	usageRepo := &departmentUsageRepoStub{}
	deps := &billingDeps{
		userRepo:           userRepo,
		departmentResolver: resolver,
	}

	applied, err := applyUsageBilling(
		context.Background(),
		usageLog.RequestID,
		usageLog,
		departmentBillingParams(1),
		deps,
		nil,
	)
	require.NoError(t, err)
	require.True(t, applied)
	writeUsageLogBestEffort(
		context.Background(),
		usageRepo,
		usageLog,
		"department.snapshot.test",
	)

	require.Equal(t, 1, userRepo.deducted)
	require.Equal(t, 1, usageRepo.created)
	require.Equal(t, UnknownDepartmentCode, usageRepo.lastLog.DepartmentCode)
}
