//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const ccTerminalTestStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5-5\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n"
const ccTerminalTestText = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
const ccTerminalTestDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n"
const ccTerminalTestStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const ccTerminalTestError = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"DO_NOT_LOG_UPSTREAM_BODY\"}}\n\n"

type ccTerminalErrorReader struct{ err error }

func (r ccTerminalErrorReader) Read([]byte) (int, error) { return 0, r.err }
func ccTerminalTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, rec
}
func ccTerminalTestForward(stream bool, c *gin.Context, body io.Reader) (*ForwardResult, error) {
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(body)}
	resp.Header.Set("x-request-id", "supplier-request-id")
	if stream {
		return (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "claude-sonnet-5-5", "claude-sonnet-5-5", nil, time.Now(), true)
	}
	return (&GatewayService{}).handleCCBufferedFromAnthropic(resp, c, "claude-sonnet-5-5", "claude-sonnet-5-5", nil, time.Now())
}
func TestCCAnthropicStream_RejectsFailedAndIncompleteStreams(t *testing.T) {
	tests := []struct {
		name, raw, code string
		readErr         error
	}{
		{name: "empty", code: "upstream_stream_incomplete"},
		{name: "start_then_eof", raw: ccTerminalTestStart, code: "upstream_stream_incomplete"},
		{name: "error_before_start", raw: ccTerminalTestError, code: "upstream_stream_error"},
		{name: "start_then_error", raw: ccTerminalTestStart + ccTerminalTestError, code: "upstream_stream_error"},
		{name: "partial_text_then_eof", raw: ccTerminalTestStart + ccTerminalTestText, code: "upstream_stream_incomplete"},
		{name: "partial_text_then_error", raw: ccTerminalTestStart + ccTerminalTestText + ccTerminalTestError, code: "upstream_stream_error"},
		{name: "stop_reason_without_stop", raw: ccTerminalTestStart + ccTerminalTestText + ccTerminalTestDelta, code: "upstream_stream_incomplete"},
		{name: "stop_without_reason", raw: ccTerminalTestStart + ccTerminalTestStop, code: "upstream_stream_invalid_terminal"},
		{name: "stop_without_start", raw: ccTerminalTestStop, code: "upstream_stream_invalid_terminal"},
		{name: "invalid_event", raw: ccTerminalTestStart + "event: message_delta\ndata: {broken}\n\n" + ccTerminalTestStop, code: "upstream_stream_invalid_event"},
		{name: "null_terminal", raw: ccTerminalTestStart + ccTerminalTestDelta + "event: message_stop\ndata: null\n\n", code: "upstream_stream_invalid_event"},
		{name: "read_error", raw: ccTerminalTestStart, readErr: io.ErrUnexpectedEOF, code: "upstream_stream_read_error"},
		{name: "upstream_timeout", raw: ccTerminalTestStart, readErr: context.DeadlineExceeded, code: "upstream_stream_read_error"},
	}
	for _, stream := range []bool{false, true} {
		mode := "buffered"
		if stream {
			mode = "streaming"
		}
		for _, tt := range tests {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				c, rec := ccTerminalTestContext()
				var body io.Reader = strings.NewReader(tt.raw)
				if tt.readErr != nil {
					body = io.MultiReader(body, ccTerminalErrorReader{tt.readErr})
				}
				_, err := ccTerminalTestForward(stream, c, body)
				require.Error(t, err)
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, http.StatusBadGateway, failure.StatusCode)
				require.Contains(t, string(failure.ResponseBody), tt.code)
				require.Equal(t, "supplier-request-id", failure.ResponseHeaders.Get("x-request-id"))
				require.NotContains(t, rec.Body.String(), "[DONE]")
				require.NotContains(t, rec.Body.String(), `"finish_reason":"stop"`)
				if !stream || tt.raw == "" || tt.name == "error_before_start" || tt.name == "stop_without_start" {
					require.False(t, c.Writer.Written(), "early failures must remain eligible for bounded failover")
					require.Empty(t, rec.Body.String())
					require.Empty(t, c.Writer.Header().Get("Content-Type"))
				}
				value, ok := c.Get(OpsUpstreamErrorsKey)
				require.True(t, ok)
				events := value.([]*OpsUpstreamErrorEvent)
				require.Len(t, events, 1)
				require.Equal(t, "supplier-request-id", events[0].UpstreamRequestID)
				require.Contains(t, events[0].Detail, `"message_stop":false`)
				require.NotContains(t, events[0].Detail, "DO_NOT_LOG_UPSTREAM_BODY")
				require.NotContains(t, string(failure.ResponseBody), "DO_NOT_LOG_UPSTREAM_BODY")
			})
		}
	}
}
func TestCCAnthropicStream_ValidTerminalsPreserveSemantics(t *testing.T) {
	tool := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_test\",\"name\":\"Read\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\":\\\"/test\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	for _, stream := range []bool{false, true} {
		mode := "buffered"
		if stream {
			mode = "streaming"
		}
		for _, tt := range []struct {
			name, content, reason, finish string
			tokens                        int
		}{
			{"text", ccTerminalTestText, "end_turn", "stop", 5},
			{"one_token_probe", "", "max_tokens", "length", 1},
			{"empty_complete", "", "end_turn", "stop", 1},
			{"final_zero_overrides_initial_one", "", "end_turn", "stop", 0},
			{"truncated_text", ccTerminalTestText, "max_tokens", "length", 5},
			{"stop_sequence", ccTerminalTestText, "stop_sequence", "stop", 5},
			{"tool", tool, "tool_use", "tool_calls", 5},
		} {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				delta, _ := json.Marshal(map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": tt.reason}, "usage": map[string]int{"output_tokens": tt.tokens}})
				raw := ccTerminalTestStart + tt.content + "event: message_delta\ndata: " + string(delta) + "\n\n" + ccTerminalTestStop
				c, rec := ccTerminalTestContext()
				result, err := ccTerminalTestForward(stream, c, strings.NewReader(raw))
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, tt.tokens, result.Usage.OutputTokens)
				require.Equal(t, 20, result.Usage.InputTokens)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Contains(t, rec.Body.String(), `"finish_reason":"`+tt.finish+`"`)
				if tt.content == ccTerminalTestText {
					require.Contains(t, rec.Body.String(), "hello")
				}
				if tt.reason == "tool_use" {
					require.Contains(t, rec.Body.String(), "Read")
					require.Contains(t, rec.Body.String(), "file_path")
				}
				if stream {
					require.Contains(t, rec.Body.String(), "[DONE]")
				}
			})
		}
	}
}
func TestCCAnthropicStream_CanceledClientDoesNotFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		c, rec := ccTerminalTestContext()
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
		_, err := ccTerminalTestForward(stream, c, strings.NewReader(ccTerminalTestStart))
		require.ErrorIs(t, err, context.Canceled)
		var failure *UpstreamFailoverError
		require.False(t, errors.As(err, &failure))
		require.False(t, c.Writer.Written())
		require.Empty(t, rec.Body.String())
	}
}
func TestCCAnthropicStream_DoesNotReadPastValidatedTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		raw := ccTerminalTestStart + ccTerminalTestText + ccTerminalTestDelta + ccTerminalTestStop
		c, _ := ccTerminalTestContext()
		_, err := ccTerminalTestForward(stream, c, io.MultiReader(strings.NewReader(raw), ccTerminalErrorReader{io.ErrClosedPipe}))
		require.NoError(t, err)
	}
}
func TestCCAnthropicStream_SSEFramingAndLimits(t *testing.T) {
	raw := ccTerminalTestStart + ccTerminalTestText + ccTerminalTestDelta + ccTerminalTestStop
	var dataLines []string
	for _, l := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(l, "event:") {
			dataLines = append(dataLines, l)
		}
	}
	for _, tt := range []struct{ name, raw string }{
		{"compact", strings.ReplaceAll(strings.ReplaceAll(raw, "event: ", "event:"), "data: ", "data:")},
		{"data_only", strings.Join(dataLines, "\n")},
		{"comments_and_id", strings.ReplaceAll(raw, "\ndata:", "\n: heartbeat\nid: diagnostic\ndata:")},
		{"multiline_data", strings.Replace(raw, `"type":"message_delta","delta"`, `"type":"message_delta",`+"\ndata: "+`"delta"`, 1)},
		{"final_frame_without_blank_line", strings.TrimRight(raw, "\n")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := ccTerminalTestContext()
			_, err := ccTerminalTestForward(false, c, strings.NewReader(tt.raw))
			require.NoError(t, err)
		})
	}
	_, err := readCCAnthropicStream(context.Background(), strings.NewReader("data: "+strings.Repeat("x", 128)), 64, func(_ *apicompat.AnthropicStreamEvent, _ bool) error { return nil })
	require.Error(t, err)
}

func TestCCAnthropicStream_ClientWriteFailureIsNotSuccessOrFailover(t *testing.T) {
	c, rec := ccTerminalTestContext()
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
	raw := ccTerminalTestStart + ccTerminalTestText + ccTerminalTestDelta + ccTerminalTestStop
	_, err := ccTerminalTestForward(true, c, strings.NewReader(raw))
	require.Error(t, err)
	var writeErr *ccStreamWriteError
	require.ErrorAs(t, err, &writeErr)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotContains(t, rec.Body.String(), "[DONE]")
}

func TestCCAnthropicStream_InputOnlyUsageDeltaPreservesOutputCount(t *testing.T) {
	raw := ccTerminalTestStart + ccTerminalTestText + ccTerminalTestDelta +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":21}}\n\n" + ccTerminalTestStop
	for _, stream := range []bool{false, true} {
		c, rec := ccTerminalTestContext()
		result, err := ccTerminalTestForward(stream, c, strings.NewReader(raw))
		require.NoError(t, err)
		require.Equal(t, 5, result.Usage.OutputTokens)
		require.Equal(t, 21, result.Usage.InputTokens)
		require.Contains(t, rec.Body.String(), `"completion_tokens":5`)
	}
}
