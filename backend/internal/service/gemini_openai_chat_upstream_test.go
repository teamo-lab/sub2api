//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func openAIChatUpstreamAccount() *Account {
	return &Account{
		ID:       900,
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                  "llm-test-key",
			"base_url":                 "https://api.llmapi.example",
			"upstream_protocol":        "openai_chat",
			"openai_chat_model_prefix": "google-vertex/",
		},
	}
}

func TestIsGeminiOpenAIChatUpstream(t *testing.T) {
	require.True(t, openAIChatUpstreamAccount().IsGeminiOpenAIChatUpstream())
	require.False(t, geminiPoolModeAPIKeyAccount().IsGeminiOpenAIChatUpstream())
	acc := openAIChatUpstreamAccount()
	acc.Platform = PlatformOpenAI
	require.False(t, acc.IsGeminiOpenAIChatUpstream())
}

func TestParseGeminiModelActionPathAndChatURL(t *testing.T) {
	u, _ := url.Parse("https://api.llmapi.example/v1beta/models/gemini-4-neon:streamGenerateContent?alt=sse")
	model, action, ok := parseGeminiModelActionPath(u)
	require.True(t, ok)
	require.Equal(t, "gemini-4-neon", model)
	require.Equal(t, "streamGenerateContent", action)
	require.Equal(t, "https://api.llmapi.example/v1/chat/completions", openAIChatCompletionsURL(u))

	u2, _ := url.Parse("https://gw.example/api/v1/v1beta/models/m:generateContent")
	require.Equal(t, "https://gw.example/api/v1/chat/completions", openAIChatCompletionsURL(u2))

	_, _, ok = parseGeminiModelActionPath(&url.URL{Path: "/v1beta/models"})
	require.False(t, ok)
}

func TestConvertGeminiRequestToOpenAIChat_FullConversation(t *testing.T) {
	body := `{
	  "systemInstruction":{"parts":[{"text":"You are a pirate."},{"text":"Answer in Chinese."}]},
	  "contents":[
	    {"role":"user","parts":[{"text":"天气？"},{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]},
	    {"role":"model","parts":[{"text":"thinking...","thought":true},{"functionCall":{"name":"get_weather","args":{"city":"北京"}},"thoughtSignature":"SIG1"},{"functionCall":{"name":"get_weather","args":{"city":"上海"}}}]},
	    {"role":"user","parts":[
	      {"functionResponse":{"name":"get_weather","response":{"temp":12}}},
	      {"functionResponse":{"name":"get_weather","response":{"temp":15},"parts":[{"inlineData":{"mimeType":"image/png","data":"BBBB"}}]}},
	      {"text":"谢谢"}
	    ]}
	  ],
	  "tools":[{"functionDeclarations":[{"name":"get_weather","description":"weather","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}},"required":["city"]}}]},{"googleSearch":{}}],
	  "toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["get_weather"]}},
	  "generationConfig":{"temperature":0.2,"maxOutputTokens":512,"stopSequences":["END"],"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"a":{"type":"STRING"}}},"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}
	}`
	out, err := convertGeminiRequestToOpenAIChat([]byte(body), "google-vertex/gemini-4-neon", true)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))

	require.Equal(t, "google-vertex/gemini-4-neon", got["model"])
	require.Equal(t, true, got["stream"])
	require.Equal(t, map[string]any{"include_usage": true}, got["stream_options"])
	require.Equal(t, 0.2, got["temperature"])
	require.Equal(t, float64(512), got["max_tokens"])
	require.Equal(t, []any{"END"}, got["stop"])
	require.Equal(t, "low", got["reasoning_effort"])
	rf := got["response_format"].(map[string]any)
	require.Equal(t, "json_schema", rf["type"])
	schema := rf["json_schema"].(map[string]any)["schema"].(map[string]any)
	require.Equal(t, "object", schema["type"])
	require.Equal(t, map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, got["tool_choice"])

	tools := got["tools"].([]any)
	require.Len(t, tools, 1, "googleSearch 内置工具没有 OpenAI 写法，丢弃")
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	require.Equal(t, "string", fn["parameters"].(map[string]any)["properties"].(map[string]any)["city"].(map[string]any)["type"])

	msgs := got["messages"].([]any)
	require.Len(t, msgs, 6)
	require.Equal(t, map[string]any{"role": "system", "content": "You are a pirate.\nAnswer in Chinese."}, msgs[0])

	user := msgs[1].(map[string]any)
	require.Equal(t, "user", user["role"])
	parts := user["content"].([]any)
	require.Equal(t, "data:image/png;base64,AAAA", parts[1].(map[string]any)["image_url"].(map[string]any)["url"])

	asst := msgs[2].(map[string]any)
	require.Nil(t, asst["content"], "思考摘要不回传")
	calls := asst["tool_calls"].([]any)
	require.Len(t, calls, 2)
	c1 := calls[0].(map[string]any)
	require.Equal(t, "call_1", c1["id"])
	require.JSONEq(t, `{"city":"北京"}`, c1["function"].(map[string]any)["arguments"].(string))
	require.Equal(t, "SIG1", c1["extra_content"].(map[string]any)["google"].(map[string]any)["thought_signature"])
	require.Nil(t, calls[1].(map[string]any)["extra_content"])

	t1 := msgs[3].(map[string]any)
	t2 := msgs[4].(map[string]any)
	require.Equal(t, "tool", t1["role"])
	require.Equal(t, "call_1", t1["tool_call_id"], "functionResponse 按名字顺序对上 functionCall 的 id")
	require.Equal(t, "call_2", t2["tool_call_id"])
	require.JSONEq(t, `{"temp":15}`, t2["content"].(string))

	last := msgs[5].(map[string]any)
	require.Equal(t, "user", last["role"])
	lp := last["content"].([]any)
	require.Equal(t, "[images returned by the tool call above]", lp[0].(map[string]any)["text"])
	require.Equal(t, "data:image/png;base64,BBBB", lp[1].(map[string]any)["image_url"].(map[string]any)["url"], "工具结果里的图片挪到 user 消息")
	require.Equal(t, "谢谢", lp[2].(map[string]any)["text"])
}

