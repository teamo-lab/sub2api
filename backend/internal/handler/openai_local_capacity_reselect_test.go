package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLocalCapacityReselectFlagUsesStrictTrustedGroupScope(t *testing.T) {
	group := int64(2)
	for _, tc := range []struct {
		name            string
		enabled, groups any
		want            bool
	}{
		{"enabled", true, []any{float64(2)}, true},
		{"typed_ids", true, []int64{2}, true},
		{"string_flag", "true", []int64{2}, false},
		{"disabled", false, []int64{2}, false},
		{"missing_ids", true, nil, false},
		{"wrong_group", true, []int64{3}, false},
		{"string_ids", true, []string{"2"}, false},
		{"fractional_id", true, []any{2.5}, false},
		{"null_id", true, []any{float64(2), nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &service.Account{ID: 23, Extra: map[string]any{localCapacityReselectEnabledKey: tc.enabled, localCapacityReselectGroupsKey: tc.groups}}
			require.Equal(t, tc.want, accountAllowsLocalCapacityReselect(account, &group))
			require.False(t, accountAllowsLocalCapacityReselect(account, nil))
		})
	}
}

func TestLocalCapacityReselectSelectorDependencyIsNotOriginalOrUpstream429(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r := &openAILocalCapacityReselect{pending: &localCapacityRejection{accountID: 23, platform: service.PlatformOpenAI, status: 429, code: gatewayQueueFullCode, errType: "rate_limit_error", message: "original account full"}}
	h := &OpenAIGatewayHandler{}
	require.True(t, r.handleSelectionError(h, c, errors.New("synthetic datastore failed"), false))
	require.Equal(t, 503, c.Writer.Status())
	require.Nil(t, r.pending)
	require.True(t, c.GetBool(service.OpsLocalCapacityFailureKey))
	_, upstream := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, upstream)
}

func TestLocalCapacityReselectAllowsOnlyUncommittedOutputOrHeartbeat(t *testing.T) {
	for _, mode := range []string{"unwritten", "heartbeat", "partial_answer", "error_headers", "committed_error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			switch mode {
			case "heartbeat":
				n, err := c.Writer.WriteString(": ping\n\n")
				require.NoError(t, err)
				recordGatewayStreamHeartbeat(c, n)
			case "partial_answer":
				_, err := c.Writer.WriteString("data: partial answer\n\n")
				require.NoError(t, err)
			case "error_headers":
				c.Writer.WriteHeader(http.StatusTooManyRequests)
				c.Writer.WriteHeaderNow()
			case "committed_error":
				service.MarkResponseCommitted(c)
			case "canceled":
				ctx, cancel := context.WithCancel(c.Request.Context())
				c.Request = c.Request.WithContext(ctx)
				cancel()
			}
			require.Equal(t, mode == "unwritten" || mode == "heartbeat", localCapacityReselectSafe(c))
		})
	}
}

func TestLocalCapacityReselectKeepsOriginalCancellationWhenRecoveryContextDetached(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	original, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(original)
	r := newOpenAILocalCapacityReselect(original, nil, nil, nil, 3)
	// Error recovery detaches cancellation and propagates it asynchronously.
	// Admission must still observe the actual original client immediately.
	c.Request = c.Request.WithContext(context.WithoutCancel(original))
	cancel()
	require.Nil(t, c.Request.Context().Err())
	require.False(t, r.requestActive(c))
}
