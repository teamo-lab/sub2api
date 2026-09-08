package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetRouterOutcomeWithoutRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, RouterContractV2Requested(c))
	require.NotPanics(t, func() { SetRouterOutcome(c, RouterOutcomeRetryableAbort) })
	require.Empty(t, c.Writer.Header().Get(RouterOutcomeHeader))
}

func TestSetRouterOutcomeIsOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		header     string
		wantHeader string
	}{
		{name: "ordinary caller", header: "", wantHeader: ""},
		{name: "wrong version", header: "v1", wantHeader: ""},
		{name: "contract v2", header: "v2", wantHeader: "retryable_abort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			if tc.header != "" {
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				c.Request.Header.Set(RouterContractHeader, tc.header)
			} else {
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			}
			SetRouterOutcome(c, RouterOutcomeRetryableAbort)
			if tc.wantHeader == "" {
				require.Empty(t, w.Header().Get(RouterOutcomeHeader))
			} else {
				require.Equal(t, tc.wantHeader, w.Header().Get(RouterOutcomeHeader))
				require.Equal(t, RouterContractV2, w.Header().Get(RouterContractHeader))
			}
		})
	}
}

func TestSetRouterOutcomeDoesNotWriteAfterResponse(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	c.String(200, "already committed")
	SetRouterOutcome(c, RouterOutcomeRetryableAbort)
	require.Empty(t, w.Header().Get(RouterOutcomeHeader))
}