func TestConvertGeminiRequestToOpenAIChat_TextOnlyUsesStringContent(t *testing.T) {
	out, err := convertGeminiRequestToOpenAIChat([]byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":20000}}}`), "m", false)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, []any{map[string]any{"role": "user", "content": "hi"}}, got["messages"])
	require.Equal(t, "high", got["reasoning_effort"])
	_, hasStream := got["stream"]
	require.False(t, hasStream)

	_, err = convertGeminiRequestToOpenAIChat([]byte(`{"contents":[]}`), "m", false)
	require.Error(t, err)
}

func TestConvertOpenAIChatCompletionToGemini(t *testing.T) {
	raw := `{"id":"r1","model":"google-vertex/gemini-4-neon","choices":[{"finish_reason":"tool_calls","message":{"content":"ok","reasoning":"why","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{\"a\":1}"},"extra_content":{"google":{"thought_signature":"SIG"}}}]}}],
	  "usage":{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":20}}}`
	out, err := convertOpenAIChatCompletionToGemini([]byte(raw), "gemini-4-neon", true)
	require.NoError(t, err)
	require.JSONEq(t, `{
	  "candidates":[{"content":{"role":"model","parts":[{"text":"why","thought":true},{"text":"ok"},{"functionCall":{"name":"f","args":{"a":1}},"thoughtSignature":"SIG"}]},"finishReason":"STOP","index":0}],
	  "modelVersion":"gemini-4-neon","responseId":"r1",
	  "usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":20,"cachedContentTokenCount":40,"totalTokenCount":130}
	}`, string(out))

	u := extractGeminiUsage(out)
	require.Equal(t, 60, u.InputTokens)
	require.Equal(t, 40, u.CacheReadInputTokens)
	require.Equal(t, 30, u.OutputTokens)
}

const llmapiStreamFixture = "data: {\"id\":\"s1\",\"model\":\"google-vertex/gemini-4-hydrogen\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"reasoning\":\"thinking\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: {\"id\":\"s1\",\"model\":\"google-vertex/gemini-4-hydrogen\",\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"t0\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"},\"extra_content\":{\"google\":{\"thought_signature\":\"SIG\"}}}]},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Beijing\\\"}\"}}]},\"finish_reason\":null,\"index\":0}]}\n\n" +
	"data: {\"usage\":{\"prompt_tokens\":81,\"completion_tokens\":70,\"total_tokens\":151,\"reasoning_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":30}},\"id\":\"s1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\",\"index\":0}]}\n\n" +
	"data: [DONE]\n\n"

