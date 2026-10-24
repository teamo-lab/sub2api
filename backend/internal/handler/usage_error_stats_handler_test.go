package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type userErrorStatsOpsRepoStub struct {
	service.OpsRepository
	called bool
	filter *service.OpsUserErrorStatsFilter
	raw    *service.OpsUserErrorStatsRaw
}

func (s *userErrorStatsOpsRepoStub) GetUserErrorStatsRaw(ctx context.Context, filter *service.OpsUserErrorStatsFilter) (*service.OpsUserErrorStatsRaw, error) {
	s.called = true
	s.filter = filter
	if s.raw != nil {
		return s.raw, nil
	}
	return &service.OpsUserErrorStatsRaw{}, nil
}

func newUserErrorStatsTestRouter(opsRepo *userErrorStatsOpsRepoStub, apiKeyRepo *dailyUsageAPIKeyRepoStub, userID int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	apiKeySvc := service.NewAPIKeyService(apiKeyRepo, nil, nil, nil, nil, nil, nil)
	opsSvc := service.NewOpsService(opsRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler := NewUsageHandler(nil, apiKeySvc, opsSvc, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
		c.Next()
	})
	router.GET("/usage/error-stats", handler.ErrorStats)
	return router
}

func TestUsageErrorStatsRejectsOtherUsersAPIKey(t *testing.T) {
	opsRepo := &userErrorStatsOpsRepoStub{}
	apiKeyRepo := &dailyUsageAPIKeyRepoStub{keys: map[int64]*service.APIKey{
		7: {ID: 7, UserID: 99, Status: service.StatusAPIKeyActive},
	}}
	router := newUserErrorStatsTestRouter(opsRepo, apiKeyRepo, 42)

	req := httptest.NewRequest(http.MethodGet, "/usage/error-stats?api_key_id=7&start_date=2026-10-23&end_date=2026-10-24", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.False(t, opsRepo.called)
}

func TestUsageErrorStatsScopesToCurrentUser(t *testing.T) {
	opsRepo := &userErrorStatsOpsRepoStub{raw: &service.OpsUserErrorStatsRaw{
		Success: []service.OpsUserBucketCount{{Bucket: "2026-10-24", Count: 9}},
		Errors:  []service.OpsUserErrorBucketRow{{Bucket: "2026-10-24", StatusCode: 502, Count: 1}},
	}}
	apiKeyRepo := &dailyUsageAPIKeyRepoStub{keys: map[int64]*service.APIKey{
		7: {ID: 7, UserID: 42, Status: service.StatusAPIKeyActive},
	}}
	router := newUserErrorStatsTestRouter(opsRepo, apiKeyRepo, 42)

	// user_id 查询参数必须被忽略:范围只来自登录态。
	req := httptest.NewRequest(http.MethodGet, "/usage/error-stats?user_id=99&api_key_id=7&model=gpt-6&granularity=hour&start_date=2026-10-23&end_date=2026-10-24", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, opsRepo.called)
	require.Equal(t, int64(42), opsRepo.filter.UserID)
	require.Equal(t, int64(7), opsRepo.filter.APIKeyID)
	require.Equal(t, "gpt-6", opsRepo.filter.Model)
	require.Equal(t, "hour", opsRepo.filter.Granularity)

	var resp struct {
		Data service.UserErrorStats `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int64(10), resp.Data.Total)
	require.Equal(t, int64(1), resp.Data.Failed)
	require.InDelta(t, 0.1, resp.Data.FailureRate, 1e-9)
}
