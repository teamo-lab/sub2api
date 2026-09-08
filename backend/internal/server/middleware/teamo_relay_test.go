package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestTeamoRelayAssignmentIncludesQueueFailureWithoutStream(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		group := int64(3)
		c.Set("api_key", &service.APIKey{ID: 1, GroupID: &group})
		c.Next()
	})
	r.Use(TeamoRelayObservation(&config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}}}))
	r.POST("/v1/responses", func(c *gin.Context) {
		require.Equal(t, 1, logs.Len(), "assignment must be recorded before admission can fail")
		c.Status(http.StatusServiceUnavailable)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	entries := logs.All()
	require.Len(t, entries, 2)
	require.Equal(t, "teamo_relay.assignment", entries[0].Message)
	require.Equal(t, true, entries[0].ContextMap()["relay_enabled"])
	require.Equal(t, "teamo_relay.request_end", entries[1].Message)
	require.EqualValues(t, 503, entries[1].ContextMap()["http_status"])
	require.Empty(t, entries[1].ContextMap()["relay_paths"])
}