func readGeminiSSEChunks(t *testing.T, r io.Reader) []map[string]any {
	t.Helper()
	all, err := io.ReadAll(r)
	require.NoError(t, err)
	var chunks []map[string]any
	for _, line := range strings.Split(string(all), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m))
		chunks = append(chunks, m)
	}
	return chunks
}

func TestOpenAIChatSSEToGemini(t *testing.T) {
	rc := newOpenAIChatToGeminiSSE(io.NopCloser(strings.NewReader(llmapiStreamFixture)), "gemini-4-hydrogen", true)
	chunks := readGeminiSSEChunks(t, rc)
	require.NoError(t, rc.Close())
	require.Len(t, chunks, 3)

	part := func(c map[string]any) map[string]any {
		return c["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)
	}
	require.Equal(t, map[string]any{"text": "thinking", "thought": true}, part(chunks[0]))
	require.Equal(t, map[string]any{"text": "Hello"}, part(chunks[1]))
	require.Equal(t, "gemini-4-hydrogen", chunks[1]["modelVersion"])
	require.Equal(t, "s1", chunks[1]["responseId"])

	final := chunks[2]
	cand := final["candidates"].([]any)[0].(map[string]any)
	require.Equal(t, "STOP", cand["finishReason"])
	fc := part(final)
	require.Equal(t, "SIG", fc["thoughtSignature"])
	require.Equal(t, map[string]any{"name": "get_weather", "args": map[string]any{"city": "Beijing"}}, fc["functionCall"])
	usage := final["usageMetadata"].(map[string]any)
	require.Equal(t, float64(81), usage["promptTokenCount"])
	require.Equal(t, float64(40), usage["candidatesTokenCount"])
	require.Equal(t, float64(30), usage["thoughtsTokenCount"])
}

func TestConvertOpenAIChatCompletionToGemini_DropsThoughtsUnlessRequested(t *testing.T) {
	raw := `{"choices":[{"finish_reason":"stop","message":{"content":"ok","reasoning":"why"}}]}`
	out, err := convertOpenAIChatCompletionToGemini([]byte(raw), "m", false)
	require.NoError(t, err)
	require.NotContains(t, string(out), "why")
	require.True(t, geminiRequestIncludesThoughts([]byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`)))
	require.False(t, geminiRequestIncludesThoughts([]byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`)))
}

func TestOpenAIChatSSEToGemini_TruncatedStreamHasNoFakeFinish(t *testing.T) {
	src := "data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"content\":\"Hel\"},\"index\":0}]}\n\n"
	chunks := readGeminiSSEChunks(t, newOpenAIChatToGeminiSSE(io.NopCloser(strings.NewReader(src)), "m", false))
	require.Len(t, chunks, 1)
	_, has := chunks[0]["candidates"].([]any)[0].(map[string]any)["finishReason"]
	require.False(t, has)
}

func TestOpenAIErrorBodyToGemini(t *testing.T) {
	out := openAIErrorBodyToGemini(402, []byte(`{"error":{"message":"Insufficient credits","type":"insufficient_quota","code":"insufficient_quota"}}`))
	require.JSONEq(t, `{"error":{"code":402,"message":"Insufficient credits (insufficient_quota)","status":"PERMISSION_DENIED"}}`, string(out))
	out = openAIErrorBodyToGemini(503, []byte("upstream down"))
	require.JSONEq(t, `{"error":{"code":503,"message":"upstream down","status":"UNAVAILABLE"}}`, string(out))
}

func newOpenAIChatUpstreamService(status int, contentType, body string) (*GeminiMessagesCompatService, *geminiCompatHTTPUpstreamStub) {
	svc, stub := newGeminiSkippedWriteService(status, body)
	stub.response.Header.Set("Content-Type", contentType)
	stub.response.Header.Set("X-Request-Id", "llm-req-1")
	stub.response.Header.Set("X-Ratelimit-Limit-Requests", "30")
	return svc, stub
}

