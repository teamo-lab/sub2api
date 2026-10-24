package admin

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetDashboardQualityTrend returns the site-wide total-duration P50/P90 and SLA trend for the admin dashboard.
// GET /api/v1/admin/dashboard/quality-trend
// Query params: start_date, end_date (YYYY-MM-DD), granularity (day/hour), timezone
func (h *OpsHandler) GetDashboardQualityTrend(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}

	startTime, endTime := parseTimeRange(c)
	granularity := "day"
	if strings.TrimSpace(c.Query("granularity")) == "hour" {
		granularity = "hour"
	}

	trend, err := h.opsService.GetDashboardQualityTrend(c.Request.Context(), &service.OpsQualityTrendFilter{
		AllUsers:    true,
		StartTime:   startTime,
		EndTime:     endTime,
		Granularity: granularity,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, trend)
}
