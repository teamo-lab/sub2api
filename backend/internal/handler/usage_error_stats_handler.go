package handler

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ErrorStats returns the current user's failure rate and client-facing status code breakdown.
// GET /api/v1/usage/error-stats
//
// 只返回计数与客户端实际收到的状态码;user_id 来自登录态,api_key_id 由
// parseUserUsageFilters 校验归属(他人 key 回 403)。
func (h *UsageHandler) ErrorStats(c *gin.Context) {
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

	stats, err := h.opsService.GetUserErrorStats(c.Request.Context(), &service.OpsUserErrorStatsFilter{
		UserID:      parsed.Filters.UserID,
		APIKeyID:    parsed.Filters.APIKeyID,
		GroupID:     parsed.Filters.GroupID,
		Model:       parsed.Filters.Model,
		StartTime:   parsed.StartTime,
		EndTime:     parsed.EndTime,
		Granularity: granularity,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stats)
}
