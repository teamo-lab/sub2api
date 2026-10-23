package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func openAIUpstreamIDOpsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func newOpenAIUpstreamIDTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func TestOpenAIHandleErrorResponse_RecordsAccountUpstreamRequestIDHeader(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID: 3, Name: "codebot", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Client-Request-Id"},
	}
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{
			"X-Request-Id":        []string{"chain-id-from-somewhere-else"},
			"X-Client-Request-Id": []string{"codebot-req-1"},
		},
		Body: io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"no available accounts"}}`))),
	}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	events := openAIUpstreamIDOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "codebot-req-1", events[0].UpstreamRequestID)
	require.Empty(t, events[0].UpstreamCFRay)
}

func TestOpenAIHandleErrorResponse_CloudflareEdge524KeepsCFRay(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{}
	account := &Account{
		ID: 2, Name: "avemujica", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Request-Id"},
	}
	resp := &http.Response{
		StatusCode: 524,
		Header: http.Header{
			"Cf-Ray":       []string{"8f1e2d3c4b5a6978-SJC"},
			"Server":       []string{"cloudflare"},
			"Content-Type": []string{"text/plain; charset=UTF-8"},
		},
		Body: io.NopCloser(strings.NewReader("error code: 524")),
	}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	events := openAIUpstreamIDOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, 524, events[0].UpstreamStatusCode)
	require.Empty(t, events[0].UpstreamRequestID)
	require.Equal(t, "8f1e2d3c4b5a6978-SJC", events[0].UpstreamCFRay)
}

func TestOpenAIHandleErrorResponse_UnconfiguredAccountKeepsXRequestID(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Header:     http.Header{"X-Request-Id": []string{"req_openai_1"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"bad"}}`))),
	}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	events := openAIUpstreamIDOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "req_openai_1", events[0].UpstreamRequestID)
}

func TestOpenAIStreamingFailoverBeforeOutput_RecordsAccountUpstreamRequestIDHeader(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	account := &Account{
		ID: 1, Name: "zero", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Oneapi-Request-Id"},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_is_overloaded","message":"Please retry later."}}}`,
			"",
		}, "\n"))),
		Header: http.Header{
			"X-Request-Id":        []string{"wrong-chain-id"},
			"X-Oneapi-Request-Id": []string{"zero-req-1"},
		},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)
	events := openAIUpstreamIDOpsEvents(t, c)
	require.NotEmpty(t, events)
	for _, ev := range events {
		require.Equal(t, "zero-req-1", ev.UpstreamRequestID)
	}
}

func TestSanitizeOpsUpstreamErrors_PersistsUpstreamCFRay(t *testing.T) {
	entry := &OpsInsertErrorLogInput{UpstreamErrors: []*OpsUpstreamErrorEvent{{
		AccountID:          2,
		UpstreamStatusCode: 524,
		Kind:               "failover",
		Detail:             "error code: 524",
		UpstreamCFRay:      " 8f1e2d3c4b5a6978-SJC ",
	}}}
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	require.NotNil(t, entry.UpstreamErrorsJSON)
	require.Contains(t, *entry.UpstreamErrorsJSON, `"upstream_cf_ray":"8f1e2d3c4b5a6978-SJC"`)
}
