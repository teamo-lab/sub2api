package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/tidwall/gjson"
	"strings"
)

func profileOpenAIJSONMarshal(ctx context.Context, value any) ([]byte, error) {
	defer requestprofile.Start(ctx, "json_serialize")()
	return marshalOpenAIUpstreamJSON(value)
}
func profileOpenAIPatches(ctx context.Context, view openAIRequestView) ([]byte, error) {
	defer requestprofile.Start(ctx, "json_patch")()
	return view.ApplyPatches()
}

// profileOpenAIReasoningObserver uses existing semantic SSE boundaries only.
// No text, reasoning content, item IDs, or tool arguments are recorded.
func profileOpenAIReasoningObserver(ctx context.Context) func([]byte, string) {
	if requestprofile.From(ctx) == nil {
		return func([]byte, string) {}
	}
	observe := requestprofile.ReasoningObserver(ctx)
	return func(data []byte, eventType string) {
		itemType := ""
		if eventType == "response.output_item.added" || eventType == "response.output_item.done" {
			itemType = gjson.GetBytes(data, "item.type").String()
		}
		reasoning, boundary := profileReasoningBoundary(eventType, itemType)
		observe(reasoning, boundary)
	}
}

func profileReasoningBoundary(eventType, itemType string) (reasoning, boundary bool) {
	if strings.HasPrefix(eventType, "response.reasoning") {
		return true, strings.HasSuffix(eventType, ".done")
	}
	if eventType == "response.output_item.added" || eventType == "response.output_item.done" {
		if itemType == "reasoning" {
			return true, eventType == "response.output_item.done"
		}
		return false, itemType != ""
	}
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error", "[DONE]":
		return false, true
	}
	for _, prefix := range []string{"response.output_text.", "response.function_call_arguments.", "response.custom_tool_call_input.", "response.audio.", "response.audio_transcript."} {
		if strings.HasPrefix(eventType, prefix) {
			return false, true
		}
	}
	return false, false
}
