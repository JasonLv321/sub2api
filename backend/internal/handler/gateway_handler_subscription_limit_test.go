package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBillingErrorDetails_SubscriptionLimitsReturnTooManyRequests(
	t *testing.T,
) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "daily", err: service.ErrDailyLimitExceeded},
		{name: "weekly", err: service.ErrWeeklyLimitExceeded},
		{name: "monthly", err: service.ErrMonthlyLimitExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code, message, retryAfter := billingErrorDetails(tt.err)
			require.Equal(t, http.StatusTooManyRequests, status)
			require.Equal(t, "rate_limit_exceeded", code)
			require.NotEmpty(t, message)
			require.Equal(t, 60, retryAfter)
		})
	}
}
