package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestRequestProfileSamplingMustNotHideProfiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := logger.Init(logger.InitOptions{
		Level: "info", Format: "json", ServiceName: "synthetic-profile-sampling", Environment: "test",
		Sampling: logger.SamplingOptions{Enabled: true, Initial: 1, Thereafter: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// Real bounded Ops queue; no worker/database is started.
	sink := service.NewOpsSystemLogSink(nil)
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil); sink.Stop() })
	router := gin.New()
	router.Use(RequestLoggerWithProfiling(true), LoggerWithProfiling(true))
	router.POST("/v1/responses", func(c *gin.Context) {
		requestprofile.Start(c.Request.Context(), "synthetic_stage")()
		c.String(200, "unchanged")
	})
	for i := 0; i < 10; i++ {
		writer := httptest.NewRecorder()
		router.ServeHTTP(writer, httptest.NewRequest("POST", "/v1/responses", nil))
		if writer.Code != 200 || writer.Body.String() != "unchanged" {
			t.Fatal("business response changed")
		}
	}
	health := sink.Health()
	t.Logf("completed_requests=10 profiles_entered_real_sink=%d sink_dropped=%d sink_write_failed=%d", health.QueueDepth, health.DroppedCount, health.WriteFailed)
	if health.QueueDepth != 10 {
		t.Fatalf("full profiling silently lost requests before its sink: queued=%d want=10 dropped=%d", health.QueueDepth, health.DroppedCount)
	}
}

func TestProfileRealSinkDisabledRejectedAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		enabled, reject      bool
		requests, wantQueued int
		wantDropped          uint64
	}{
		{"disabled", false, false, 10, 0, 0}, {"rejected", true, true, 10, 0, 0}, {"overflow", true, false, 5007, 5000, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := logger.Init(logger.InitOptions{Level: "warn", Format: "json", Sampling: logger.SamplingOptions{Enabled: true, Initial: 1, Thereafter: 100}}); err != nil {
				t.Fatal(err)
			}
			sink := service.NewOpsSystemLogSink(nil)
			logger.SetSink(sink)
			defer logger.SetSink(nil)
			defer sink.Stop()
			r := gin.New()
			r.Use(RequestLoggerWithProfiling(tc.enabled), LoggerWithProfiling(tc.enabled))
			r.POST("/responses", func(c *gin.Context) {
				if tc.reject {
					MarkIngressRejected(c, IngressRejectInvalidAPIKey)
					c.Status(401)
					return
				}
				c.String(200, "same")
			})
			for i := 0; i < tc.requests; i++ {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest("POST", "/responses", nil))
			}
			h := sink.Health()
			if int(h.QueueDepth) != tc.wantQueued || h.DroppedCount != tc.wantDropped {
				t.Fatalf("queue/drop mismatch %+v", h)
			}
		})
	}
}
