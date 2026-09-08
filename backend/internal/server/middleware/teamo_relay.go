package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const relayObservationHeader = "X-Teamo-Observation-ID"
const relayAttemptHeader = "X-Teamo-Attempt-ID"

// These IDs are correlation input only. They cannot select a group, enable
// Relay, authenticate a caller or prove an experiment arm without the matching
// Gateway admission and Router dispatch records.
func relayCorrelationFields(c *gin.Context) []zap.Field {
	fields := []zap.Field{zap.Bool(logger.OpsSystemLogSkipField, true)}
	for _, pair := range [][2]string{{relayObservationHeader, "observation_id"}, {relayAttemptHeader, "attempt_id"}} {
		raw := c.GetHeader(pair[0])
		id, err := uuid.Parse(raw)
		if err == nil && id.String() == raw && id != uuid.Nil {
			fields = append(fields, zap.String(pair[1], raw))
		}
	}
	return fields
}

// TeamoRelayIngress records pre-authentication transport arrival on configured
// experiment instances, including requests rejected before group assignment.
// It never uses an unverified Header as group identity or authorization.
func TeamoRelayIngress(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg == nil || len(cfg.Gateway.TeamoRelayGroupIDs) == 0 || c.Request == nil || c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		path := strings.TrimSuffix(c.Request.URL.Path, "/")
		if path != "/v1/responses" && path != "/v1/chat/completions" && path != "/v1/messages" {
			c.Next()
			return
		}
		fields := relayCorrelationFields(c)
		// Both canonical IDs are required to bound unsolicited log volume. A
		// dispatch with no matching ingress remains unknown in the collector.
		if len(fields) != 3 {
			c.Next()
			return
		}
		fields = append(fields, zap.String("component", "teamo_relay"),
			zap.String("deployment_slot", cfg.Deployment.Slot),
			zap.String("deployment_version", cfg.Deployment.Version),
			zap.String("deployment_digest", cfg.Deployment.Digest),
			zap.String("release_id", cfg.Deployment.ReleaseID))
		log := logger.FromContext(c.Request.Context()).With(fields...)
		log.Info("teamo_relay.ingress")
		c.Next()
		// A panic may be converted to HTTP 500 by an outer Recovery after this
		// frame unwinds. Do not publish a premature/default 200 from a defer.
		log.Info("teamo_relay.ingress_end", zap.Int("http_status", c.Writer.Status()))
	}
}

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
		).With(relayCorrelationFields(c)...)
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
