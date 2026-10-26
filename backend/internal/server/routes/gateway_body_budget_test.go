package routes

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var gatewayRouteParam = regexp.MustCompile(`[:*][A-Za-z_]+`)

// newGatewayBudgetTestRouter 和 newGatewayRoutesTestRouterWithConfig 一样，但带 Recovery：
// 被预算放行的请求会进到依赖为空的 handler，这里只关心有没有被预算拦下。
func newGatewayBudgetTestRouter(cfg *config.Config, platform string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			groupID := int64(1)
			c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{
				GroupID: &groupID,
				Group:   &service.Group{Platform: platform},
			})
			c.Next()
		}),
		nil, nil, nil, nil, nil,
		cfg,
	)
	return router
}

func budgetTestConfig(maxInflight int64) *config.Config {
	return &config.Config{
		Gateway: config.GatewayConfig{
			MaxBodySize:       8 << 20,
			TextMaxBodySize:   1 << 20,
			RequestBodyBudget: config.RequestBodyBudgetConfig{MaxInflightBytes: maxInflight},
		},
	}
}

// 每条会读请求体的网关路由（非 GET/HEAD）都挂着预算，而且预占有有限的上界：声明 MaxInt64 的
// 请求体按路由上限预占，超出预算时在读请求体之前就回 503 + Retry-After。
func TestEveryGatewayBodyRouteIsBehindTheRequestBodyBudget(t *testing.T) {
	router := newGatewayBudgetTestRouter(budgetTestConfig(512<<10), service.PlatformOpenAI)
	checked := 0
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet || route.Method == http.MethodHead {
			continue
		}
		// Gemini 分组用 Google 鉴权，依赖真实的 APIKeyService，这里鉴权不了（回 401，碰不到预算）；
		// 它们的预算接法和其它分组相同，错误格式由中间件的单测覆盖。
		if strings.HasPrefix(route.Path, "/v1beta/") || strings.HasPrefix(route.Path, "/antigravity/v1beta/") {
			continue
		}
		path := gatewayRouteParam.ReplaceAllString(route.Path, "x")
		req := httptest.NewRequest(route.Method, path, bytes.NewReader([]byte(`{}`)))
		req.ContentLength = math.MaxInt64
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s %s", route.Method, route.Path)
		require.Equal(t, "10", rec.Header().Get("Retry-After"), "%s %s", route.Method, route.Path)
		checked++
	}
	require.Greater(t, checked, 40, "the walk must actually cover the gateway's body routes")
}

// 文本类接口（embeddings、alpha search）只经过一层更小的上限：预算按它预占，不会先按 max_body_size 预占。
// 预算 2MiB、声明 4MiB：普通路由按 4MiB 预占被拒，文本路由按 1MiB 预占被放行。
func TestGatewayTextRoutesReserveTheTextBodyLimit(t *testing.T) {
	router := newGatewayBudgetTestRouter(budgetTestConfig(2<<20), service.PlatformOpenAI)
	send := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
		req.ContentLength = 4 << 20
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, path := range []string{"/v1/embeddings", "/embeddings", "/v1/alpha/search", "/alpha/search", "/backend-api/codex/alpha/search"} {
		require.NotEqual(t, http.StatusServiceUnavailable, send(path), path)
	}
	for _, path := range []string{"/v1/responses", "/responses", "/v1/chat/completions", "/backend-api/codex/responses"} {
		require.Equal(t, http.StatusServiceUnavailable, send(path), path)
	}
}