func requireOpenAIChatUpstreamRequest(t *testing.T, stub *geminiCompatHTTPUpstreamStub, wantModel string, wantStream bool) map[string]any {
	t.Helper()
	require.NotNil(t, stub.lastReq)
	require.Equal(t, "https://api.llmapi.example/v1/chat/completions", stub.lastReq.URL.String())
	require.Equal(t, "Bearer llm-test-key", stub.lastReq.Header.Get("Authorization"))
	require.Empty(t, stub.lastReq.Header.Get("x-goog-api-key"))
	sent, err := io.ReadAll(stub.lastReq.Body)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(sent, &got))
	require.Equal(t, wantModel, got["model"])
	if wantStream {
		require.Equal(t, true, got["stream"])
	}
	return got
}

func TestGeminiForwardNative_OpenAIChatUpstreamNonStream(t *testing.T) {
	svc, stub := newOpenAIChatUpstreamService(http.StatusOK, "application/json",
		`{"id":"r1","model":"google-vertex/gemini-4-neon","choices":[{"finish_reason":"stop","message":{"content":"pong"}}],"usage":{"prompt_tokens":37,"completion_tokens":19,"total_tokens":56,"completion_tokens_details":{"reasoning_tokens":17}}}`)
	c, rec := newGeminiNativeTestContext(t)

	res, err := svc.ForwardNative(context.Background(), c, openAIChatUpstreamAccount(), "gemini-4-neon", "generateContent", false,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`))
	require.NoError(t, err)
	requireOpenAIChatUpstreamRequest(t, stub, "google-vertex/gemini-4-neon", false)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"text":"pong"`)
	require.Empty(t, rec.Header().Get("X-Ratelimit-Limit-Requests"), "上游网关自己的限流头不透给客户端")
	require.Equal(t, 37, res.Usage.InputTokens)
	require.Equal(t, 19, res.Usage.OutputTokens)
}

func TestGeminiForwardNative_OpenAIChatUpstreamStream(t *testing.T) {
	svc, stub := newOpenAIChatUpstreamService(http.StatusOK, "text/event-stream", llmapiStreamFixture)
	c, rec := newGeminiNativeTestContext(t)

	res, err := svc.ForwardNative(context.Background(), c, openAIChatUpstreamAccount(), "gemini-4-hydrogen", "streamGenerateContent", true,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"weather"}]}],"tools":[{"functionDeclarations":[{"name":"get_weather"}]}]}`))
	require.NoError(t, err)
	requireOpenAIChatUpstreamRequest(t, stub, "google-vertex/gemini-4-hydrogen", true)
	chunks := readGeminiSSEChunks(t, bytes.NewReader(rec.Body.Bytes()))
	require.Len(t, chunks, 2, "客户端没开 includeThoughts：思考摘要不下发")
	require.NotContains(t, rec.Body.String(), `"thought":true`)
	require.Contains(t, rec.Body.String(), `"thoughtSignature":"SIG"`)
	require.Equal(t, 81, res.Usage.InputTokens)
	require.Equal(t, 70, res.Usage.OutputTokens)
}

func TestGeminiForwardNative_OpenAIChatUpstream402FailsOver(t *testing.T) {
	svc, _ := newOpenAIChatUpstreamService(http.StatusPaymentRequired, "application/json",
		`{"error":{"message":"Insufficient credits","type":"insufficient_quota","code":"insufficient_quota"}}`)
	c, _ := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, openAIChatUpstreamAccount(), "gemini-4-neon", "generateContent", false,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
}

func TestGeminiForwardNative_OpenAIChatUpstreamCountTokensIsLocal(t *testing.T) {
	svc, stub := newOpenAIChatUpstreamService(http.StatusOK, "application/json", `{}`)
	c, rec := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, openAIChatUpstreamAccount(), "gemini-4-neon", "countTokens", false,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`))
	require.NoError(t, err)
	require.Equal(t, 0, stub.calls)
	require.Contains(t, rec.Body.String(), "totalTokens")
}

func TestGeminiForwardAsChatCompletions_OpenAIChatUpstream(t *testing.T) {
	svc, stub := newOpenAIChatUpstreamService(http.StatusOK, "application/json",
		`{"id":"r1","model":"google-vertex/gemini-3.8-flash","choices":[{"finish_reason":"stop","message":{"content":"hi there"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	c, rec := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "false", 1))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, openAIChatUpstreamAccount(), body)
	require.NoError(t, err)
	requireOpenAIChatUpstreamRequest(t, stub, "google-vertex/gemini-3.8-flash", false)
	require.Contains(t, rec.Body.String(), "hi there")
}
