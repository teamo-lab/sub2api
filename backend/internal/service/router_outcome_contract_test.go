package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRouterUpstreamAttemptsPreserveFallbackOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	BeginRouterUpstreamAttempts(c)
	RecordRouterUpstreamAccount(c, 12)
	MarkRouterUpstreamAttemptFailed(c, 12, http.StatusTooManyRequests, "rate_limit", time.Now().UnixMilli())
	RecordRouterUpstreamAccount(c, 57)

	var attempts []RouterUpstreamAttempt
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(RouterAttemptsHeader)), &attempts))
	require.Len(t, attempts, 2)
	require.Equal(t, int64(-12), attempts[0].AccountID)
	require.Equal(t, "failed", attempts[0].Result)
	require.Equal(t, http.StatusTooManyRequests, attempts[0].Status)
	require.Equal(t, int64(-57), attempts[1].AccountID)
	require.Equal(t, "pending", attempts[1].Result)
}

func TestRouterUpstreamAttemptsStayPrivate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	BeginRouterUpstreamAttempts(c)
	RecordRouterUpstreamAccount(c, 12)
	require.Empty(t, recorder.Header().Get(RouterAttemptsHeader))
}

func TestRouterUpstreamAttemptsStartUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	BeginRouterUpstreamAttempts(c)
	require.JSONEq(t, `[{"account_id":0,"result":"pending"}]`, recorder.Header().Get(RouterAttemptsHeader))
}
