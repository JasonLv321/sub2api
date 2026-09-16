//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestForwardAsAnthropic_TransportError_ReturnsFailoverError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{
		err: errors.New(`dial tcp 1.2.3.4:443: connect: connection refused`),
	}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	account := rawChatCompletionsTestAccount()
	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "transport error should return UpstreamFailoverError for handler failover, got: %T", err)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
}

func TestForwardAsAnthropic_TransportError_DoesNotWriteResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{
		err: errors.New(`read tcp: connection reset by peer`),
	}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	account := rawChatCompletionsTestAccount()
	_, _ = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.Equal(t, http.StatusOK, rec.Code, "transport error must not write HTTP response — handler owns the response for failover")
	require.Empty(t, rec.Body.String(), "response body must be empty so handler can write the correct error or failover")
}

func TestForwardAsAnthropic_TransportError_ClientCanceled_NoFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(cancelCtx)
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{
		err: context.Canceled,
	}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	account := rawChatCompletionsTestAccount()
	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "client-canceled transport error should NOT trigger failover")
}

// /messages 的缓冲路径与 chat 缓冲路径同病：上游 200 + 非 SSE 顶包时直接甩 502，
// 下游明明一个字节都没收到，组里还有健康账号。
func TestHandleAnthropicBufferedStreamingResponse_NonSSEBodyWithoutOutputFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/plain; charset=utf-8"},
			"x-request-id": []string{"upstream-rid"},
		},
		Body: io.NopCloser(strings.NewReader("Hi! What can I help you with?")),
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	_, err := svc.handleAnthropicBufferedStreamingResponse(
		resp, c,
		&Account{ID: 31, Name: "openai-compat-upstream", Platform: PlatformOpenAI},
		"claude-fable-5", "claude-fable-5", "claude-fable-5", time.Now(),
	)

	require.Error(t, err)
	require.False(t, c.Writer.Written(), "没写出任何字节才谈得上安全重试")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
}

// 归类依赖：channel_monitor_v2_error_taxonomy.go 按 "missing terminal event"
// 这串字面量把错误归为 transport，措辞漂了监控就漏记。
func TestAnthropicBufferedMissingTerminalErrorTextIsClassifiable(t *testing.T) {
	require.True(t, channelMonitorV2ContainsAny(
		"stream usage incomplete: missing terminal event: upstream error: 502 (failover)",
		"missing terminal event"))
}
