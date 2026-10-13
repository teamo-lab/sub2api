//go:build unit

package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIResponses_ModelNotFoundSkipsPoolRetryAndExhaustsMaxSwitches(t *testing.T) {
	assertModelErrorBoundedFailover(t, http.StatusNotFound, `{"error":{"code":"model_not_found","message":"model not found"}}`, false)
}

func TestOpenAIResponses_CodexPlanGatedSkipsPoolRetryAndExhaustsMaxSwitches(t *testing.T) {
	t.Run("non_streaming", func(t *testing.T) {
		assertModelErrorBoundedFailover(t, http.StatusBadRequest, `{"detail":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}`, false)
	})
	t.Run("streaming", func(t *testing.T) {
		assertModelErrorBoundedFailover(t, http.StatusBadRequest, `{"detail":"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account."}`, true)
	})
}

func assertModelErrorBoundedFailover(t *testing.T, statusCode int, body string, streaming bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(4203)
	accounts := []service.Account{
		{
			ID: 9910, Name: "pool-api-key", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{
				"api_key":                      "sk-pool",
				"base_url":                     "https://api.example.test",
				"pool_mode":                    true,
				"pool_mode_retry_count":        float64(1),
				"pool_mode_retry_status_codes": []any{float64(statusCode)},
			},
			Extra: map[string]any{"openai_passthrough": true},
		},
		{
			ID: 9911, Name: "fallback-api-key", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2,
			Credentials: map[string]any{
				"api_key":  "sk-fallback",
				"base_url": "https://api.example.test",
			},
			Extra: map[string]any{"openai_passthrough": true},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxAccountSwitches = 1

	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	upstream := &openAIHTTPPassthroughFailoverUpstream{statusCode: statusCode, errorBody: body}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		upstream,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	h := NewOpenAIGatewayHandler(
		gatewaySvc,
		service.NewConcurrencyService(nil),
		billingCacheSvc,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	stream := "false"
	if streaming {
		stream = "true"
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"hello","stream":`+stream+`}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 1803, GroupID: &groupID,
		User:  &service.User{ID: 1703, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1703, Concurrency: 0})

	h.Responses(c)

	require.Equal(t, []int64{9910, 9911}, upstream.calls())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}
