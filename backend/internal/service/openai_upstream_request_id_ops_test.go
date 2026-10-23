package service

import (
	"bytes"
	"context"
	"errors"
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

// openAIMidStreamGOAWAY 复现 sv-1 case 137dbcae：上游 200 响应头和部分正文已经到达、
// 客户端已收到输出之后，HTTP/2 连接被 GOAWAY 断开。
var openAIMidStreamGOAWAY = errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=37, ErrCode=NO_ERROR, debug=\"\"")

func switchbaseUpstreamIDTestAccount() *Account {
	return &Account{
		ID: 4, Name: "switchbase-0.2x", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Client-Request-Id"},
	}
}

func midStreamTestHeaders() http.Header {
	return http.Header{
		"Content-Type":        []string{"text/event-stream"},
		"X-Request-Id":        []string{"chain-id-not-switchbase"},
		"X-Client-Request-Id": []string{"sb-req-137"},
		"Cf-Ray":              []string{"8f1e2d3c4b5a6978-SJC"},
	}
}

func requireSingleStreamErrorEvent(t *testing.T, c *gin.Context) *OpsUpstreamErrorEvent {
	t.Helper()
	events := openAIUpstreamIDOpsEvents(t, c)
	var streamErrors []*OpsUpstreamErrorEvent
	for _, ev := range events {
		if ev.Kind == "stream_error" {
			streamErrors = append(streamErrors, ev)
		}
	}
	require.Len(t, streamErrors, 1, "events=%+v", events)
	return streamErrors[0]
}

func TestOpenAIStreamingMidStreamReadError_RecordsAccountUpstreamRequestID(t *testing.T) {
	c, rec := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     midStreamTestHeaders(),
		Body: &openAIResponseFlushReadError{
			payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
			err:     openAIMidStreamGOAWAY,
		},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, switchbaseUpstreamIDTestAccount(), time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.ErrorContains(t, err, "stream read error")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "客户端已收到输出，不能换号")
	require.Contains(t, rec.Body.String(), "stream_read_error")

	ev := requireSingleStreamErrorEvent(t, c)
	require.Equal(t, int64(4), ev.AccountID)
	require.Equal(t, "switchbase-0.2x", ev.AccountName)
	require.Equal(t, http.StatusBadGateway, ev.UpstreamStatusCode)
	require.Equal(t, "sb-req-137", ev.UpstreamRequestID)
	require.Empty(t, ev.UpstreamCFRay, "拿到账号指定的业务 ID 时不另存 cf-ray")
	require.False(t, ev.Passthrough)
	require.Contains(t, ev.Message, "GOAWAY")
	status, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok)
	require.Equal(t, http.StatusBadGateway, status)
}

func TestOpenAIStreamingMidStreamReadError_FallsBackToXRequestIDAndKeepsCFRay(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	headers := midStreamTestHeaders()
	headers.Del("X-Client-Request-Id")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     headers,
		Body: &openAIResponseFlushReadError{
			payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
			err:     openAIMidStreamGOAWAY,
		},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, switchbaseUpstreamIDTestAccount(), time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.ErrorContains(t, err, "stream read error")
	ev := requireSingleStreamErrorEvent(t, c)
	require.Equal(t, "chain-id-not-switchbase", ev.UpstreamRequestID)
	require.Equal(t, "8f1e2d3c4b5a6978-SJC", ev.UpstreamCFRay)
}

func TestOpenAIStreamingMidStreamEOFWithoutTerminal_RecordsUpstreamRequestID(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     midStreamTestHeaders(),
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")),
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, switchbaseUpstreamIDTestAccount(), time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.ErrorContains(t, err, "missing terminal event")
	ev := requireSingleStreamErrorEvent(t, c)
	require.Equal(t, "sb-req-137", ev.UpstreamRequestID)
	require.Contains(t, ev.Message, "stream ended before terminal event")
}

func TestOpenAIStreamingClientCanceledAfterOutput_DoesNotRecordStreamError(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     midStreamTestHeaders(),
		Body: &openAIResponseFlushReadError{
			payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
			err:     context.Canceled,
		},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, switchbaseUpstreamIDTestAccount(), time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.ErrorIs(t, err, context.Canceled)
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok, "客户端取消不是上游断流")
}

func TestOpenAIPassthroughMidStreamReadError_RecordsAccountUpstreamRequestID(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     midStreamTestHeaders(),
		Body: &openAIResponseFlushReadError{
			payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
			err:     openAIMidStreamGOAWAY,
		},
	}

	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, switchbaseUpstreamIDTestAccount(), time.Now(), "gpt-6-astra", "gpt-6-astra")
	require.ErrorContains(t, err, "stream read error")
	ev := requireSingleStreamErrorEvent(t, c)
	require.True(t, ev.Passthrough)
	require.Equal(t, "sb-req-137", ev.UpstreamRequestID)
	require.Contains(t, ev.Message, "GOAWAY")
}

func TestOpenAIRawChatMidStreamTruncation_RecordsAccountUpstreamRequestID(t *testing.T) {
	c, _ := newOpenAIUpstreamIDTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	headers := midStreamTestHeaders()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     headers,
		Body: &openAIResponseFlushReadError{
			payload: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"),
			err:     openAIMidStreamGOAWAY,
		},
	}

	_, err := svc.streamRawChatCompletions(c, resp, switchbaseUpstreamIDTestAccount(), "gpt-6-astra", "gpt-6-astra", "gpt-6-astra", nil, nil, time.Now(), 20)
	require.Error(t, err)
	events := openAIUpstreamIDOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "http_error", events[0].Kind)
	require.Equal(t, "sb-req-137", events[0].UpstreamRequestID)
	require.Empty(t, events[0].UpstreamCFRay)

	// 账号没配头时沿用 x-request-id，同时保留 cf-ray。
	c2, _ := newOpenAIUpstreamIDTestContext()
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp.Body = &openAIResponseFlushReadError{
		payload: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"),
		err:     openAIMidStreamGOAWAY,
	}
	plain := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	_, err = svc.streamRawChatCompletions(c2, resp, plain, "gpt-6-astra", "gpt-6-astra", "gpt-6-astra", nil, nil, time.Now(), 20)
	require.Error(t, err)
	events = openAIUpstreamIDOpsEvents(t, c2)
	require.Len(t, events, 1)
	require.Equal(t, "chain-id-not-switchbase", events[0].UpstreamRequestID)
	require.Equal(t, "8f1e2d3c4b5a6978-SJC", events[0].UpstreamCFRay)
}
