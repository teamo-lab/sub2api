package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T, accountType string, retries, switches int) (*gin.Context, *httptest.ResponseRecorder, *Account, *UpstreamFailoverError) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	rule := &model.ErrorPassthroughRule{ID: 7, Name: "test", Enabled: true, MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{accountType}, UpstreamCodes: []string{"server_is_overloaded"}, SameAccountRetries: retries, AccountSwitches: switches, BudgetSeconds: 1}}
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, svc)
	a := &Account{ID: 1, Type: accountType, Platform: "openai"}
	err := &UpstreamFailoverError{StatusCode: 503, ClientStatusCode: 503, ResponseBody: []byte(`{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`)}
	t.Cleanup(func() { CloseErrorRecovery(c) })
	return c, w, a, err
}
func TestErrorRecoveryAPIKeyOneSwitchNoSameRetry(t *testing.T) {
	c, w, a, e := recoveryFixture(t, "apikey", 0, 1)
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, a, "gpt", e))
	deadline := recoveryState(c).deadline
	a.ID = 2
	require.Equal(t, ErrorRecoveryStop, ApplyErrorRecovery(c, a, "gpt", e))
	require.Equal(t, deadline, recoveryState(c).deadline)
	WriteErrorRecoveryExhausted(c)
	WriteErrorRecoveryExhausted(c)
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), `"code":"server_is_overloaded"`)
	require.Contains(t, w.Body.String(), `"recovery_exhausted":true`)
}
func TestErrorRecoveryOAuthOneRetryThenSwitch(t *testing.T) {
	c, _, a, e := recoveryFixture(t, "oauth", 1, 1)
	require.Equal(t, ErrorRecoveryRetry, ApplyErrorRecovery(c, a, "gpt", e))
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, a, "gpt", e))
}
func TestErrorRecoveryAccountAndCodeIsolation(t *testing.T) {
	for _, tc := range []struct{ name, kind, body string }{
		{"wrong_account", "apikey", `{"error":{"code":"server_is_overloaded"}}`},
		{"generic503", "oauth", `{"error":{"code":"server_error","message":"unavailable"}}`},
		{"echoed_request", "oauth", `{"error":{"code":"server_error"},"request":{"error":{"code":"server_is_overloaded"}}}`},
		{"policy", "oauth", `{"error":{"code":"content_policy_violation"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, a, e := recoveryFixture(t, "oauth", 0, 1)
			a.Type = tc.kind
			e.ResponseBody = []byte(tc.body)
			require.Equal(t, ErrorRecoveryDefault, ApplyErrorRecovery(c, a, "gpt", e))
			require.Nil(t, recoveryState(c))
		})
	}
}
func TestErrorRecoveryDeadlineCancelsWaitingAttempt(t *testing.T) {
	c, w, a, e := recoveryFixture(t, "apikey", 0, 1)
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, a, "gpt", e))
	select {
	case <-c.Request.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("inflight attempt not cancelled")
	}
	require.True(t, WriteRecoveryBudgetError(c))
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "server_is_overloaded")
}
func TestErrorRecoveryOutputDisarmsBudget(t *testing.T) {
	c, _, a, e := recoveryFixture(t, "apikey", 0, 1)
	ApplyErrorRecovery(c, a, "gpt", e)
	CompleteErrorRecovery(c)
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, c.Request.Context().Err())
	require.False(t, RecoveryBudgetExpired(c))
}
func TestErrorRecoveryClientCancellationDoesNotWrite(t *testing.T) {
	c, w, a, e := recoveryFixture(t, "apikey", 0, 1)
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	ApplyErrorRecovery(c, a, "gpt", e)
	cancel()
	WriteErrorRecoveryExhausted(c)
	require.Empty(t, w.Body.String())
}
func TestErrorRecoveryCommittedStreamUsesSSEError(t *testing.T) {
	c, w, a, e := recoveryFixture(t, "apikey", 0, 0)
	require.Equal(t, ErrorRecoveryStop, ApplyErrorRecovery(c, a, "gpt", e))
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeaderNow()
	WriteErrorRecoveryExhausted(c)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "event: error")
	require.Contains(t, w.Body.String(), "server_is_overloaded")
}

// 中转上游把错误装在 SSE 帧里连同 5xx 状态码返回（线上账号 69 样本）：
// code 必须能从 data 载荷里取出来，否则恢复规则永远不匹配。
func TestRecoveryErrorCodeParsesSSEFramedBodies(t *testing.T) {
	sse := []byte("event: error\ndata: {\"error\":{\"code\":\"server_error\",\"message\":\"Our servers are currently overloaded. Please try again later.\",\"type\":\"service_unavailable_error\"},\"sequence_number\":2,\"type\":\"error\"}\n\n")
	require.Equal(t, "server_error", recoveryErrorCode(sse))

	multi := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\ndata: {\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n")
	require.Equal(t, "rate_limit_exceeded", recoveryErrorCode(multi))

	require.Equal(t, "server_error", recoveryErrorCode([]byte(`{"error":{"code":"server_error"}}`)))
	require.Equal(t, "", recoveryErrorCode([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")))
	require.Equal(t, "", recoveryErrorCode([]byte("plain text failure")))
}

func TestRecoveryErrorCodeTypeOnly(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"json", `{"error":{"type":"service_unavailable_error","message":"temporarily unavailable"}}`, "service_unavailable_error"},
		{"response_failed", `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":null}}}`, "rate_limit_error"},
		{"sse", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\"}}\n\n", "service_unavailable_error"},
		{"sse_response_failed", "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"rate_limit_error\"}}}\n\n", "rate_limit_error"},
		{"code_before_type", `{"error":{"code":"content_policy_violation","type":"service_unavailable_error"}}`, "content_policy_violation"},
		{"nested_code_before_type", `{"error":{"type":"service_unavailable_error"},"response":{"error":{"code":"invalid_api_key"}}}`, "invalid_api_key"},
		{"root_code_before_type", `{"code":"invalid_request_error","error":{"type":"service_unavailable_error"}}`, "invalid_request_error"},
		{"root_type_is_metadata", `{"type":"service_unavailable_error"}`, ""},
		{"response_type_is_metadata", `{"type":"response.failed","response":{"type":"service_unavailable_error"}}`, ""},
		{"echoed_request_is_not_error", `{"request":{"error":{"type":"service_unavailable_error"}}}`, ""},
		{"non_string_type", `{"error":{"type":{"value":"service_unavailable_error"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, recoveryErrorCode([]byte(tc.body)))
		})
	}
}

func TestErrorRecoveryTypeOnlyRuleScope(t *testing.T) {
	for _, tc := range []struct {
		name, accountType, platform, model, body string
		status                                   int
		want                                     ErrorRecoveryAction
	}{
		{"matching", "apikey", "openai", "gpt-5.1", `{"error":{"type":"service_unavailable_error"}}`, 503, ErrorRecoverySwitch},
		{"matching_sse", "apikey", "openai", "gpt-5.1", "event: error\ndata: {\"error\":{\"type\":\"service_unavailable_error\"}}\n\n", 503, ErrorRecoverySwitch},
		{"wrong_account_type", "oauth", "openai", "gpt-5.1", `{"error":{"type":"service_unavailable_error"}}`, 503, ErrorRecoveryDefault},
		{"wrong_platform", "apikey", "anthropic", "gpt-5.1", `{"error":{"type":"service_unavailable_error"}}`, 503, ErrorRecoveryDefault},
		{"wrong_model", "apikey", "openai", "other-model", `{"error":{"type":"service_unavailable_error"}}`, 503, ErrorRecoveryDefault},
		{"wrong_http_status", "apikey", "openai", "gpt-5.1", `{"error":{"type":"service_unavailable_error"}}`, 400, ErrorRecoveryDefault},
		{"permanent_code_wins", "apikey", "openai", "gpt-5.1", `{"error":{"code":"content_policy_violation","type":"service_unavailable_error"}}`, 503, ErrorRecoveryDefault},
		{"permanent_type", "apikey", "openai", "gpt-5.1", `{"error":{"type":"invalid_request_error"}}`, 503, ErrorRecoveryDefault},
		{"unknown_type", "apikey", "openai", "gpt-5.1", `{"error":{"type":"unknown_error"}}`, 503, ErrorRecoveryDefault},
		{"event_metadata", "apikey", "openai", "gpt-5.1", `{"type":"service_unavailable_error","response":{"type":"service_unavailable_error"}}`, 503, ErrorRecoveryDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, account, failure := recoveryFixture(t, "apikey", 0, 1)
			rule := &model.ErrorPassthroughRule{ID: 8, Enabled: true, MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{"apikey"}, Models: []string{"gpt-5.1"}, UpstreamCodes: []string{"service_unavailable_error"}, AccountSwitches: 1, BudgetSeconds: 1}}
			svc := &ErrorPassthroughService{}
			svc.setLocalCache([]*model.ErrorPassthroughRule{rule})
			BindErrorPassthroughService(c, svc)
			account.Type, account.Platform = tc.accountType, tc.platform
			failure.StatusCode, failure.ResponseBody = tc.status, []byte(tc.body)
			require.Equal(t, tc.want, ApplyErrorRecovery(c, account, tc.model, failure))
			if tc.want == ErrorRecoveryDefault {
				require.Nil(t, recoveryState(c))
			}
		})
	}
}

// 合成信封必须保留原始结构化 code/type，不能用网关生成的类型激活恢复规则。
func TestStreamFailoverEnvelopeKeepsUpstreamErrorCodeForRecovery(t *testing.T) {
	cases := []struct {
		name, payload, wantCode string
	}{
		{"rate limit", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later","type":"rate_limit_error"},"status":"failed"}}`, "rate_limit_exceeded"},
		{"overloaded", `{"error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"type":"error"}`, "server_error"},
		{"upstream error", `{"error":{"code":"upstream_error","message":"Upstream service temporarily unavailable","type":"upstream_error"},"type":"response.failed"}`, "upstream_error"},
		{"type-only error", `{"type":"error","error":{"type":"api_error","message":"temporarily unavailable"}}`, "api_error"},
		{"type-only response failure", `{"type":"response.failed","response":{"error":{"type":"service_unavailable_error","message":"temporarily unavailable"}}}`, "service_unavailable_error"},
		{"unknown error", `{"type":"response.failed","response":{"error":{"message":"something failed"}}}`, ""},
		{"event metadata", `{"type":"response.failed"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			failure := svc.newOpenAIStreamFailoverError(nil, account, false, "", []byte(tc.payload), "temporarily unavailable")
			require.Equal(t, tc.wantCode, recoveryFailureErrorCode(failure))
			if tc.wantCode == "" {
				require.NotNil(t, failure.RecoveryErrorCode, "explicit empty identity prevents inference from the generic envelope")
				c, _, scopedAccount, _ := recoveryFixture(t, "apikey", 0, 1)
				rule := &model.ErrorPassthroughRule{ID: 10, Enabled: true, MatchMode: "all", ErrorCodes: []int{502}, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", UpstreamCodes: []string{"upstream_error"}, AccountSwitches: 1, BudgetSeconds: 1}}
				recoveryService := &ErrorPassthroughService{}
				recoveryService.setLocalCache([]*model.ErrorPassthroughRule{rule})
				BindErrorPassthroughService(c, recoveryService)
				require.Equal(t, ErrorRecoveryDefault, ApplyErrorRecovery(c, scopedAccount, "gpt", failure))
			}
		})
	}
}
