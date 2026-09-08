package service

import "github.com/tidwall/gjson"

func nativeFirstDeliveryNeedsFlush(relayEnabled, committed bool, upstreamTTFTKnown, visible, release bool) bool {
	if !visible || !release {
		return false
	}
	if relayEnabled {
		return !committed
	}
	return !upstreamTTFTKnown
}

// nativeRelayHoldableReasoning is a delivery policy over the existing native
// Responses parser. It does not classify errors or unknown protocol families.
// Opaque state and unknown kinds retain the existing conservative boundary.
func nativeRelayHoldableReasoning(data, eventType string) bool {
	if !gjson.Valid(data) {
		return false
	}
	value := gjson.Parse(data)
	switch eventType {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return value.Get("delta").Type == gjson.String
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		return value.Get("text").Type == gjson.String
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		part := value.Get("part")
		return part.Get("type").String() == "summary_text" && part.Get("text").Type == gjson.String
	case "response.output_item.added", "response.output_item.done":
		item := value.Get("item")
		if item.Get("type").String() != "reasoning" || item.Get("encrypted_content").String() != "" {
			return false
		}
		summary := item.Get("summary")
		if !summary.IsArray() {
			return !summary.Exists() || summary.Type == gjson.Null
		}
		for _, part := range summary.Array() {
			if part.Get("type").String() != "summary_text" || part.Get("text").Type != gjson.String {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Pure text has no executable side effect before a writer is called. Keep the
// set explicit: unfamiliar deltas, opaque state and tool events are fenced.
func nativeRelayNonExecutableText(data, eventType string) bool {
	if !gjson.Valid(data) {
		return false
	}
	value := gjson.Parse(data)
	textPart := func(part gjson.Result) bool {
		switch part.Get("type").String() {
		case "output_text":
			return part.Get("text").Type == gjson.String
		case "refusal":
			return part.Get("refusal").Type == gjson.String
		default:
			return false
		}
	}
	switch eventType {
	case "response.output_text.delta":
		return value.Get("delta").Type == gjson.String
	case "response.output_text.done":
		return value.Get("text").Type == gjson.String
	case "response.content_part.added", "response.content_part.done":
		return textPart(value.Get("part"))
	case "response.output_item.added", "response.output_item.done":
		item := value.Get("item")
		if item.Get("type").String() != "message" || !item.Get("content").IsArray() {
			return false
		}
		for _, part := range item.Get("content").Array() {
			if !textPart(part) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
