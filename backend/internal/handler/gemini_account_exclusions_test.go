//go:build unit

package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiAccountExclusionsTrustAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, role, header string
		want               int
	}{
		{"admin", service.RoleAdmin, "1119,1133,1119", 2},
		{"user", service.RoleUser, "1119", 0},
		{"invalid", service.RoleAdmin, "1119,no", 0},
		{"negative", service.RoleAdmin, "-1", 0},
		{"too_many", service.RoleAdmin, strings.Repeat("1119,", 65) + "1133", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/", nil)
			c.Request.Header.Set(geminiExcludedAccountsHeader, tc.header)
			fs := NewFailoverState(3, false)
			installGeminiAccountExclusions(c, &service.APIKey{User: &service.User{Role: tc.role}}, fs)
			require.Len(t, fs.FailedAccountIDs, tc.want)
		})
	}
}

func TestGeminiCapabilityExclusionSurvives503Backoff(t *testing.T) {
	fs := NewFailoverState(3, false)
	fs.excludeForRequest(1119)
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), nil, 1133, service.PlatformGemini, 1, &service.UpstreamFailoverError{StatusCode: 404, Reason: service.GeminiModelNotSupportedReason}))
	fs.RecordProfitVeto(1127)
	fs.LastFailoverErr = &service.UpstreamFailoverError{StatusCode: 503}
	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(context.Background()))
	fs.FailedAccountIDs[1136] = struct{}{}
	require.Equal(t, FailoverContinue, fs.HandleSelectionExhausted(context.Background()))
	require.Contains(t, fs.FailedAccountIDs, int64(1119))
	require.Contains(t, fs.FailedAccountIDs, int64(1133))
	require.Contains(t, fs.FailedAccountIDs, int64(1127))
	require.NotContains(t, fs.FailedAccountIDs, int64(1136))
}
