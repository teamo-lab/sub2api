package service

import (
	"context"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GetCacheAnalysis 返回窗口内的缓存命中率与未命中根因拆分。
func (s *OpsService) GetCacheAnalysis(ctx context.Context, filter *OpsDashboardFilter) (*OpsCacheAnalysisResponse, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	store, ok := s.opsRepo.(OpsCacheAnalysisStore)
	if !ok || store == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops cache analysis store not available")
	}
	if filter == nil {
		return nil, infraerrors.BadRequest("OPS_FILTER_REQUIRED", "filter is required")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, infraerrors.BadRequest("OPS_TIME_RANGE_REQUIRED", "start_time/end_time are required")
	}
	if filter.StartTime.After(filter.EndTime) {
		return nil, infraerrors.BadRequest("OPS_TIME_RANGE_INVALID", "start_time must be <= end_time")
	}
	return store.QueryCacheAnalysis(ctx, filter)
}
