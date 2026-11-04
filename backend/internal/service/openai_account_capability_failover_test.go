//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const codebotModelUnsupported404Body = `{"error":{"type":"model_not_found","message":"Model \"gpt-6.1-terra\" is not supported by any configured account in this group"}}`

func TestIsOpenAIToolChoiceViolation(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  string
		body string
		want bool
	}{
		{"http json", "", `{"error":{"type":"upstream_error","message":"basispoints response violates tool_choice"}}`, true},
		{"stream frame", "", `{"type":"response.failed","response":{"error":{"message":"basispoints response violates tool_choice"}}}`, true},
		{"plain text", "", `basispoints response violates tool_choice`, true},
		{"upstream message", "Response violates tool_choice", ``, true},
		{"other error", "", `{"error":{"message":"Upstream request failed"}}`, false},
		{"echoed request content", "", `{"error":{"message":"invalid input"},"input":"response violates tool_choice"}`, false},
		{"policy violation", "", `{"error":{"message":"This request violates our usage policy"}}`, false},
		{"empty", "", ``, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAIToolChoiceViolation(tt.msg, []byte(tt.body)))
		})
	}
}

func TestIsCloudflareEdgeBlock(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"1010", 403, `error code: 1010`, true},
		{"1020 with whitespace", 403, "\n  Error Code: 1020\n", true},
		{"embedded", 403, `upstream said error code: 1006 (access denied)`, true},
		{"json quota", 403, `{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`, false},
		{"json echo", 403, `{"error":{"message":"forbidden"},"detail":"error code: 1010"}`, false},
		{"html", 403, `<!DOCTYPE html><html><body>error code: 1010</body></html>`, true},
		{"other status", 502, `error code: 1010`, false},
		{"non cloudflare code", 403, `error code: 502`, false},
		{"no digits", 403, `error code: 10`, false},
		{"plain forbidden", 403, `Forbidden`, false},
		{"empty", 403, ``, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isCloudflareEdgeBlock(tt.status, []byte(tt.body)))
		})
	}
}

func TestOpenAIAccountCapabilityMiss_FailoverErrorClassification(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       int
		body         string
		reason       GatewayFailureReason
		clientStatus int
		clientMsg    string
	}{
		{"model unsupported", http.StatusNotFound, codebotModelUnsupported404Body, openAIModelUnsupportedReason, http.StatusNotFound, `Model "gpt-6.1-terra" is not supported by any configured account in this group`},
		{"model not found code", http.StatusNotFound, `{"error":{"code":"model_not_found","message":"The model gpt-x does not exist"}}`, openAIModelUnsupportedReason, http.StatusNotFound, "The model gpt-x does not exist"},
		{"tool choice", http.StatusBadGateway, `{"error":{"message":"basispoints response violates tool_choice"}}`, openAIToolChoiceViolationReason, http.StatusBadGateway, "basispoints response violates tool_choice"},
		{"cloudflare", http.StatusForbidden, `error code: 1010`, openAIEdgeBlockedReason, 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			s := &OpenAIGatewayService{}
			require.True(t, s.shouldFailoverOpenAIUpstreamResponse(tt.status, extractUpstreamErrorMessage(body), body))

			// 池模式会要求同号重试；这里的错误必须直接换号。
			failoverErr := newOpenAIUpstreamFailoverError(tt.status, http.Header{}, body, extractUpstreamErrorMessage(body), true)
			require.Equal(t, tt.reason, failoverErr.Reason)
			require.True(t, failoverErr.IsOpenAIAccountCapabilityMiss())
			require.False(t, failoverErr.RetryableOnSameAccount)
			require.False(t, failoverErr.RequestScopedTransient)
			require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
			require.True(t, failoverErr.ShouldRetryNextAccount())
			require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
			require.Equal(t, tt.clientStatus, failoverErr.ClientStatusCode)
			require.Equal(t, tt.clientMsg, failoverErr.ClientMessage)
			_, _, eligible := classifyOpenAIAPIKeyHealthFailure(failoverErr)
			require.False(t, eligible, "must not count toward the API key health breaker")
		})
	}
}

