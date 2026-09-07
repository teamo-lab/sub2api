package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDeploymentIdentityHandlerOverwritesClientIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/probe", func(c *gin.Context) {
		require.Empty(t, c.GetHeader(deploymentSlotHeader))
		c.Status(http.StatusNoContent)
	})
	cfg := &config.Config{
		Server: config.ServerConfig{Host: "127.0.0.1", Port: 8080, ReadHeaderTimeout: 10, MaxHeaderBytes: 64 * 1024, IdleTimeout: 120},
		Deployment: config.DeploymentConfig{
			ProcessRole: config.ProcessRoleAPI,
			Slot:        "green",
			ReleaseID:   "release-1",
			Version:     "v1",
			Digest:      "sha256:" + strings.Repeat("a", 64),
		},
	}
	server := ProvideHTTPServer(cfg, router)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(deploymentSlotHeader, "blue")
	recorder := httptest.NewRecorder()

	server.Handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, "green", recorder.Header().Get(deploymentSlotHeader))
	require.Equal(t, "release-1", recorder.Header().Get(deploymentReleaseHeader))
	require.Equal(t, config.ProcessRoleAPI, recorder.Header().Get(processRoleHeader))
}

func TestWorkerRoleServesOnlyHealth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	cfg := &config.Config{
		Server:     config.ServerConfig{Host: "127.0.0.1", Port: 8080},
		Deployment: config.DeploymentConfig{ProcessRole: config.ProcessRoleWorker, Slot: "worker"},
	}
	server := ProvideHTTPServer(cfg, router)

	health := httptest.NewRecorder()
	server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, health.Code)
	require.Equal(t, config.ProcessRoleWorker, health.Header().Get(processRoleHeader))

	api := httptest.NewRecorder()
	server.Handler.ServeHTTP(api, httptest.NewRequest(http.MethodPost, "/v1/probe", nil))
	require.Equal(t, http.StatusServiceUnavailable, api.Code)
	require.Contains(t, api.Body.String(), "worker_only")
}
