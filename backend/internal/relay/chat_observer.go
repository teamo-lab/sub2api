package relay

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrMalformedFrame = errors.New("invalid Chat Completions stream frame")
var ErrIncompleteTool = errors.New("Chat Completions stream ended with an unfinished tool call")
var ErrEmptyResponse = errors.New("Chat Completions stream ended without output or usage")

// ChatObserver reads execution facts only. It neither converts protocols nor
// classifies supplier error codes for retry. Usage alone is not a terminal: some
// suppliers attach running usage to ordinary deltas.
type ChatObserver struct {
	Ready        bool
	Terminal     bool
	Failure      json.RawMessage
	failed       bool
	pendingTools map[int]bool
	openChoices  map[int]bool
	hasOutput    bool
	hasUsage     bool
}

func (o *ChatObserver) Observe(payload string) error {
	if o.failed {
		return ErrMalformedFrame
	}
	payload = strings.TrimSpace(payload)
	if payload == "[DONE]" {
		if len(o.pendingTools) != 0 {
			o.failed = true
			return ErrIncompleteTool
		}
		if !o.hasOutput && !o.hasUsage {
			o.failed = true
			return ErrEmptyResponse
		}
		o.Terminal = true
		o.Ready = true
		o.openChoices = nil
		return nil
	}
	if payload == "" || strings.EqualFold(payload, "SSE-Keep-Alive") {
		return nil
	}
	var frame struct {
		Type         string          `json:"type"`
		Error        json.RawMessage `json:"error"`
		Keepalive    bool            `json:"SSE-Keep-Alive"`
		KeepaliveAlt bool            `json:"sse_keep_alive"`
		Choices      []struct {
			Index int `json:"index"`
			Delta struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				Reasoning        string            `json:"reasoning"`
				Refusal          string            `json:"refusal"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
				FunctionCall     json.RawMessage   `json:"function_call"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		o.failed = true
		return ErrMalformedFrame
	}
	if frame.Keepalive || frame.KeepaliveAlt {
		return nil
	}
	var usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	}
	if presentJSON(frame.Usage) && json.Unmarshal(frame.Usage, &usage) == nil && (usage.Prompt > 0 || usage.Completion > 0 || usage.Total > 0) {
		o.hasUsage = true
	}
	if frame.Type == "error" || presentJSON(frame.Error) {
		o.failed = true
		o.Failure = append(json.RawMessage(nil), payload...)
		return nil // The caller applies its existing error policy.
	}
	if frame.Choices == nil && !presentJSON(frame.Usage) {
		o.failed = true
		return ErrMalformedFrame
	}
	for _, choice := range frame.Choices {
		if o.openChoices == nil {
			o.openChoices = make(map[int]bool)
		}
		if _, exists := o.openChoices[choice.Index]; !exists && len(o.openChoices) >= 128 {
			o.failed = true
			return ErrMalformedFrame
		}
		o.openChoices[choice.Index] = true
		delta := choice.Delta
		// Once reasoning is sent it is also a commit, regardless of visibility
		// in a particular client UI. Never restart another response behind it.
		if delta.Content != "" || delta.ReasoningContent != "" || delta.Reasoning != "" || delta.Refusal != "" {
			o.hasOutput = true
			o.Ready = true
		}
		if len(delta.ToolCalls) > 0 || presentJSON(delta.FunctionCall) {
			o.hasOutput = true
			if o.pendingTools == nil {
				o.pendingTools = make(map[int]bool)
			}
			o.pendingTools[choice.Index] = true
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			o.openChoices[choice.Index] = false
			delete(o.pendingTools, choice.Index)
			o.Terminal = true
			if len(o.pendingTools) == 0 && (o.hasOutput || o.hasUsage) {
				o.Ready = true
			}
		}
	}
	if o.Complete() {
		o.Ready = true
	}
	return nil
}

func (o *ChatObserver) Complete() bool {
	if !o.Terminal || o.failed || len(o.pendingTools) != 0 || (!o.hasOutput && !o.hasUsage) {
		return false
	}
	for _, open := range o.openChoices {
		if open {
			return false
		}
	}
	return true
}
func presentJSON(p json.RawMessage) bool { return len(p) > 0 && string(p) != "null" }
