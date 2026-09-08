package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func nativeSafetyContext(writer http.ResponseWriter, ctx context.Context) (*OpenAIGatewayService, *gin.Context) {
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	enableTeamoRelayTestGroup(c)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}}}}
	return svc, c
}

func nativeSafetyRun(svc *OpenAIGatewayService, c *gin.Context, body io.ReadCloser) (*openaiStreamingResult, error) {
	return svc.handleStreamingResponse(c.Request.Context(), &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, time.Now(), "gpt-6-astra", "gpt-6-astra")
}

func nativeSafetyFrame(data string) string { return "data: " + data + "\n\n" }

func TestNativeRelayFirstDeliveryFlushIgnoresMeasuredUpstreamReasoningTTFT(t *testing.T) {
	// A busy scan queue does not request an ordinary queue-drain flush. The
	// first answer must independently flush even though reasoning set TTFT.
	require.True(t, nativeFirstDeliveryNeedsFlush(true, false, true, true, true))
	require.False(t, nativeFirstDeliveryNeedsFlush(false, false, true, true, true))
	require.False(t, nativeFirstDeliveryNeedsFlush(true, true, true, true, true))
	require.False(t, nativeFirstDeliveryNeedsFlush(true, false, true, true, false))
}

func TestNativeRelayHoldClassificationIsNarrow(t *testing.T) {
	require.True(t, nativeRelayHoldableReasoning(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, "response.reasoning_summary_text.delta"))
	for _, tc := range []struct{ data, kind string }{
		{`{"delta":"unknown"}`, "response.reasoning_new_side_effect.delta"},
		{`{"item":{"type":"reasoning","encrypted_content":"opaque"}}`, "response.output_item.added"},
		{`{"item":{"type":"web_search_call"}}`, "response.output_item.added"},
		{`{"item":{"type":"function_call","arguments":"{}"}}`, "response.output_item.done"},
	} {
		require.False(t, nativeRelayHoldableReasoning(tc.data, tc.kind))
		require.False(t, nativeRelayNonExecutableText(tc.data, tc.kind))
	}
	require.True(t, nativeRelayNonExecutableText(`{"delta":"answer"}`, "response.output_text.delta"))
	require.False(t, nativeRelayNonExecutableText(`{"delta":"opaque"}`, "response.unknown.delta"))
}

func TestNativeRelayToolsAndTerminalVerdictsAreNotReplayed(t *testing.T) {
	created := nativeSafetyFrame(`{"type":"response.created","response":{"id":"resp_safe"}}`)
	reasoning := nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"held-thinking"}`)
	failed := nativeSafetyFrame(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"temporarily unavailable"}}}`)
	cases := []struct {
		name, wire string
		failover   bool
	}{
		{"partial_client_tool", created + reasoning + nativeSafetyFrame(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","name":"lookup","arguments":""}}`) + nativeSafetyFrame(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{"}`) + failed, true},
		{"executable_tool", created + reasoning + nativeSafetyFrame(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{}"}}`) + failed, false},
		{"opaque_tool", created + reasoning + nativeSafetyFrame(`{"type":"response.web_search_call.in_progress","item_id":"ws_1"}`) + failed, false},
		{"unknown_reasoning_family", created + nativeSafetyFrame(`{"type":"response.reasoning_side_effect.delta","delta":"opaque"}`) + failed, false},
		{"policy", created + reasoning + nativeSafetyFrame(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates policy"}}}`), false},
		{"context", created + reasoning + nativeSafetyFrame(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"maximum context length exceeded"}}}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			svc, c := nativeSafetyContext(rec, context.Background())
			_, err := nativeSafetyRun(svc, c, io.NopCloser(strings.NewReader(tc.wire)))
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.Equal(t, tc.failover, errors.As(err, &failover))
			require.NotContains(t, rec.Body.String(), "response.completed")
			if tc.failover {
				require.Empty(t, rec.Body.String())
			} else {
				require.Contains(t, rec.Body.String(), "response.failed")
			}
		})
	}
}

type nativeSafetyWriteError struct {
	header   http.Header
	accepted int
	calls    int
}

func (w *nativeSafetyWriteError) Header() http.Header { return w.header }
func (w *nativeSafetyWriteError) WriteHeader(int)     {}
func (w *nativeSafetyWriteError) Write(p []byte) (int, error) {
	w.calls++
	return min(w.accepted, len(p)), io.ErrShortWrite
}
func (w *nativeSafetyWriteError) Flush() {}

func TestNativeRelayUncertainWriteClosesReplayEvenWhenZeroBytesReported(t *testing.T) {
	for _, accepted := range []int{0, 5} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			writer := &nativeSafetyWriteError{header: http.Header{}, accepted: accepted}
			svc, c := nativeSafetyContext(writer, context.Background())
			wire := nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`) + nativeSafetyFrame(`{"type":"response.output_text.delta","delta":"answer"}`)
			_, err := nativeSafetyRun(svc, c, &nativeAuditBody{reader: strings.NewReader(wire), tail: errors.New("read reset")})
			var failover *UpstreamFailoverError
			require.Error(t, err)
			require.False(t, errors.As(err, &failover))
			require.Equal(t, 1, writer.calls)
		})
	}
}

func TestNativeRelayTextBeforeWriteCanRecoverAtBufferCap(t *testing.T) {
	rec := httptest.NewRecorder()
	svc, c := nativeSafetyContext(rec, context.Background())
	wire := nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"`+strings.Repeat("r", openAIFirstOutputStageMaxBytes-512)+`"}`) + nativeSafetyFrame(`{"type":"response.output_text.delta","delta":"`+strings.Repeat("a", 2048)+`"}`)
	_, err := nativeSafetyRun(svc, c, io.NopCloser(strings.NewReader(wire)))
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Empty(t, rec.Body.String())
}

type nativeSafetyObservedWriter struct {
	gin.ResponseWriter
	answerReady *atomic.Bool
	earlyWrite  *atomic.Bool
}

func (w *nativeSafetyObservedWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && !w.answerReady.Load() {
		w.earlyWrite.Store(true)
	}
	return w.ResponseWriter.Write(p)
}

func TestNativeRelayReasoningProgressPreservesFirstOutputBudget(t *testing.T) {
	var answerReady, earlyWrite atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
		w.(http.Flusher).Flush()
		time.Sleep(1100 * time.Millisecond)
		answerReady.Store(true)
		fmt.Fprint(w, nativeSafetyFrame(`{"type":"response.output_text.delta","delta":"answer"}`)+nativeSafetyFrame(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":2}}}`))
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	rec := httptest.NewRecorder()
	svc, c := nativeSafetyContext(rec, context.Background())
	c.Writer = &nativeSafetyObservedWriter{ResponseWriter: c.Writer, answerReady: &answerReady, earlyWrite: &earlyWrite}
	svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	result, err := nativeSafetyRun(svc, c, resp.Body)
	require.NoError(t, err)
	require.False(t, earlyWrite.Load())
	require.NotNil(t, result.firstTokenMs)
	require.Less(t, *result.firstTokenMs, 1000)
	require.Contains(t, rec.Body.String(), "answer")
}

func TestNativeRelayCancellationBeforeDeliveryDoesNotReplayOrLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"held"}`))
		cancel()
		_ = writer.Close()
	}()
	rec := httptest.NewRecorder()
	svc, c := nativeSafetyContext(rec, ctx)
	_, err := nativeSafetyRun(svc, c, reader)
	var failover *UpstreamFailoverError
	require.Error(t, err)
	require.False(t, errors.As(err, &failover))
	require.Empty(t, rec.Body.String())
}
