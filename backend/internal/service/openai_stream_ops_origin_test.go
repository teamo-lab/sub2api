package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const originFirstFailure = `{"type":"response.failed","response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"first upstream capacity failure"}}}`
const originLateFailure = `{"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"late context failure"}}}`

type originReadAheadBody struct {
	*strings.Reader
	eof atomic.Bool
}

func (r *originReadAheadBody) Read(value []byte) (int, error) {
	n, err := r.Reader.Read(value)
	if err == io.EOF {
		r.eof.Store(true)
	}
	return n, err
}

func (*originReadAheadBody) Close() error { return nil }

type originAsyncWriter struct {
	gin.ResponseWriter
	reader            *originReadAheadBody
	writtenAfterDrain bool
}

func (w *originAsyncWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte("gateway_concurrency_limit")) {
		w.writtenAfterDrain = w.reader.eof.Load()
	}
	return w.ResponseWriter.Write(value)
}

func (w *originAsyncWriter) WriteString(value string) (int, error) { return w.Write([]byte(value)) }

func TestOpenAIStreamOpsOriginAsyncBufferedTerminalCannotBeReplaced(t *testing.T) {
	// Once initial progress disarms the scanner gate, the producer can queue the
	// remaining (fewer than 16) lines before the consumer resumes. The assertion
	// below proves this exercised delayed flush after the late error was drained.
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial answer\"}\n\ndata: " + originFirstFailure + "\n\ndata: " + originLateFailure + "\n\n"
	c, recorder, upstream, svc, account := originTestFixture(t, false, body)
	svc.cfg.Gateway.StreamDataIntervalTimeout = 2
	reader := &originReadAheadBody{Reader: strings.NewReader(body)}
	upstream.responses[0].Body = reader
	writer := &originAsyncWriter{ResponseWriter: c.Writer, reader: reader}
	c.Writer = writer
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic probe"}`))
	require.Error(t, err)
	require.True(t, writer.writtenAfterDrain, "must exercise buffered terminal flushed after all upstream frames were consumed")
	require.Contains(t, recorder.Body.String(), "gateway_concurrency_limit")
	require.NotContains(t, recorder.Body.String(), "context_length_exceeded")
	value, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events := value.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.Equal(t, 502, events[0].UpstreamStatusCode)
	require.Equal(t, "first upstream capacity failure", events[0].Message)
}

func TestOpenAIStreamOpsOriginFreezesFirstDeliveredFailure(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial answer\"}\n\ndata: " + originFirstFailure + "\n\ndata: " + originLateFailure + "\n\n"
			c, recorder, upstream, svc, account := originTestFixture(t, passthrough, body)
			rules := &ErrorPassthroughService{}
			rules.setLocalCache([]*model.ErrorPassthroughRule{{ID: 93, Name: "synthetic late exclusion", Enabled: true, MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{400}, Keywords: []string{"context"}, PassthroughCode: true, PassthroughBody: true, SkipMonitoring: true}})
			BindErrorPassthroughService(c, rules)
			_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic probe"}`))
			require.Error(t, err)
			require.Len(t, upstream.bodies, 1)
			require.NotContains(t, recorder.Body.String(), "context_length_exceeded")
			eventsValue, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events := eventsValue.([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, 502, events[0].UpstreamStatusCode)
			require.Equal(t, "first upstream capacity failure", events[0].Message)
			require.False(t, events[0].SkipMonitoring)
			require.False(t, c.GetBool(OpsSkipPassthroughKey))
		})
	}
}

type originFailingWriter struct {
	gin.ResponseWriter
	short bool
}

func (w originFailingWriter) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte("gateway_concurrency_limit")) {
		if w.short {
			return len(value) - 1, nil
		}
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(value)
}

func (w originFailingWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func TestOpenAIStreamOpsOriginDoesNotCommitFailedOrShortWrite(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough]+map[bool]string{false: "/write_error", true: "/short_write"}[short], func(t *testing.T) {
				body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial answer\"}\n\ndata: " + originFirstFailure + "\n\ndata: " + originLateFailure + "\n\n"
				c, _, upstream, svc, account := originTestFixture(t, passthrough, body)
				c.Writer = originFailingWriter{ResponseWriter: c.Writer, short: short}
				_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic probe"}`))
				require.Error(t, err)
				require.Len(t, upstream.bodies, 1)
				if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
					for _, event := range value.([]*OpsUpstreamErrorEvent) {
						require.NotEqual(t, "http_error", event.Kind, "drained or incompletely written errors do not own the client terminal")
					}
				}
			})
		}
	}
}

func originTestFixture(t *testing.T, passthrough bool, body string) (*gin.Context, *httptest.ResponseRecorder, *httpUpstreamRecorder, *OpenAIGatewayService, *Account) {
	t.Helper()
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid"}, Extra: map[string]any{"use_responses_api": true, "openai_passthrough": passthrough}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, recorder, upstream, svc, account
}

func TestOpenAIStreamOpsOriginAfterOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, failure := range []string{
			`{"type":"error","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}`,
			`{"type":"response.failed","response":{"status":"failed","error":{"code":"gateway_queue_full","type":"rate_limit_error","message":"Too many pending requests, please retry later"}}}`,
		} {
			t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough]+failure, func(t *testing.T) {
				upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial answer\"}\n\ndata: " + failure + "\n\n"))}}}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid"}, Extra: map[string]any{"use_responses_api": true, "openai_passthrough": passthrough}}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"synthetic probe"}`))
				require.Error(t, err)
				require.Len(t, upstream.bodies, 1, "this telemetry change must never replay an answered request")
				require.Contains(t, recorder.Body.String(), "synthetic partial answer")
				eventsValue, exists := c.Get(OpsUpstreamErrorsKey)
				require.True(t, exists, "delivered upstream failure needs explicit provenance before the logger classifies it")
				events := eventsValue.([]*OpsUpstreamErrorEvent)
				require.Len(t, events, 1)
				require.Equal(t, account.ID, events[0].AccountID)
				require.GreaterOrEqual(t, events[0].UpstreamStatusCode, 400)
			})
		}
	}
}
