//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiEmptyResponseDetector_NonStream(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		empty bool
	}{
		// 1119/RootFlow 生产实测的空回复形态
		{"empty candidates", `{"candidates":[],"usageMetadata":{"promptTokenCount":812,"candidatesTokenCount":0,"totalTokenCount":812}}`, true},
		{"missing candidates", `{"usageMetadata":{"promptTokenCount":812,"totalTokenCount":812},"modelVersion":"gemini-3.8-flash"}`, true},
		{"empty text part with STOP", `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`, true},
		{"no parts no finishReason", `{"candidates":[{"content":{"role":"model"}}]}`, true},
		{"thought only", `{"candidates":[{"content":{"parts":[{"text":"let me think","thought":true},{"text":"","thoughtSignature":"abc"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":0,"thoughtsTokenCount":40}}`, true},
		{"empty body", ``, true},
		{"whitespace body", "  \n", true},
		{"code assist wrapper empty", `{"response":{"candidates":[],"usageMetadata":{"promptTokenCount":5}}}`, true},

		// 必须原样放行
		{"text answer", `{"candidates":[{"content":{"parts":[{"text":"Hello"}]},"finishReason":"STOP"}]}`, false},
		{"max tokens without content", `{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"candidatesTokenCount":0,"thoughtsTokenCount":1024}}`, false},
		{"safety", `{"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH","blocked":true}]}]}`, false},
		{"prohibited content", `{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}]}`, false},
		{"recitation", `{"candidates":[{"finishReason":"RECITATION"}]}`, false},
		{"prompt blocked", `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":9}}`, false},
		{"function call only", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":0}}`, false},
		{"image output", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]},"finishReason":"STOP"}]}`, false},
		{"output tokens counted", `{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":3}}`, false},
		{"upstream error object", `{"error":{"code":500,"message":"internal"}}`, false},
		{"not json", `<html>oops</html>`, false},
		{"json array", `[{"candidates":[]}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, det, err := peekGeminiResponseForEmpty(io.NopCloser(strings.NewReader(tc.body)), false, 0)
			require.NoError(t, err)
			require.Equal(t, tc.empty, det.empty())
			replayed, readErr := io.ReadAll(body)
			require.NoError(t, readErr)
			require.Equal(t, tc.body, string(replayed), "peek must not change forwarded bytes")
		})
	}
}

func geminiSSE(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\r\n\r\n")
	}
	return b.String()
}

func TestGeminiEmptyResponseDetector_Stream(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		empty  bool
	}{
		{"empty stop chunk", geminiSSE(`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`), true},
		{"empty candidates then usage", geminiSSE(`{"candidates":[]}`, `{"usageMetadata":{"promptTokenCount":812,"totalTokenCount":812}}`), true},
		{"no events", "", true},
		{"done only", "data: [DONE]\n\n", true},
		{"thought then empty stop", geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}`, `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`), true},
		{"comment lines only", ": keepalive\n\n", true},

		{"text in second chunk", geminiSSE(`{"candidates":[{"content":{"parts":[{"text":""}]}}]}`, `{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`), false},
		{"thought then text", geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}`, `{"candidates":[{"content":{"parts":[{"text":"Answer"}]}}]}`), false},
		{"max tokens", geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"x","thought":true}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"candidatesTokenCount":0}}`), false},
		{"safety", geminiSSE(`{"candidates":[{"finishReason":"SAFETY"}]}`), false},
		{"function call", geminiSSE(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}]}`), false},
		{"image", geminiSSE(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}}]}`), false},
		{"code assist wrapped text", geminiSSE(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}}`), false},
		{"in-stream error", geminiSSE(`{"error":{"code":429,"message":"Resource has been exhausted"}}`), false},
		{"json array stream", "[{\"candidates\":[]}\n]\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, det, err := peekGeminiResponseForEmpty(io.NopCloser(strings.NewReader(tc.stream)), true, 0)
			require.NoError(t, err)
			require.Equal(t, tc.empty, det.empty())
			replayed, readErr := io.ReadAll(body)
			require.NoError(t, readErr)
			require.Equal(t, tc.stream, string(replayed), "peek must not change forwarded bytes")
		})
	}
}

// 第一段回答到达后立刻放行，不再等待后面的数据（否则会拖慢首字）。
func TestGeminiEmptyResponsePeek_StopsAtFirstContent(t *testing.T) {
	head := geminiSSE(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`)
	boom := errors.New("upstream reset")
	body := io.NopCloser(io.MultiReader(strings.NewReader(head), geminiStickyErrReader{err: boom}))

	replay, det, err := peekGeminiResponseForEmpty(body, true, 0)
	require.NoError(t, err)
	require.False(t, det.empty())

	got, readErr := io.ReadAll(replay)
	require.ErrorIs(t, readErr, boom, "the rest of the stream must still flow to the handler")
	require.Equal(t, head, string(got))
}

