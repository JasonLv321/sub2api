package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type paymentRouteSettingRepo struct {
	values map[string]string
	err    error
}

func (r *paymentRouteSettingRepo) Get(
	context.Context,
	string,
) (*service.Setting, error) {
	panic("unexpected Get call")
}

func (r *paymentRouteSettingRepo) GetValue(
	context.Context,
	string,
) (string, error) {
	panic("unexpected GetValue call")
}

func (r *paymentRouteSettingRepo) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (r *paymentRouteSettingRepo) GetMultiple(
	_ context.Context,
	keys []string,
) (map[string]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *paymentRouteSettingRepo) SetMultiple(
	context.Context,
	map[string]string,
) error {
	panic("unexpected SetMultiple call")
}

func (r *paymentRouteSettingRepo) GetAll(
	context.Context,
) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (r *paymentRouteSettingRepo) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func TestRegisterRoutes_PaymentRoutesFollowSetting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		values     map[string]string
		err        error
		registered bool
	}{
		{
			name:       "enabled",
			values:     map[string]string{"payment_enabled": "true"},
			registered: true,
		},
		{
			name:   "disabled",
			values: map[string]string{"payment_enabled": "false"},
		},
		{name: "read failure", err: errors.New("database unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			settings := service.NewSettingService(
				&paymentRouteSettingRepo{values: tt.values, err: tt.err},
				&config.Config{},
			)
			handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
			pass := func(c *gin.Context) { c.Next() }

			registerRoutes(
				router,
				handlers,
				middleware2.JWTAuthMiddleware(pass),
				middleware2.OptionalJWTAuthMiddleware(pass),
				middleware2.AdminAuthMiddleware(pass),
				middleware2.APIKeyAuthMiddleware(pass),
				middleware2.AuditLogMiddleware(pass),
				middleware2.StepUpAuthMiddleware(pass),
				nil,
				nil,
				nil,
				settings,
				nil,
				&config.Config{},
				nil,
			)

			registered := false
			for _, route := range router.Routes() {
				if route.Method == http.MethodGet &&
					route.Path == "/api/v1/payment/config" {
					registered = true
					break
				}
			}
			require.Equal(t, tt.registered, registered)

			if !tt.registered {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(
					http.MethodGet,
					"/api/v1/payment/config",
					nil,
				)
				router.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusNotFound, recorder.Code)
			}
		})
	}
}
