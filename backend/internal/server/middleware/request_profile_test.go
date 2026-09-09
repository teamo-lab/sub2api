package middleware

import (
	"context"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestProfileScopeIncludesGatewayAliasesOnly(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses", "/responses/compact", "/chat/completions", "/backend-api/codex/responses", "/antigravity/v1/messages", "/v1beta/models/test:generateContent"} {
		if !isProfiledInferenceRequest(httptest.NewRequest("POST", path, nil)) {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/api/v1/admin/accounts", "/api/v1/auth/login", "/health"} {
		if isProfiledInferenceRequest(httptest.NewRequest("POST", path, nil)) {
			t.Fatal(path)
		}
	}
}
func TestRequestProfilePersistsOnceAtInfoAndWarnLevels(t *testing.T) {
	for _, level := range []string{"info", "warn"} {
		t.Run(level, func(t *testing.T) {
			sink := initMiddlewareTestLoggerWithLevel(t, level)
			router := gin.New()
			router.Use(RequestLogger(), Logger())
			router.POST("/v1/responses", func(c *gin.Context) {
				requestprofile.Start(c.Request.Context(), "prepare")()
				c.String(200, "unchanged")
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", nil))
			if w.Body.String() != "unchanged" {
				t.Fatal("response modified")
			}
			count := 0
			for _, e := range sink.list() {
				if e.Message == "http request completed" && e.Fields["request_profile"] != nil {
					count++
					s, ok := e.Fields["request_profile"].(*requestprofile.Snapshot)
					if !ok || len(s.Spans) != 1 || s.Evidence != "measured" {
						t.Fatal(e.Fields)
					}
					raw, _ := json.Marshal(e.Fields)
					if strings.Contains(string(raw), "Authorization") {
						t.Fatal("credential field captured")
					}
					if e.Fields["request_id"] != w.Header().Get("X-Request-ID") {
						t.Fatal("correlation lost")
					}
				}
			}
			if count != 1 {
				t.Fatal("duplicated or missing persisted profile", count)
			}
		})
	}
}
func TestRequestProfileSurvivesHandlerContextReplacement(t *testing.T) {
	sink := initMiddlewareTestLogger(t)
	router := gin.New()
	router.Use(RequestLogger(), Logger())
	router.POST("/responses", func(c *gin.Context) {
		requestprofile.Start(c.Request.Context(), "prepare")()
		c.Request = c.Request.WithContext(context.Background())
		c.Status(204)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/responses", nil))
	found := false
	for _, e := range sink.list() {
		if e.Message == "http request completed" && e.Fields["request_profile"] != nil {
			s, ok := e.Fields["request_profile"].(*requestprofile.Snapshot)
			found = ok && len(s.Spans) == 1 && e.Fields["request_id"] == w.Header().Get("X-Request-ID")
		}
	}
	if !found {
		t.Fatal("entry recorder lost")
	}
}

func TestRequestProfileDisabledPreservesResponseWithoutTrace(t *testing.T) {
	sink := initMiddlewareTestLogger(t)
	router := gin.New()
	router.Use(RequestLoggerWithProfiling(false), LoggerWithProfiling(false))
	router.POST("/responses", func(c *gin.Context) {
		if requestprofile.From(c.Request.Context()) != nil {
			t.Fatal("disabled recorder attached")
		}
		c.String(200, "same response")
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/responses", nil))
	if w.Body.String() != "same response" {
		t.Fatal(w.Body.String())
	}
	for _, e := range sink.list() {
		if _, ok := e.Fields["request_profile"]; ok {
			t.Fatal("disabled recorder emitted")
		}
	}
}