func TestGeminiEmptyResponsePeek_ReadErrorIsReplayed(t *testing.T) {
	head := geminiSSE(`{"candidates":[]}`)
	boom := errors.New("upstream reset")
	body := io.NopCloser(io.MultiReader(strings.NewReader(head), geminiStickyErrReader{err: boom}))

	replay, _, err := peekGeminiResponseForEmpty(body, true, 0)
	require.ErrorIs(t, err, boom)
	got, readErr := io.ReadAll(replay)
	require.ErrorIs(t, readErr, boom)
	require.Equal(t, head, string(got))
}

func TestGeminiEmptyResponsePeek_LimitStopsBuffering(t *testing.T) {
	thought := `{"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("t", 200) + `","thought":true}]}}]}`
	stream := geminiSSE(thought, thought, thought, `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`)
	replay, det, err := peekGeminiResponseForEmpty(io.NopCloser(strings.NewReader(stream)), true, 300)
	require.NoError(t, err)
	require.False(t, det.empty(), "over the peek limit the gate must give up and pass through")
	got, readErr := io.ReadAll(replay)
	require.NoError(t, readErr)
	require.Equal(t, stream, string(got))
}

func requireGeminiEmptyFailover(t *testing.T, c *gin.Context, rec *httptest.ResponseRecorder, err error) {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, GeminiEmptyResponseReason, failoverErr.Reason)
	require.False(t, failoverErr.RetryableOnSameAccount, "empty replies switch to the next account")
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, c.Writer.Written(), "nothing may reach the client before failover")
	require.Zero(t, rec.Body.Len())

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "failover", last.Kind)
	require.Equal(t, string(GeminiEmptyResponseReason), last.Reason)
	require.Equal(t, int64(700), last.AccountID)
}

const geminiEmptyStopStream = "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":0,\"candidatesTokenCount\":0}}\n\n"

const geminiChatTestBody = `{"model":"gemini-3.8-flash","stream":%s,"messages":[{"role":"user","content":"hello"}]}`

func newGeminiEmptyTestContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	return c, rec
}

func TestGeminiForwardAsChatCompletions_EmptyStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiEmptyStopStream)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "true", 1))

	res, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

func TestGeminiForwardAsChatCompletions_EmptyNonStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, `{"candidates":[],"usageMetadata":{"promptTokenCount":0}}`)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "false", 1))

	res, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

func TestGeminiForwardAsChatCompletions_ToolCallOnlyStreamPassesThrough(t *testing.T) {
	stream := geminiSSE(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, stream)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "true", 1))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "lookup")
	require.Contains(t, rec.Body.String(), "tool_calls")
}

func TestGeminiForwardAsChatCompletions_MaxTokensStreamPassesThrough(t *testing.T) {
	stream := geminiSSE(`{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0,"thoughtsTokenCount":64}}`)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, stream)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "true", 1))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), `"finish_reason":"length"`)
}

func TestGeminiForwardAsChatCompletions_TextStreamPassesThrough(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiStreamWithUsage)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "true", 1))

	res, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.NoError(t, err)
	require.Equal(t, 7, res.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "Hello")
}

func TestGeminiForwardNative_EmptyStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiSSE(`{"candidates":[]}`, `{"usageMetadata":{"promptTokenCount":812}}`))
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse")

	res, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

func TestGeminiForwardNative_EmptyNonStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0}}`)
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:generateContent")

	res, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

func TestGeminiForwardNative_SafetyBlockPassesThrough(t *testing.T) {
	upstream := `{"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HARASSMENT","probability":"HIGH","blocked":true}]}],"usageMetadata":{"promptTokenCount":12}}`
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, upstream)
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:generateContent")

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, upstream, rec.Body.String())
}

func TestGeminiForwardNative_PromptBlockedStreamPassesThrough(t *testing.T) {
	stream := geminiSSE(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},"usageMetadata":{"promptTokenCount":12}}`)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, stream)
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse")

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "PROHIBITED_CONTENT")
}

