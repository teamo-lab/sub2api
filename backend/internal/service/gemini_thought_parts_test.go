package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 上游（如 AnyRoute cc.anyroute.io）不管请求有没有要 includeThoughts，都会在回包里带
// "thought": true 的思考摘要片段。兼容转换以前把它们当普通 text 输出，思考过程就进了
// choices[0].message.content。下面这组用例固定：thought 片段走 thinking 块 →
// reasoning_content，正文只有真正的回答。

const (
	geminiThoughtA = "Greeting in three words\n"
	geminiThoughtB = "Selected \"Hello there friend\""
	geminiAnswer   = "Hello there friend"
)

func geminiThoughtThenAnswerResp() map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": geminiThoughtA + geminiThoughtB, "thought": true},
				map[string]any{"text": geminiAnswer},
			}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 3, "thoughtsTokenCount": 7},
	}
}

// 流式回包：两段 thought（增量）+ 一段正文，正文那一帧带 finishReason/usage。
const geminiThoughtThenAnswerSSE = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Greeting in three words\n","thought":true}]}}]}` + "\n\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Selected \"Hello there friend\"","thought":true}]}}]}` + "\n\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello there friend"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":7}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestConvertGeminiToClaudeMessage_ThoughtPartBecomesThinkingBlock(t *testing.T) {
	geminiResp := geminiThoughtThenAnswerResp()
	raw, err := json.Marshal(geminiResp)
	require.NoError(t, err)

	claudeResp, _ := convertGeminiToClaudeMessage(geminiResp, "gemini-test", raw, false)
	blocks, ok := claudeResp["content"].([]any)
	require.True(t, ok)
	require.Equal(t, []any{
		map[string]any{"type": "thinking", "thinking": geminiThoughtA + geminiThoughtB},
		map[string]any{"type": "text", "text": geminiAnswer},
	}, blocks)
}

func TestGeminiResponseToChatCompletions_ThoughtGoesToReasoningContent(t *testing.T) {
	geminiResp := geminiThoughtThenAnswerResp()
	raw, err := json.Marshal(geminiResp)
	require.NoError(t, err)

	got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", raw, nil)
	require.NoError(t, err)
	require.Len(t, got.Choices, 1)

	msg := got.Choices[0].Message
	var content string
	require.NoError(t, json.Unmarshal(msg.Content, &content))
	require.Equal(t, geminiAnswer, content)
	require.Equal(t, geminiThoughtA+geminiThoughtB, msg.ReasoningContent)
	require.Equal(t, "stop", got.Choices[0].FinishReason)
}

// 非流式请求、上游走流式（useUpstreamStream）时由 collectGeminiSSE 聚合：
// thought 和正文要分开合并，不能把思考拼进正文 part。
func TestCollectGeminiSSE_KeepsThoughtSeparateFromAnswer(t *testing.T) {
	collected, usage, err := collectGeminiSSE(strings.NewReader(geminiThoughtThenAnswerSSE), false)
	require.NoError(t, err)
	require.NotNil(t, usage)

	parts := extractGeminiParts(collected)
	require.Len(t, parts, 2)
	require.Equal(t, true, parts[0]["thought"])
	require.Equal(t, geminiThoughtA+geminiThoughtB, parts[0]["text"])
	_, answerIsThought := parts[1]["thought"]
	require.False(t, answerIsThought)
	require.Equal(t, geminiAnswer, parts[1]["text"])

	raw, err := json.Marshal(collected)
	require.NoError(t, err)
	got, _, err := geminiResponseToChatCompletions(collected, "gemini-test", raw, usage)
	require.NoError(t, err)
	var content string
	require.NoError(t, json.Unmarshal(got.Choices[0].Message.Content, &content))
	require.Equal(t, geminiAnswer, content)
	require.Equal(t, geminiThoughtA+geminiThoughtB, got.Choices[0].Message.ReasoningContent)
}

// 最后一帧同时带 thought 和正文（旧逻辑会把合并后的正文写进第一个 text part，
// 也就是那个 thought part，正文被标成 thought、思考被丢掉或重复）。
func TestMergeCollectedTextParts_LastChunkHasThoughtAndAnswer(t *testing.T) {
	last := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "t2", "thought": true, "thoughtSignature": "sig"},
				map[string]any{"text": "a2"},
			}},
		}},
	}
	merged := mergeCollectedTextParts(last, []string{"a1", "a2"}, []string{"t1", "t2"})
	parts := extractGeminiParts(merged)
	require.Len(t, parts, 2)
	require.Equal(t, map[string]any{"text": "t1t2", "thought": true, "thoughtSignature": "sig"}, parts[0])
	require.Equal(t, map[string]any{"text": "a1a2"}, parts[1])
}

func TestGeminiMessagesHandleStreamingResponse_ThoughtPartsBecomeThinkingDeltas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(geminiThoughtThenAnswerSSE)),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleStreamingResponse(c, resp, time.Now(), "gemini-test")
	require.NoError(t, err)

	var blockTypes []string
	var thinking, text strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock *struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta *struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &ev))
		switch ev.Type {
		case "content_block_start":
			blockTypes = append(blockTypes, ev.ContentBlock.Type)
		case "content_block_delta":
			switch ev.Delta.Type {
			case "thinking_delta":
				thinking.WriteString(ev.Delta.Thinking)
			case "text_delta":
				text.WriteString(ev.Delta.Text)
			}
		}
	}

	require.Equal(t, []string{"thinking", "text"}, blockTypes)
	require.Equal(t, geminiThoughtA+geminiThoughtB, thinking.String())
	require.Equal(t, geminiAnswer, text.String())
}

func TestGeminiChatCompletionsStreaming_ThoughtPartsGoToReasoningContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(geminiThoughtThenAnswerSSE)),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleChatCompletionsStreamingResponseFromGemini(c, resp, time.Now(), "gemini-test", false, false)
	require.NoError(t, err)

	var content, reasoning strings.Builder
	sawDone := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent *string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk))
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
			if ch.Delta.ReasoningContent != nil {
				reasoning.WriteString(*ch.Delta.ReasoningContent)
			}
		}
	}

	require.True(t, sawDone)
	require.Equal(t, geminiAnswer, content.String())
	require.Equal(t, geminiThoughtA+geminiThoughtB, reasoning.String())
}

// 客户端多轮时会把我们给的 thinking 块原样带回来：不能再当 unknown block 序列化成
// JSON 文本塞进 Gemini 历史；只有 thinking 的 assistant 轮整轮跳过，避免空 parts 400。
func TestConvertClaudeMessagesToGeminiContents_DropsThinkingBlocks(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": geminiThoughtA},
			map[string]any{"type": "text", "text": geminiAnswer},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "redacted_thinking", "data": "xxx"},
		}},
		map[string]any{"role": "user", "content": "again"},
	}
	out, err := convertClaudeMessagesToGeminiContents(messages, map[string]string{})
	require.NoError(t, err)
	require.Len(t, out, 3)
	require.Equal(t, map[string]any{
		"role":  "model",
		"parts": []any{map[string]any{"text": geminiAnswer}},
	}, out[1])
	b, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "thinking")
}
