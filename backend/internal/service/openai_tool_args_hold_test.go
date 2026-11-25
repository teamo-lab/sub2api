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

func TestOpenAIToolArgsHoldBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	hold := newOpenAIToolArgsHold()
	hold.now = func() time.Time { return now }

	// 非参数帧既不开始也不延长暂存
	require.False(t, hold.Exceeded("response.created", 1<<20))
	require.False(t, hold.Exceeded("response.output_item.added", 1<<20))

	require.False(t, hold.Exceeded("response.function_call_arguments.delta", 100))
	now = now.Add(openAIToolArgsHoldMaxDuration - time.Millisecond)
	require.False(t, hold.Exceeded("response.function_call_arguments.delta", 100))
	now = now.Add(time.Millisecond)
	require.True(t, hold.Exceeded("response.function_call_arguments.delta", 100), "hold past its time budget must release")

	bytesHold := newOpenAIToolArgsHold()
	bytesHold.now = func() time.Time { return now }
	require.False(t, bytesHold.Exceeded("response.custom_tool_call_input.delta", openAIToolArgsHoldMaxBytes))
	require.True(t, bytesHold.Exceeded("response.custom_tool_call_input.delta", 1), "hold past its byte budget must release")

	var nilHold *openAIToolArgsHold
	require.False(t, nilHold.Exceeded("response.function_call_arguments.delta", 1<<30))
}

const toolArgsHoldTestPreamble = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_big"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"write","call_id":"c1","arguments":""}}` + "\n\n"

func toolArgsHoldTestBigDelta() string {
	return "event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"` +
		strings.Repeat("x", openAIToolArgsHoldMaxBytes) + `"}` + "\n\n"
}

func TestOpenAIStreamingPassthroughLongToolArgsStreamBeforeItemDone(t *testing.T) {
	bigDelta := toolArgsHoldTestBigDelta()
	rest := "event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"y"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"write","call_id":"c1","arguments":"xy"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_big","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	upstream := toolArgsHoldTestPreamble + bigDelta + rest

	_, recorder, writer, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	require.NoError(t, err)
	require.Equal(t, upstream, recorder.Body.String())
	// 参数超出暂存预算的那一帧就放行，不再等 output_item.done
	require.NotEmpty(t, writer.flushBodyLengths)
	require.Equal(t, len(toolArgsHoldTestPreamble)+len(bigDelta), writer.flushBodyLengths[0])
}

func TestOpenAIStreamingPassthroughFailureAfterLongToolArgsReleaseDoesNotReplay(t *testing.T) {
	upstream := toolArgsHoldTestPreamble + toolArgsHoldTestBigDelta() +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_big","status":"failed","error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}}` + "\n\n"

	_, recorder, _, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(upstream)), -1)

	// 半截参数已经写给下游，不能再换号重放（否则下游会收到两段参数）
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		require.False(t, failoverErr.SafeToFailoverAfterWrite)
	}
	require.Contains(t, recorder.Body.String(), "response.function_call_arguments.delta")
}

func newChatToolArgsHoldTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, rec
}

func TestHandleChatStreamingResponse_KeepaliveDuringHeldToolArgsOnLargeBody(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		_, _ = writer.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_ka"}}` + "\n\n"))
		_, _ = writer.Write([]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"write","call_id":"c1","arguments":""}}` + "\n\n"))
		// 上游持续吐参数（下游被扣着），跨过一个心跳节拍
		for i := 0; i < 12; i++ {
			_, _ = writer.Write([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"a"}` + "\n\n"))
			time.Sleep(120 * time.Millisecond)
		}
		_, _ = writer.Write([]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"write","call_id":"c1","arguments":"aaaaaaaaaaaa"}}` + "\n\n"))
		_, _ = writer.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_ka","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}()

	c, rec := newChatToolArgsHoldTestContext()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}}

	_, err := svc.handleChatStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", time.Now(), openAISilentRefusalMinRequestBodyBytes, "high")

	require.NoError(t, err)
	body := rec.Body.String()
	keepaliveAt := strings.Index(body, ":\n\n")
	toolCallAt := strings.Index(body, "tool_calls")
	require.GreaterOrEqual(t, keepaliveAt, 0, "large-body request must get a keepalive while tool args are held: %q", body)
	require.Greater(t, toolCallAt, keepaliveAt)
}

func TestHandleChatStreamingResponse_PreOutputFailureAfterKeepaliveStillFailsOver(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		_, _ = writer.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_fail"}}` + "\n\n"))
		time.Sleep(1300 * time.Millisecond)
		_, _ = writer.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_fail","status":"failed","error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}}` + "\n\n"))
	}()

	c, rec := newChatToolArgsHoldTestContext()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}}

	_, err := svc.handleChatStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", time.Now(), openAISilentRefusalMinRequestBodyBytes, "high")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.SafeToFailoverAfterWrite, "only keepalive comments reached the client")
	require.Equal(t, ":\n\n", rec.Body.String())
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c), "keepalive bytes are not semantic output")
}

func TestHandleChatStreamingResponse_LongToolArgsReleaseBeforeItemDone(t *testing.T) {
	big := strings.Repeat("x", openAIToolArgsHoldMaxBytes)
	upstream := `data: {"type":"response.created","response":{"id":"resp_big"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"write","call_id":"c1","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"` + big + `"}` + "\n\n" +
		`data: {"type":"response.failed","response":{"id":"resp_big","status":"failed","error":{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}}}` + "\n\n"

	c, rec := newChatToolArgsHoldTestContext()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstream))}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	_, err := svc.handleChatStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", time.Now(), 0, "high")

	// 超预算的参数已经边生成边下发；之后失败不能再换号重放
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "partial tool args were delivered; got %v", err)
	require.Contains(t, rec.Body.String(), "tool_calls")
	require.Contains(t, rec.Body.String(), big[:64])
}
