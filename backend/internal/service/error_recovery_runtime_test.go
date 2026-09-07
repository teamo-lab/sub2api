package service

import (
	"context"
	"encoding/json"
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

// 流内容量错误（HTTP 200 + SSE error/response.failed）合成的 failover 信封必须
// 保留上游错误码。matchRecoveryRule 的第一步是 recoveryErrorCode(ResponseBody)，
// 取不到 code 就直接放弃匹配——信封只带 type/message 时，线上主力失败类
// （429 rate_limit_exceeded、503 server_error、502 upstream_error）永远进不了
// 换号恢复，表现为规则配了却零命中。
func TestStreamFailoverEnvelopeKeepsUpstreamErrorCodeForRecovery(t *testing.T) {
	cases := []struct {
		name, payload, wantCode string
	}{
		{"rate limit", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later","type":"rate_limit_error"},"status":"failed"}}`, "rate_limit_exceeded"},
		{"overloaded", `{"error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error"},"type":"error"}`, "server_error"},
		{"upstream error", `{"error":{"code":"upstream_error","message":"Upstream service temporarily unavailable","type":"upstream_error"},"type":"response.failed"}`, "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantCode, openAIStreamFailedEventErrorCode([]byte(tc.payload)),
				"上游码提取必须先于信封合成成立")

			envelope := gin.H{"type": "upstream_error", "message": "x"}
			if code := openAIStreamFailedEventErrorCode([]byte(tc.payload)); code != "" {
				envelope["code"] = code
			}
			body, err := json.Marshal(gin.H{"error": envelope})
			require.NoError(t, err)

			// 恢复引擎看到的就是这个 body：必须能取回上游码，否则规则零命中。
			require.Equal(t, tc.wantCode, recoveryErrorCode(body))
		})
	}

	// 回归：没有 code 的信封会让恢复引擎放弃匹配。
	legacy, err := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "message": "x"}})
	require.NoError(t, err)
	require.Equal(t, "", recoveryErrorCode(legacy))
}
