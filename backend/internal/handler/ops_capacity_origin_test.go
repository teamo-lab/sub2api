package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsCapacityOriginUpstreamOverridesRoutingMarker(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetOpsUpstreamError(c, 502, "Concurrency limit exceeded for user, please retry later", "")
	markOpsRoutingCapacityLimited(c)
	phase, excluded, owner, source := classifyOpsErrorLog(c, "upstream_error", "Concurrency limit exceeded for user, please retry later", "gateway_concurrency_limit", 502)
	require.False(t, excluded)
	require.Equal(t, "upstream", phase)
	require.Equal(t, "provider", owner)
	require.Equal(t, "upstream_http", source)
}

func TestOpsCapacityOriginNativeSSECannotGainExclusionFromText(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		status        int
	}{
		{"concurrency_502", `{"type":"response.failed","response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}}`, 502},
		{"queue_429", `{"type":"error","error":{"type":"rate_limit_error","code":"gateway_queue_full","message":"Too many pending requests, please retry later"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 1)
			gin.SetMode(gin.TestMode)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/responses", func(c *gin.Context) {
				setOpsSelectedAccount(c, 22, "openai")
				c.Data(http.StatusOK, "text/event-stream", []byte("data: "+tc.failure+"\n\n"))
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			entry := (<-opsErrorLogQueue).entry
			require.Equal(t, tc.status, entry.StatusCode)
			require.False(t, entry.IsBusinessLimited, "unattributed capacity wording cannot prove a customer contract limit")
			require.Nil(t, entry.UpstreamStatusCode, "a parsed terminal does not prove that an upstream attempt happened")
			require.Nil(t, entry.UpstreamErrorMessage)
		})
	}
}

func TestOpsCapacityOriginLocalAndProviderSSEFields(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(map[bool]string{false: "local_user", true: "provider"}[provider], func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 1)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/responses", func(c *gin.Context) {
				if provider {
					service.SetOpsUpstreamError(c, 429, "Concurrency limit exceeded for user", "")
				} else {
					service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonUserConcurrency)
				}
				c.Data(200, "text/event-stream", []byte("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"gateway_concurrency_limit\",\"message\":\"Concurrency limit exceeded for user\"}}}\n\n"))
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			entry := (<-opsErrorLogQueue).entry
			require.Equal(t, 429, entry.StatusCode)
			require.Equal(t, !provider, entry.IsBusinessLimited)
			if provider {
				require.Equal(t, "provider", entry.ErrorOwner)
				require.NotNil(t, entry.UpstreamStatusCode)
				require.Equal(t, 429, *entry.UpstreamStatusCode)
			} else {
				require.Equal(t, "client", entry.ErrorOwner)
				require.Nil(t, entry.UpstreamStatusCode)
				require.Nil(t, entry.UpstreamErrorMessage)
			}
		})
	}
}

func TestOpsCapacityOriginWSAdmissionDoesNotLeakAcrossTurns(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
	service.BeginOpsStreamTurn(c, 1)
	service.MarkOpsStreamFailure(c, "rate_limit_error", "gateway_concurrency_limit", "Concurrency limit exceeded for account", 429)
	service.BeginOpsStreamTurn(c, 2)
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonUserConcurrency)
	service.MarkOpsStreamFailure(c, "rate_limit_error", "gateway_concurrency_limit", "Concurrency limit exceeded for user", 429)
	service.BeginOpsStreamTurn(c, 3)
	require.False(t, service.HasOpsClientBusinessLimited(c))
	service.SetOpsUpstreamError(c, 502, "provider failed", "")
	service.MarkOpsStreamFailure(c, "upstream_error", "upstream_error", "provider failed", 502)
	// Simulate a later user admission failure before the whole WS connection logs.
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonUserConcurrency)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	logOpsStreamError(c, ops, http.StatusSwitchingProtocols)
	require.Equal(t, int64(3), OpsErrorLogQueueLength())
	first, second, third := (<-opsErrorLogQueue).entry, (<-opsErrorLogQueue).entry, (<-opsErrorLogQueue).entry
	require.False(t, first.IsBusinessLimited)
	require.Equal(t, "platform", first.ErrorOwner)
	require.True(t, second.IsBusinessLimited)
	require.Equal(t, "client", second.ErrorOwner)
	require.Nil(t, second.UpstreamStatusCode)
	require.False(t, third.IsBusinessLimited)
	require.Equal(t, "provider", third.ErrorOwner)
}

func TestOpsCapacityOriginRequiresExplicitUserAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, message, marker string
		excluded              bool
	}{
		{"account", "Concurrency limit exceeded for account, please retry later", "", false},
		{"untrusted_user_text", "Concurrency limit exceeded for user, please retry later", "", false},
		{"queue_without_scope", "Too many pending requests, please retry later", "", false},
		{"global_image", "Image generation concurrency limit exceeded, please retry later", "", false},
		{"user_contract", "Concurrency limit exceeded for user, please retry later", "local_user_concurrency", true},
		{"user_queue", "Too many pending requests, please retry later", "local_user_wait_queue", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.marker != "" {
				service.MarkOpsClientBusinessLimited(c, tc.marker)
			}
			_, excluded, _, _ := classifyOpsErrorLog(c, "rate_limit_error", tc.message, "", 429)
			require.Equal(t, tc.excluded, excluded)
		})
	}
}
