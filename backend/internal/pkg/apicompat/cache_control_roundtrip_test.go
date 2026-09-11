package apicompat

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCacheControlRoundTripPreservesMessageBoundaries(t *testing.T) {
	for _, role := range []string{"user", "system", "developer", "assistant"} {
		t.Run(role, func(t *testing.T) {
			kind := "input_text"
			if role == "assistant" {
				kind = "output_text"
			}
			raw := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","prompt_cache_key":"stable-key","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","input":[{"type":"message","role":%q,"content":[{"type":%q,"text":"stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":%q,"text":"changing suffix"}]}]}`, role, kind, kind))
			var req ResponsesRequest
			require.NoError(t, json.Unmarshal(raw, &req))
			chat, err := ResponsesToChatCompletionsRequest(&req)
			require.NoError(t, err)
			back, err := ChatCompletionsToResponses(chat)
			require.NoError(t, err)
			encoded, err := json.Marshal(back)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(encoded, &got))
			require.Equal(t, map[string]any{"mode": "explicit", "ttl": "30m"}, got["prompt_cache_options"])
			require.Equal(t, "stable-key", got["prompt_cache_key"])
			require.Equal(t, "24h", got["prompt_cache_retention"])
			items := got["input"].([]any)
			require.Len(t, items, 1)
			msg := items[0].(map[string]any)
			require.Equal(t, role, msg["role"])
			parts, ok := msg["content"].([]any)
			require.True(t, ok, "marked blocks must not flatten")
			require.Len(t, parts, 2)
			require.Equal(t, map[string]any{"mode": "explicit"}, parts[0].(map[string]any)["prompt_cache_breakpoint"])
			require.Equal(t, "stable prefix", parts[0].(map[string]any)["text"])
			require.Equal(t, "changing suffix", parts[1].(map[string]any)["text"])
		})
	}
}

func TestCacheControlRoundTripPreservesToolResultBoundary(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","prompt_cache_options":{"mode":"explicit"},"input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"stable result","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"dynamic result"}]}]}`)
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	chat, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	back, err := ChatCompletionsToResponses(chat)
	require.NoError(t, err)
	encoded, err := json.Marshal(back)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"prompt_cache_breakpoint":{"mode":"explicit"}`)
	require.Contains(t, string(encoded), `"call_id":"call_1"`)
}

func TestCacheControlRoundTripPreservesMixedContent(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_image","image_url":"https://example.com/image.png","detail":"high"},{"type":"input_file","file_id":"file_test"}]}]}`)
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	chat, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	back, err := ChatCompletionsToResponses(chat)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(back.Input, &items))
	var parts []map[string]any
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 3)
	require.Equal(t, map[string]any{"mode": "explicit"}, parts[0]["prompt_cache_breakpoint"])
	require.Equal(t, "https://example.com/image.png", parts[1]["image_url"])
	require.Equal(t, "high", parts[1]["detail"])
	require.Equal(t, "file_test", parts[2]["file_id"])
}

func TestCacheControlStructuredToolOutputSurvivesSerialization(t *testing.T) {
	raw := []byte(`{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]}`)
	var item ResponsesInputItem
	require.NoError(t, json.Unmarshal(raw, &item))
	encoded, err := json.Marshal(item)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(encoded))
}

func TestCacheControlRejectsUnrepresentableToolMedia(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-5.6-sol","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{}},{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`), &req))
	_, err := ResponsesToChatCompletionsRequest(&req)
	require.ErrorContains(t, err, "Responses-capable upstream")
}

func TestCacheControlSingleBlockAndAssistantReasoning(t *testing.T) {
	for _, role := range []string{"user", "system", "developer", "assistant"} {
		t.Run(role, func(t *testing.T) {
			msg := ChatMessage{Role: role, Content: json.RawMessage(`{"type":"text","text":"prefix","prompt_cache_breakpoint":{}}`)}
			if role == "assistant" {
				msg.ReasoningContent = "reasoning"
			}
			items, err := chatMessageToResponsesItems(msg)
			require.NoError(t, err)
			require.Len(t, items, 1)
			var parts []map[string]any
			require.NoError(t, json.Unmarshal(items[0].Content, &parts))
			last := parts[len(parts)-1]
			require.Equal(t, "prefix", last["text"])
			require.Equal(t, map[string]any{}, last["prompt_cache_breakpoint"])
			if role == "assistant" {
				require.Len(t, parts, 2)
				require.Equal(t, "<thinking>reasoning</thinking>\n", parts[0]["text"])
			}
		})
	}
}
