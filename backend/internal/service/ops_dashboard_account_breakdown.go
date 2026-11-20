package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// OpsAccountBreakdownLimit caps how many accounts one breakdown returns.
const OpsAccountBreakdownLimit = 500

// OpsDashboardAccountBreakdownItem is one upstream account's request quality in the
// dashboard window. Counts use the same definitions as OpsDashboardOverview, so the
// rows of a group add up to the group's overview.
type OpsDashboardAccountBreakdownItem struct {
	// AccountID is 0 for failed requests that never got an account (for example
	// "no available accounts").
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`

	SuccessCount      int64 `json:"success_count"`
	ErrorCountTotal   int64 `json:"error_count_total"`
	ErrorCountSLA     int64 `json:"error_count_sla"`
	RequestCountTotal int64 `json:"request_count_total"`
	RequestCountSLA   int64 `json:"request_count_sla"`

	// SLA is nil when the account has no SLA samples (only business-limited errors).
	SLA *float64 `json:"sla"`

	TokenConsumed       int64 `json:"token_consumed"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`

	TTFTP50MS     *int `json:"ttft_p50_ms"`
	TTFTP90MS     *int `json:"ttft_p90_ms"`
	DurationP50MS *int `json:"duration_p50_ms"`
	DurationP90MS *int `json:"duration_p90_ms"`
}

type OpsDashboardAccountBreakdownResponse struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	// LatencyDegraded is true when the percentile query timed out and the latency
	// columns are left empty.
	LatencyDegraded bool `json:"latency_degraded"`
	// Truncated is true when more than OpsAccountBreakdownLimit accounts matched.
	Truncated bool                                `json:"truncated"`
	Items     []*OpsDashboardAccountBreakdownItem `json:"items"`
}

func (s *OpsService) GetDashboardAccountBreakdown(ctx context.Context, filter *OpsDashboardFilter) (*OpsDashboardAccountBreakdownResponse, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s.opsRepo == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_REPO_UNAVAILABLE", "Ops repository not available")
	}
	if filter == nil || filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, infraerrors.BadRequest("OPS_TIME_RANGE_REQUIRED", "start_time/end_time are required")
	}
	if filter.StartTime.After(filter.EndTime) {
		return nil, infraerrors.BadRequest("OPS_TIME_RANGE_INVALID", "start_time must be <= end_time")
	}
	return s.opsRepo.GetDashboardAccountBreakdown(ctx, filter)
}
