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

type qualityTrendOpsRepoStub struct {
	service.OpsRepository
	usageFilter   *service.OpsQualityTrendFilter
	failureFilter *service.OpsQualityTrendFilter
}

func (s *qualityTrendOpsRepoStub) GetQualityTrendUsageRaw(ctx context.Context, filter *service.OpsQualityTrendFilter) ([]service.OpsQualityTrendBucket, error) {
	s.usageFilter = filter
	p50 := 1200.0
	return []service.OpsQualityTrendBucket{{Bucket: "2026-10-24 01:00", SuccessCount: 19, DurationP50Ms: &p50}}, nil
}

func (s *qualityTrendOpsRepoStub) GetQualityTrendFailures(ctx context.Context, filter *service.OpsQualityTrendFilter) ([]service.OpsUserBucketCount, error) {
	s.failureFilter = filter
	return []service.OpsUserBucketCount{{Bucket: "2026-10-24 01:00", Count: 1}}, nil
}

func TestUsageDashboardQualityTrendScopesToCurrentUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	opsRepo := &qualityTrendOpsRepoStub{}
	apiKeySvc := service.NewAPIKeyService(&dailyUsageAPIKeyRepoStub{}, nil, nil, nil, nil, nil, nil)
	opsSvc := service.NewOpsService(opsRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler := NewUsageHandler(nil, apiKeySvc, opsSvc, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Next()
	})
	router.GET("/usage/dashboard/quality-trend", handler.DashboardQualityTrend)

	// user_id 查询参数必须被忽略:范围只来自登录态,也绝不能变成全站查询。
	req := httptest.NewRequest(http.MethodGet, "/usage/dashboard/quality-trend?user_id=99&granularity=hour&start_date=2026-10-23&end_date=2026-10-24", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, int64(42), opsRepo.usageFilter.UserID)
	require.False(t, opsRepo.usageFilter.AllUsers)
	require.Equal(t, "hour", opsRepo.usageFilter.Granularity)
	require.Equal(t, int64(42), opsRepo.failureFilter.UserID)
	require.False(t, opsRepo.failureFilter.AllUsers)

	var resp struct {
		Data service.DashboardQualityTrend `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data.Points, 1)
	require.InDelta(t, 0.95, *resp.Data.Points[0].SLA, 1e-9)
	require.InDelta(t, 1200, *resp.Data.Points[0].DurationP50Ms, 1e-9)
}
