//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

var audit403Body = []byte(`{"error":{"type":"permission_error","message":"内容审计命中风险规则，请调整输入后重试"}}`)

func TestOpenAIGPTContentAuditScope(t *testing.T) {
	for _, tt := range []struct {
		name, platform, model string
		status                int
		body                  []byte
		want                  bool
	}{
		{"gpt", PlatformOpenAI, "gpt-5.6-terra", 403, audit403Body, true},
		{"code", PlatformOpenAI, "gpt-5.6-sol", 403, []byte(`{"error":{"code":"content_policy_violation","message":"blocked"}}`), true},
		{"nested", PlatformOpenAI, "gpt-5.6-sol", 403, []byte(`{"response":{"error":{"code":"content_filter","message":"blocked"}}}`), true},
		{"auth", PlatformOpenAI, "gpt-5.6-terra", 403, []byte(`{"error":{"code":"invalid_api_key","message":"forbidden"}}`), false},
		{"status", PlatformOpenAI, "gpt-5.6-terra", 429, audit403Body, false},
		{"claude", PlatformOpenAI, "claude-opus-4", 403, audit403Body, false},
		{"platform", PlatformAnthropic, "gpt-5.6-terra", 403, audit403Body, false},
		{"grok", PlatformGrok, "gpt-5.6-terra", 403, audit403Body, false},
		{"unknown", PlatformOpenAI, "", 403, audit403Body, false},
		{"echo", PlatformOpenAI, "gpt-5.6-terra", 403, []byte(`{"error":{"message":"permission denied"},"request":{"message":"内容审计命中风险规则"}}`), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAIGPTContentAuditRejection(&Account{Platform: tt.platform}, tt.model, tt.status, tt.body))
		})
	}
}
func TestOpenAIGPTContentAuditBypassesPoolRetryAndHealthPolicy(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	a := &Account{ID: 69, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 1}}
	s := &OpenAIGatewayService{}
	e := s.failoverOpenAIUpstreamHTTPError(context.Background(), c, a, &http.Response{StatusCode: 403, Header: http.Header{}}, audit403Body, "", "gpt-5.6-terra")
	require.NotNil(t, e)
	require.Equal(t, OpenAIContentAuditRejectedReason, e.Reason)
	require.False(t, e.ShouldRetryNextAccount())
	require.False(t, e.RetryableOnSameAccount)
	require.False(t, e.ShouldReportAccountScheduleFailure())
	require.Equal(t, audit403Body, e.ResponseBody)
}
func TestOpenAIGPTContentAuditChatCompletions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"hello"}],"stream":false}`)
			if stream {
				body = bytes.Replace(body, []byte(`"stream":false`), []byte(`"stream":true`), 1)
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
			up := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(audit403Body))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: up}
			a := rawChatCompletionsTestAccount()
			a.Credentials["pool_mode"] = true
			a.Credentials["pool_mode_retry_count"] = 1
			_, err := s.ForwardAsChatCompletions(context.Background(), c, a, body, "", "")
			var e *UpstreamFailoverError
			require.True(t, errors.As(err, &e), "%v", err)
			require.Equal(t, OpenAIContentAuditRejectedReason, e.Reason)
			require.False(t, e.ShouldRetryNextAccount())
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestOpenAIGPTContentAuditResponses(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[passthrough], func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-terra","input":"hello","stream":true}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			up := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(audit403Body))}}
			s := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: up}
			a := rawChatCompletionsTestAccount()
			a.Credentials["pool_mode"] = true
			a.Credentials["pool_mode_retry_count"] = 1
			a.Extra = map[string]any{"openai_passthrough": passthrough}
			_, err := s.Forward(context.Background(), c, a, body)
			var e *UpstreamFailoverError
			require.True(t, errors.As(err, &e), "%v", err)
			require.Equal(t, OpenAIContentAuditRejectedReason, e.Reason)
			require.False(t, e.ShouldRetryNextAccount())
			require.Empty(t, rec.Body.String())
		})
	}
}
