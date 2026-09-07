package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIFailoverExhaustedStampsOptInRouterOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		err    *service.UpstreamFailoverError
		want   string
		stream bool
	}{
		{
			name: "pre-dispatch upstream failure is retryable",
			err: &service.UpstreamFailoverError{
				StatusCode:   http.StatusServiceUnavailable,
				ResponseBody: []byte(`{"error":{"message":"sub capacity exhausted"}}`),
			},
			want: string(service.RouterOutcomeRetryableAbort),
		},
		{
			name: "content audit is terminal",
			err: &service.UpstreamFailoverError{
				StatusCode:    http.StatusForbidden,
				Reason:        service.OpenAIContentAuditRejectedReason,
				ClientMessage: "blocked",
				ResponseBody:  []byte(`{"error":{"code":"content_policy_violation"}}`),
			},
			want: string(service.RouterOutcomeTerminalError),
		},
		{
			name:   "already committed stream is not replayable",
			err:    &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway},
			want:   string(service.RouterOutcomeBusinessCommit),
			stream: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set(service.RouterContractHeader, service.RouterContractV2)
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, tt.err, tt.stream)
			require.Equal(t, tt.want, w.Header().Get(service.RouterOutcomeHeader))
			require.Equal(t, service.RouterContractV2, w.Header().Get(service.RouterContractHeader))
		})
	}
}
