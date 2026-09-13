//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestUsageLog_RequestPayloadHashPersistence 证明 request_payload_hash 从插入到读回
// 逐字往返，缺失时为 NULL。这是 60 个位置参数里最新加的一个，列错位只有真插一行才看得出来。
func TestUsageLog_RequestPayloadHashPersistence(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{Email: "payload-hash-" + uuid.NewString() + "@example.com"})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-payload-" + uuid.NewString(), Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-payload-" + uuid.NewString()})

	hash := service.HashUsageRequestPayload([]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	require.Len(t, hash, 64)

	withHash := &service.UsageLog{
		UserID:             user.ID,
		APIKeyID:           apiKey.ID,
		AccountID:          account.ID,
		RequestID:          uuid.NewString(),
		Model:              "claude-3",
		InputTokens:        11,
		OutputTokens:       5,
		TotalCost:          1.0,
		ActualCost:         2.0,
		RequestPayloadHash: &hash,
		CreatedAt:          time.Now().UTC(),
	}
	_, err := repo.Create(ctx, withHash)
	require.NoError(t, err)
	require.NotZero(t, withHash.ID)

	withoutHash := &service.UsageLog{
		UserID:       user.ID,
		APIKeyID:     apiKey.ID,
		AccountID:    account.ID,
		RequestID:    uuid.NewString(),
		Model:        "claude-3",
		InputTokens:  7,
		OutputTokens: 3,
		TotalCost:    0.5,
		ActualCost:   0.5,
		CreatedAt:    time.Now().UTC(),
	}
	_, err = repo.Create(ctx, withoutHash)
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, withHash.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RequestPayloadHash)
	require.Equal(t, hash, *got.RequestPayloadHash)

	// 相邻列没有被挤错位：这才是位置参数出错时真正的症状。
	require.Equal(t, 11, got.InputTokens)
	require.Equal(t, 5, got.OutputTokens)
	require.Equal(t, 2.0, got.ActualCost)
	require.Equal(t, "claude-3", got.Model)
	require.False(t, got.CreatedAt.IsZero())

	gotNil, err := repo.GetByID(ctx, withoutHash.ID)
	require.NoError(t, err)
	require.Nil(t, gotNil.RequestPayloadHash)
}