func TestOpenAIAccountCapabilityMiss_LeavesOtherErrorsAlone(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"json 403 quota", http.StatusForbidden, `{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`},
		{"ordinary 502", http.StatusBadGateway, `{"error":{"message":"Upstream request failed"}}`},
		{"route 404", http.StatusNotFound, `{"error":{"message":"Invalid URL (POST /v1/foo)","type":"invalid_request_error"}}`},
		{"plan gated 400", http.StatusBadRequest, `{"detail":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			failoverErr := newOpenAIUpstreamFailoverError(tt.status, http.Header{}, body, extractUpstreamErrorMessage(body), false)
			require.False(t, failoverErr.IsOpenAIAccountCapabilityMiss())
			require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
		})
	}
	// 普通 5xx 仍计入健康熔断。
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(newOpenAIUpstreamFailoverError(http.StatusBadGateway, http.Header{}, []byte(`{"error":{"message":"Upstream request failed"}}`), "Upstream request failed", false))
	require.True(t, eligible)
	require.False(t, (*UpstreamFailoverError)(nil).IsOpenAIAccountCapabilityMiss())
}

func TestOpenAIPassthroughFailover_AccountCapabilityMiss(t *testing.T) {
	apiKey := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	oauth := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, account := range []*Account{apiKey, oauth} {
		require.True(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusNotFound, []byte(codebotModelUnsupported404Body)), account.Type)
		require.True(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusBadGateway, []byte(`{"error":{"message":"basispoints response violates tool_choice"}}`)), account.Type)
		require.True(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusForbidden, []byte(`error code: 1010`)), account.Type)
		// JSON 403 透传规则不变：原样回给客户端，不换号。
		require.False(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusForbidden, []byte(`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`)), account.Type)
		require.False(t, shouldFailoverOpenAIPassthroughResponse(account, http.StatusNotFound, []byte(`{"error":{"message":"Invalid URL (POST /v1/foo)","type":"invalid_request_error"}}`)), account.Type)
	}
}

func TestOpenAIStreamToolChoiceViolation_FailsOver(t *testing.T) {
	failed := []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"invalid_request_error","message":"basispoints response violates tool_choice"}}}`)
	require.True(t, openAIStreamFailedEventShouldFailover(failed, extractOpenAISSEErrorMessage(failed)))
	bare := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"basispoints response violates tool_choice"}}`)
	require.True(t, openAIStreamErrorEventShouldFailover(bare, extractOpenAISSEErrorMessage(bare)))

	// 其它 "violat" 仍是终止错误。
	policy := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","message":"This request violates our usage policy"}}}`)
	require.False(t, openAIStreamFailedEventShouldFailover(policy, extractOpenAISSEErrorMessage(policy)))

	s := &OpenAIGatewayService{}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	failoverErr := s.newOpenAIStreamFailoverError(nil, account, false, "", failed, extractOpenAISSEErrorMessage(failed))
	require.True(t, failoverErr.IsOpenAIToolChoiceViolation())
	require.False(t, failoverErr.ShouldReportAccountScheduleFailure())
	require.Equal(t, http.StatusBadGateway, failoverErr.ClientStatusCode)
	require.Equal(t, "basispoints response violates tool_choice", failoverErr.ClientMessage)
}

func TestOpenAIAccountCapabilityMiss_NoAccountCooldown(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"model unsupported", http.StatusNotFound, codebotModelUnsupported404Body},
		{"tool choice", http.StatusBadGateway, `{"error":{"message":"basispoints response violates tool_choice","type":"upstream_error"}}`},
		{"cloudflare", http.StatusForbidden, `error code: 1010`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newOpenAI403TestHarness(t, 801, 1, 2, 3, 4, 5)
			h.account.Type = AccountTypeAPIKey
			gateway := &OpenAIGatewayService{rateLimitService: h.svc}
			for i := 0; i < openAI403DisableThreshold+2; i++ {
				gateway.handleOpenAIAccountUpstreamError(context.Background(), h.account, tt.status, http.Header{}, []byte(tt.body), "gpt-6.1-terra")
			}
			h.requireNoAccountPenalty(t)
			require.False(t, gateway.isOpenAIAccountRuntimeBlocked(h.account))
			require.False(t, gateway.isOpenAIAccountModelRuntimeBlocked(h.account, "gpt-6.1-terra"))
		})
	}
}

func TestHandleUpstreamError_OpenAICloudflare403DoesNotPenalizeAccount(t *testing.T) {
	h := newOpenAI403TestHarness(t, 802, 1, 2, 3, 4, 5)
	for i := 0; i < openAI403DisableThreshold+2; i++ {
		require.False(t, h.handle("error code: 1010"))
	}
	h.requireNoAccountPenalty(t)

	// 对照：JSON 403 仍按原逻辑处罚。
	h = newOpenAI403TestHarness(t, 803, 1)
	require.True(t, h.handle(`{"error":{"message":"insufficient quota","type":"insufficient_quota"}}`))
	require.Equal(t, 1, h.repo.tempCalls)
}
