package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// chunked 请求体读到一半预算不够：handler 要按服务端过载回 503 + Retry-After，
// 和已知长度请求在中间件里被拒的语义一致，而不是 400「Failed to read request body」。
func TestGatewayMessages_ChunkedBodyOverBudgetReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Gateway.MaxBodySize = 1 << 20
	budget := middleware2.NewRequestBodyBudget(config.RequestBodyBudgetConfig{MaxInflightBytes: 1000, MinBodyBytes: 100}, 0)
	require.NotNil(t, budget)

	r := gin.New()
	r.POST("/v1/messages",
		middleware2.RequestBodyLimitWithBudget(cfg.Gateway.MaxBodySize, budget),
		func(c *gin.Context) {
			apiKey := &service.APIKey{ID: 1, UserID: 7}
			c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})
			c.Next()
		},
		(&GatewayHandler{cfg: cfg}).Messages,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", io.NopCloser(strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"`+strings.Repeat("a", 4000)+`"}]}`)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "10", rec.Header().Get("Retry-After"))
	require.Contains(t, rec.Body.String(), `"overloaded_error"`)
	require.Equal(t, int64(0), budget.Used())
}
