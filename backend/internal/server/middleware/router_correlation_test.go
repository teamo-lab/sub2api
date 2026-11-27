package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

func TestClientRequestIDCapturesRouterCorrelation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name        string
		requestID   string
		userID      string
		wantRequest string
		wantUser    string
	}{
		{"valid", "7dca1e02-c9a3-4ece-97ba-c49242ea4c6e", "u-2fb74c8f6df14d4d891d8d5bce273bff", "7dca1e02-c9a3-4ece-97ba-c49242ea4c6e", "u-2fb74c8f6df14d4d891d8d5bce273bff"},
		{"missing", "", "", "", ""},
		{"rejects spaces and quotes", "abc def", "u-1\"; drop", "", ""},
		{"rejects too long", repeat("x", 80), "u-ok", "", "u-ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotRequest, gotUser string
			r := gin.New()
			r.Use(ClientRequestID())
			r.GET("/", func(c *gin.Context) {
				gotRequest, _ = c.Request.Context().Value(ctxkey.RouterRequestID).(string)
				gotUser, _ = c.Request.Context().Value(ctxkey.RouterUserID).(string)
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.requestID != "" {
				req.Header.Set(routerRequestIDHeader, tc.requestID)
			}
			if tc.userID != "" {
				req.Header.Set(routerUserIDHeader, tc.userID)
			}
			r.ServeHTTP(httptest.NewRecorder(), req)
			if gotRequest != tc.wantRequest || gotUser != tc.wantUser {
				t.Fatalf("got (%q,%q), want (%q,%q)", gotRequest, gotUser, tc.wantRequest, tc.wantUser)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
