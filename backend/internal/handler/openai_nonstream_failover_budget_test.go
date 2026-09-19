//go:build unit

package handler

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAINonStreamFailoverBudgetExceeded(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAINonStreamResponseHeaderTimeout = 90

	require.False(t, openAINonStreamFailoverBudgetExceeded(cfg, false, time.Now().Add(-30*time.Second)), "within budget may fail over")
	require.True(t, openAINonStreamFailoverBudgetExceeded(cfg, false, time.Now().Add(-91*time.Second)), "past budget must not fail over")
	require.False(t, openAINonStreamFailoverBudgetExceeded(cfg, true, time.Now().Add(-300*time.Second)), "streaming is not bounded by the budget")

	cfg.Gateway.OpenAINonStreamResponseHeaderTimeout = 0
	require.False(t, openAINonStreamFailoverBudgetExceeded(cfg, false, time.Now().Add(-300*time.Second)), "0 disables the budget")
	require.False(t, openAINonStreamFailoverBudgetExceeded(nil, false, time.Now().Add(-300*time.Second)))
}
