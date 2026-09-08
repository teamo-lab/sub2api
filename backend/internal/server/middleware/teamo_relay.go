package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// TeamoRelayObservation runs after API-key authentication and before selection
// or queue admission. The assignment event precedes the outcome, so requests
// failing before the stream starts remain in the experimental denominator.
// These are normal structured logs consumed by OP; Relay has no receipt store.
func TeamoRelayObservation(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg == nil || len(cfg.Gateway.TeamoRelayGroupIDs) == 0 || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		path := strings.TrimSuffix(c.Request.URL.Path, "/")
		if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/chat/completions") && !strings.HasSuffix(path, "/messages") {
			c.Next()
			return
		}
		value, _ := c.Get("api_key")
		apiKey, ok := value.(*service.APIKey)
		if !ok || apiKey == nil || apiKey.GroupID == nil || !slices.Contains(cfg.Gateway.TeamoRelayGroupIDs, *apiKey.GroupID) {
			c.Next()
			return
		}
		enabled := service.TeamoRelayEnabledForRequest(cfg, c)
		routerTraceID, _ := normalizeCorrelationID(c.GetHeader("X-Trace-ID"))
		log := logger.FromContext(c.Request.Context()).With(
			zap.String("component", "teamo_relay"),
			zap.Int64("group_id", *apiKey.GroupID),
			zap.Bool("relay_enabled", enabled),
			zap.String("router_trace_id", routerTraceID),
		)
		log.Info("teamo_relay.assignment")
		c.Next()
		paths, _ := c.Get(service.TeamoRelayPathsKey)
		list, _ := paths.([]string)
		log.Info("teamo_relay.request_end",
			zap.Strings("relay_paths", list),
			zap.Int("http_status", c.Writer.Status()),
		)
	}
}
