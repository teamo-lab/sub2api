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
