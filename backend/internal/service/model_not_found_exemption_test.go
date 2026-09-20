//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelNotFoundCooldownExempt(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"code", 404, `{"error":{"code":"model_not_found"}}`, true},
		{"nested", 404, `{"response":{"error":{"code":"model_not_found"}}}`, true},
		{"message", 404, `{"error":{"message":"model not found"}}`, true},
		{"missing model", 404, `{"error":{"message":"The model 'gpt-6-astra' does not exist"}}`, true},
		{"text", 404, `Unknown model gpt-6-astra`, true},
		{"endpoint", 404, `{"error":{"message":"endpoint not found"}}`, false},
		{"echoed model", 404, `{"error":{"message":"endpoint not found"},"model":"gpt-6-astra","input":"model not found"}`, false},
		{"different code", 404, `{"error":{"code":"route_not_found","message":"model not found"}}`, false},
		{"401", 401, `{"error":{"code":"model_not_found"}}`, false},
		{"429", 429, `{"error":{"code":"model_not_found"}}`, false},
		{"502", 502, `{"error":{"code":"model_not_found"}}`, false},
		{"503", 503, `{"error":{"code":"model_not_found"}}`, false},
		{"plan gated", 400, `{"detail":"model is not supported when using Codex"}`, true},
		{"plan gated message", 400, `{"error":{"code":"invalid_request_error","message":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}}`, true},
		{"plan gated nested", 400, `{"response":{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}}`, true},
		{"plan gated text", 400, `The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account.`, true},
		{"ordinary 400", 400, `{"error":{"message":"No tool output found for function call fc_123"}}`, false},
		{"context 400", 400, `{"error":{"message":"Input exceeds the context window"}}`, false},
		{"echoed plan gated", 400, `{"error":{"message":"invalid input"},"input":"model is not supported when using Codex"}`, false},
		{"code-only 400", 400, `{"error":{"code":"model_not_found"}}`, false},
		{"plan gated 429", 429, `{"detail":"model is not supported when using Codex"}`, false},
		{"plan gated 503", 503, `{"detail":"model is not supported when using Codex"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isModelNotFoundCooldownExempt(tt.status, []byte(tt.body)))
		})
	}
}

func TestModelNotFoundExemption_AllHealthEntrypoints(t *testing.T) {
	body := []byte(`{"error":{"code":"model_not_found","message":"model not found"}}`)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, custom := range []bool{false, true} {
			for _, pool := range []bool{false, true} {
				account := openAIModelNotFoundTempAccount()
				account.Type = accountType
				account.Credentials["pool_mode"] = pool
				account.Credentials["custom_error_codes_enabled"] = custom
				account.Credentials["custom_error_codes"] = []any{float64(404)}
				repo := &modelNotFoundAccountRepoStub{}
				svc := &RateLimitService{accountRepo: repo}
				ctx := context.Background()
				require.Equal(t, ErrorPolicySkipped, svc.CheckErrorPolicy(ctx, account, 404, body, "gpt-6-astra"))
				require.True(t, svc.HandleUpstreamModelNotFound(ctx, account, "gpt-6-astra", 404, body))
				require.True(t, svc.HandleUpstreamError(ctx, account, 404, http.Header{}, body, "gpt-6-astra"))
				require.True(t, svc.HandleUpstreamError(ctx, account, 404, http.Header{}, body))
				require.False(t, svc.HandleTempUnschedulable(ctx, account, 404, body, "gpt-6-astra"))
				require.False(t, svc.tryTempUnschedulable(ctx, account, 404, body))
				gateway := &OpenAIGatewayService{rateLimitService: svc}
				require.True(t, gateway.handleOpenAIAccountUpstreamError(ctx, account, 404, http.Header{}, body, "gpt-6-astra"))
				require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra"))
				require.Empty(t, repo.modelRateLimitCalls)
				require.Zero(t, repo.tempCalls)
				require.True(t, account.IsSchedulableForModelWithContext(ctx, "gpt-6-astra"))
			}
		}
	}
}

func TestModelNotFoundExemption_DisablesSameAccountRetry(t *testing.T) {
	body := []byte(`{"error":{"code":"model_not_found","message":"model not found"}}`)
	err := newOpenAIUpstreamFailoverError(404, nil, body, "model not found", true)
	require.Equal(t, 404, err.StatusCode)
	require.Equal(t, body, err.ResponseBody)
	require.False(t, err.RetryableOnSameAccount)
	require.False(t, err.RequestScopedTransient)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		account := &Account{Platform: PlatformOpenAI, Type: accountType}
		svc := &OpenAIGatewayService{}
		require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(404, "", body))
		require.True(t, shouldFailoverOpenAIPassthroughResponse(account, 404, body))
	}
}

func TestCodexPlanGatedExemption_AllHealthEntrypoints(t *testing.T) {
	for _, body := range []string{
		`{"detail":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}`,
		`{"error":{"code":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`,
		`{"response":{"error":{"message":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}}}`,
	} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, custom := range []bool{false, true} {
				account := openAICodexPlanGatedOAuthAccount()
				account.Type = accountType
				account.Credentials["custom_error_codes_enabled"] = custom
				account.Credentials["custom_error_codes"] = []any{float64(400)}
				account.Credentials["temp_unschedulable_enabled"] = true
				account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
					"error_code": float64(400), "keywords": []any{"not supported"}, "duration_minutes": float64(30),
				}}
				repo := &modelNotFoundAccountRepoStub{}
				svc := &RateLimitService{accountRepo: repo}
				ctx := context.Background()
				payload := []byte(body)
				require.Equal(t, ErrorPolicySkipped, svc.CheckErrorPolicy(ctx, account, 400, payload, "gpt-6-astra"))
				require.True(t, svc.HandleUpstreamModelNotFound(ctx, account, "gpt-6-astra", 400, payload))
				require.True(t, svc.HandleUpstreamError(ctx, account, 400, http.Header{}, payload, "gpt-6-astra"))
				require.True(t, svc.HandleUpstreamError(ctx, account, 400, http.Header{}, payload))
				require.False(t, svc.HandleTempUnschedulable(ctx, account, 400, payload, "gpt-6-astra"))
				require.False(t, svc.tryTempUnschedulable(ctx, account, 400, payload))
				gateway := &OpenAIGatewayService{rateLimitService: svc}
				require.True(t, gateway.handleOpenAIAccountUpstreamError(ctx, account, 400, http.Header{}, payload, "gpt-6-astra"))
				require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra"))
				require.Empty(t, repo.modelRateLimitCalls)
				require.Zero(t, repo.tempCalls)
				require.True(t, account.IsSchedulableForModelWithContext(ctx, "gpt-6-astra"))
				err := newOpenAIUpstreamFailoverError(400, nil, payload, "model unavailable", true)
				require.False(t, err.RetryableOnSameAccount)
				require.False(t, err.RequestScopedTransient)
				require.True(t, gateway.shouldFailoverOpenAIUpstreamResponse(400, "", payload))
				require.True(t, shouldFailoverOpenAIPassthroughResponse(account, 400, payload))
			}
		}
	}
}
