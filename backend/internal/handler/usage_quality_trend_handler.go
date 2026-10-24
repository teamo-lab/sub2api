package handler

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// DashboardQualityTrend returns the current user's total-duration P50/P90 and SLA trend.
// GET /api/v1/usage/dashboard/quality-trend
//
// user_id 只来自登录态;只用时间范围和粒度,不接受其他筛选条件。
func (h *UsageHandler) DashboardQualityTrend(c *gin.Context) {
	parsed, ok := h.parseUserUsageFilters(c, true)
	if !ok {
		return
	}
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}

	granularity := "day"
	if strings.TrimSpace(c.Query("granularity")) == "hour" {
		granularity = "hour"
	}

	trend, err := h.opsService.GetDashboardQualityTrend(c.Request.Context(), &service.OpsQualityTrendFilter{
		UserID:      parsed.Filters.UserID,
		StartTime:   parsed.StartTime,
		EndTime:     parsed.EndTime,
		Granularity: granularity,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, trend)
}
