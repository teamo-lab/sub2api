package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

func TestTeamoRelayIngressRecordsAuthRejectWithoutAssigningGroup(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
	})
	cfg := &config.Config{Gateway: config.GatewayConfig{TeamoRelayGroupIDs: []int64{3}},
		Deployment: config.DeploymentConfig{Slot: "green", Version: "fixture", Digest: "fixture-digest"}}
	r.Use(TeamoRelayIngress(cfg))
	r.Use(func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	r.POST("/v1/responses", func(c *gin.Context) { t.Fatal("unauthenticated handler reached") })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	id, attempt := uuid.NewString(), uuid.NewString()
	req.Header.Set(relayObservationHeader, id)
	req.Header.Set(relayAttemptHeader, attempt)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	entries := logs.All()
	require.Len(t, entries, 2)
	require.Equal(t, "teamo_relay.ingress", entries[0].Message)
	require.Equal(t, "teamo_relay.ingress_end", entries[1].Message)
	fields := entries[1].ContextMap()
	require.Equal(t, id, fields["observation_id"])
	require.Equal(t, attempt, fields["attempt_id"])
	require.Equal(t, "green", fields["deployment_slot"])
	require.EqualValues(t, 401, fields["http_status"])
	require.Equal(t, true, fields[logger.OpsSystemLogSkipField])
	require.NotContains(t, fields, "group_id")
	require.NotContains(t, fields, "relay_enabled")
}

func TestTeamoRelayCorrelationCannotEnableRelayOrLogInvalidIDs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		group := int64(3)
		c.Set("api_key", &service.APIKey{ID: 1, GroupID: &group})
	})
	cfg := &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: false, TeamoRelayGroupIDs: []int64{3}}}
	r.Use(TeamoRelayIngress(cfg), TeamoRelayObservation(cfg))
	r.POST("/v1/responses", func(c *gin.Context) { c.Status(204) })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set(relayObservationHeader, "untrusted-string")
	req.Header.Set(relayAttemptHeader, uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	entries := logs.All()
	require.Len(t, entries, 2, "invalid ingress IDs must not generate pre-auth logs")
	require.Equal(t, "teamo_relay.assignment", entries[0].Message)
	require.Equal(t, false, entries[0].ContextMap()["relay_enabled"])
	require.NotContains(t, entries[0].ContextMap(), "observation_id")
	require.Equal(t, 204, rec.Code)
}

func TestTeamoRelayPanicDoesNotPublishPrematureSuccessStatus(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard))
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
	})
	r.Use(TeamoRelayIngress(&config.Config{Gateway: config.GatewayConfig{TeamoRelayGroupIDs: []int64{3}}}))
	r.POST("/v1/responses", func(c *gin.Context) { panic("synthetic panic") })
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set(relayObservationHeader, uuid.NewString())
	req.Header.Set(relayAttemptHeader, uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, 500, rec.Code)
	require.Len(t, logs.All(), 1)
	require.Equal(t, "teamo_relay.ingress", logs.All()[0].Message)
}