func TestGeminiForwardNative_ImageOutputPassesThrough(t *testing.T) {
	stream := geminiSSE(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":0}}`)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, stream)
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash-image:streamGenerateContent?alt=sse")

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash-image", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "inlineData")
}

func TestGeminiForwardNative_CountTokensNotGated(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, `{"totalTokens":0}`)
	c, rec := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:countTokens")

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "countTokens", false, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.JSONEq(t, `{"totalTokens":0}`, rec.Body.String())
}

func TestGeminiForwardMessages_EmptyStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiEmptyStopStream)
	c, rec := newGeminiEmptyTestContext("/v1/messages")
	body := []byte(`{"model":"gemini-3.8-flash","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	res, err := svc.Forward(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

func TestGeminiForwardMessages_EmptyNonStreamFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, `{"candidates":[]}`)
	c, rec := newGeminiEmptyTestContext("/v1/messages")
	body := []byte(`{"model":"gemini-3.8-flash","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`)

	res, err := svc.Forward(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.Nil(t, res)
	requireGeminiEmptyFailover(t, c, rec, err)
}

// geminiEndlessReader 模拟迟迟不换行的上游：无限输出同一个字节，并记录被读走多少。
type geminiEndlessReader struct {
	b    byte
	read int
}

func (r *geminiEndlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	r.read += len(p)
	return len(p), nil
}

// 单个 data: 行超过预读上限且一直不换行时，预读必须在上限附近停下并放行，不能无界读取。
func TestGeminiEmptyResponsePeek_HugeLineWithoutNewlineIsBounded(t *testing.T) {
	const limit = 100 << 10
	prefix := `data: {"candidates":[{"content":{"parts":[{"text":"`
	src := &geminiEndlessReader{b: 'x'}
	body := io.NopCloser(io.MultiReader(strings.NewReader(prefix), src))

	replay, det, err := peekGeminiResponseForEmpty(body, true, limit)
	require.NoError(t, err)
	require.False(t, det.empty(), "an oversized line must pass through")
	require.LessOrEqual(t, len(prefix)+src.read, limit+2*geminiEmptyResponsePeekChunk, "peek must stop near the limit")

	got := make([]byte, limit+4096)
	_, readErr := io.ReadFull(replay, got)
	require.NoError(t, readErr)
	require.Equal(t, prefix, string(got[:len(prefix)]))
	require.Equal(t, strings.Repeat("x", len(got)-len(prefix)), string(got[len(prefix):]), "replay must continue the stream without gaps")
}

// 单行大于上限（例如大图片 inlineData）最终换行结束：放行，且重放字节完全一致。
func TestGeminiEmptyResponsePeek_HugeSingleLineReplaysExactly(t *testing.T) {
	const limit = 64 << 10
	stream := geminiSSE(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+strings.Repeat("A", 3*limit)+`"}}]},"finishReason":"STOP"}]}`) +
		geminiSSE(`{"candidates":[],"usageMetadata":{"candidatesTokenCount":1290}}`)

	replay, det, err := peekGeminiResponseForEmpty(io.NopCloser(strings.NewReader(stream)), true, limit)
	require.NoError(t, err)
	require.False(t, det.empty())
	got, readErr := io.ReadAll(replay)
	require.NoError(t, readErr)
	require.Equal(t, stream, string(got))
}

// 空回复被分成很小的片段到达（每次 Read 只给几个字节）也要能判定。
func TestGeminiEmptyResponsePeek_EmptyStreamInTinyReads(t *testing.T) {
	stream := geminiEmptyStopStream + geminiSSE(`{"usageMetadata":{"promptTokenCount":3}}`)
	_, det, err := peekGeminiResponseForEmpty(io.NopCloser(iotestOneByteReader{r: strings.NewReader(stream)}), true, 0)
	require.NoError(t, err)
	require.True(t, det.empty())
}

type iotestOneByteReader struct{ r io.Reader }

func (o iotestOneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
