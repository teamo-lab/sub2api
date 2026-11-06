package service

import (
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

// 线上样本（sv-1 11-06 14:46，Zero 中转站）：上游自己也是 Sub2API，它的上游流断了，
// 回 200 后只推一个扁平 error 帧。
const relayStreamReadErrorFrame = `{"code":"upstream_stream_read_error","message":"Upstream response stream was interrupted","param":null,"sequence_number":0,"type":"error"}`

func TestOpenAIStreamErrorEventShouldFailoverOnRelayStreamBreakCodes(t *testing.T) {
	for _, tc := range []struct {
		name, payload, message string
		want                   bool
	}{
		{"flat_stream_read_error", relayStreamReadErrorFrame, "Upstream response stream was interrupted", true},
		{"flat_http2_stream_error", `{"code":"upstream_http2_stream_error","message":"Upstream HTTP/2 stream failed","param":null,"sequence_number":0,"type":"error"}`, "Upstream HTTP/2 stream failed", true},
		{"flat_stream_truncated", `{"code":"upstream_stream_truncated","message":"Upstream response stream ended before completion","param":null,"sequence_number":0,"type":"error"}`, "Upstream response stream ended before completion", true},
		{"nested_stream_read_error", `{"type":"error","error":{"type":"upstream_error","code":"upstream_stream_read_error","message":"Upstream response stream was interrupted"}}`, "Upstream response stream was interrupted", true},
		{"policy_still_terminal", `{"type":"error","code":"content_policy_violation","message":"This request violates our usage policy"}`, "This request violates our usage policy", false},
		{"unknown_code_still_terminal", `{"type":"error","code":"some_other_error","message":"Upstream response stream was interrupted"}`, "Upstream response stream was interrupted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIStreamErrorEventShouldFailover([]byte(tc.payload), tc.message))
		})
	}
}

func relayStreamBreakRunners() []struct {
	name string
	run  func(*OpenAIGatewayService, *gin.Context, *http.Response, *Account) error
} {
	return []struct {
		name string
		run  func(*OpenAIGatewayService, *gin.Context, *http.Response, *Account) error
	}{
		{
			name: "native",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account) error {
				_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				return err
			},
		},
		{
			name: "passthrough",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account) error {
				_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				return err
			},
		},
	}
}

func runRelayStreamBreak(t *testing.T, stream string, run func(*OpenAIGatewayService, *gin.Context, *http.Response, *Account) error) (*httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(stream)),
		Header:     http.Header{"X-Request-Id": []string{"rid-relay-stream-break"}},
	}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "Zero"}
	return rec, c, run(svc, c, resp, account)
}

// 上游第一帧就是 relay 断流：不能把错误转给客户端，要换号重发。
func TestOpenAIStreamRelayStreamBreakBeforeOutputFailsOver(t *testing.T) {
	stream := strings.Join([]string{
		"event: error",
		"data: " + relayStreamReadErrorFrame,
		"",
	}, "\n")
	for _, tt := range relayStreamBreakRunners() {
		t.Run(tt.name, func(t *testing.T) {
			rec, c, err := runRelayStreamBreak(t, stream, tt.run)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
		})
	}
}

// 只下发了推理过程就断流：仍可换号（SafeToFailoverAfterWrite）。
func TestOpenAIStreamRelayStreamBreakAfterReasoningOnlyFailsOver(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]},"sequence_number":1}`,
		"",
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"thinking about it","sequence_number":2}`,
		"",
		"event: error",
		"data: " + relayStreamReadErrorFrame,
		"",
	}, "\n")
	for _, tt := range relayStreamBreakRunners() {
		t.Run(tt.name, func(t *testing.T) {
			rec, _, err := runRelayStreamBreak(t, stream, tt.run)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.True(t, failoverErr.SafeToFailoverAfterWrite)
			require.NotContains(t, rec.Body.String(), "upstream_stream_read_error")
		})
	}
}

// 正文已经下发后断流：不能重放，维持把终止错误转给客户端。
func TestOpenAIStreamRelayStreamBreakAfterAnswerOutputNotReplayed(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"answer","sequence_number":1}`,
		"",
		"event: error",
		"data: " + relayStreamReadErrorFrame,
		"",
	}, "\n")
	rec, _, err := runRelayStreamBreak(t, stream, relayStreamBreakRunners()[0].run)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Contains(t, rec.Body.String(), "answer")
}
