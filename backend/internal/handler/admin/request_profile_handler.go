package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"strconv"
	"strings"
	"time"
)

func (h *OpsHandler) GetRequestProfiles(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, 503, "Ops unavailable")
		return
	}
	end := time.Now()
	start := end.Add(-time.Hour)
	for key, dest := range map[string]*time.Time{"from": &start, "to": &end} {
		if v := c.Query(key); v != "" {
			t, e := time.Parse(time.RFC3339Nano, v)
			if e != nil {
				response.BadRequest(c, "Invalid "+key)
				return
			}
			*dest = t
		}
	}
	f := service.RequestProfileFilter{From: start, To: end, Model: strings.TrimSpace(c.Query("model")), Protocol: c.DefaultQuery("protocol", "sse"), ErrorType: c.Query("error_type"), RequestID: strings.TrimPrefix(strings.TrimSpace(c.Query("request_id")), "client:"), Page: 1, Limit: 50}
	f.IncludeOptions = c.Query("include_options") == "true"
	for key, dest := range map[string]*[]string{"models": &f.Models, "group_ids": &f.GroupIDs, "account_ids": &f.AccountIDs} {
		raw := c.Query(key)
		if raw == "" {
			continue
		}
		values := strings.Split(raw, ",")
		if len(values) > 100 {
			response.BadRequest(c, "Too many "+key)
			return
		}
		seen := map[string]bool{}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 256 {
				response.BadRequest(c, "Invalid "+key)
				return
			}
			if key != "models" {
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n < 1 {
					response.BadRequest(c, "Invalid "+key)
					return
				}
				value = strconv.FormatInt(n, 10)
			}
			if !seen[value] {
				*dest = append(*dest, value)
				seen[value] = true
			}
		}
	}
	for key, dest := range map[string]*int64{"group_id": &f.GroupID, "account_id": &f.AccountID, "id": &f.ID} {
		if v := c.Query(key); v != "" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 1 {
				response.BadRequest(c, "Invalid "+key)
				return
			}
			*dest = n
		}
	}
	for key, dest := range map[string]*int{"page": &f.Page, "limit": &f.Limit} {
		if v := c.Query(key); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 100000 {
				response.BadRequest(c, "Invalid "+key)
				return
			}
			*dest = n
		}
	}
	switch f.ErrorType {
	case "", "failed", "http_4xx", "http_5xx", "network_error", "timeout", "upstream_cancelled", "client_cancelled", "client_disconnected", "downstream_write_error", "retry", "fallback", "local_reselect":
	default:
		response.BadRequest(c, "Invalid error_type")
		return
	}
	switch f.Protocol {
	case "", "http", "sse", "websocket_session":
	default:
		response.BadRequest(c, "Invalid protocol")
		return
	}
	f.Evidence = c.DefaultQuery("evidence", "measured")
	if f.Evidence != "measured" && f.Evidence != "historical" {
		response.BadRequest(c, "Invalid evidence")
		return
	}
	result, err := h.opsService.QueryRequestProfiles(c.Request.Context(), f)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
