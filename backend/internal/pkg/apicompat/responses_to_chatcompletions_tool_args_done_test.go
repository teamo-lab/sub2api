package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	toolArgsRunCommands = `{"commands":["git status --short","git log --oneline -5"]}`
	toolArgsSearch      = `{"queries":["ResponsesEventToChatChunks"],"path":"backend"}`
)

type assembledChatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// runChatToolStream 把事件序列喂给流式转换器，按 tool_calls index 拼出客户端看到的调用。
func runChatToolStream(t *testing.T, events []ResponsesStreamEvent) (map[int]*assembledChatToolCall, string) {
	t.Helper()
	state := NewResponsesEventToChatState()
	state.Model = "gpt-6-sol"
	calls := map[int]*assembledChatToolCall{}
	finish := ""
	for i := range events {
		for _, chunk := range ResponsesEventToChatChunks(&events[i], state) {
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil {
					finish = *choice.FinishReason
				}
				for _, tc := range choice.Delta.ToolCalls {
					require.NotNil(t, tc.Index)
					call := calls[*tc.Index]
					if call == nil {
						call = &assembledChatToolCall{}
						calls[*tc.Index] = call
					}
					if tc.ID != "" {
						require.Empty(t, call.ID, "call id sent twice for index %d", *tc.Index)
						call.ID = tc.ID
					}
					call.Name += tc.Function.Name
					call.Arguments += tc.Function.Arguments
				}
			}
		}
	}
	return calls, finish
}

func toolAdded(outputIndex int, itemID, callID, name string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: outputIndex,
		Item:        &ResponsesOutput{Type: "function_call", ID: itemID, CallID: callID, Name: name},
	}
}

func toolArgsDelta(outputIndex int, itemID, delta string) ResponsesStreamEvent {
	return ResponsesStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: outputIndex, ItemID: itemID, Delta: delta}
}

func toolArgsDone(outputIndex int, itemID, args string) ResponsesStreamEvent {
	return ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: outputIndex, ItemID: itemID, Arguments: args}
}

func toolItemDone(outputIndex int, itemID, callID, name, args string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: outputIndex,
		Item:        &ResponsesOutput{Type: "function_call", ID: itemID, CallID: callID, Name: name, Arguments: args, Status: "completed"},
	}
}

func completedEvent() ResponsesStreamEvent {
	return ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed"}}
}

func createdEvent() ResponsesStreamEvent {
	return ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: "resp_1", Model: "gpt-6-sol"}}
}

func assertChatToolCall(t *testing.T, call *assembledChatToolCall, id, name, args string) {
	t.Helper()
	require.NotNil(t, call)
	assert.Equal(t, id, call.ID)
	assert.Equal(t, name, call.Name)
	assert.Equal(t, args, call.Arguments)
	assert.True(t, json.Valid([]byte(call.Arguments)), "arguments must be valid JSON: %q", call.Arguments)
}

// 两个并行调用先后 added，参数只在 function_call_arguments.done / output_item.done 里给出，
// 没有任何增量：客户端必须拿到完整参数。
func TestResponsesEventToChatChunks_ToolArgsOnlyInDoneEvents(t *testing.T) {
	calls, finish := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		toolArgsDone(1, "fc_b", toolArgsSearch),
		toolItemDone(1, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	require.Len(t, calls, 2)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
	assertChatToolCall(t, calls[1], "call_b", "search_codebase", toolArgsSearch)
	assert.Equal(t, "tool_calls", finish)
}

// 只有 output_item.done 带参数（没有 function_call_arguments.* 事件）。
func TestResponsesEventToChatChunks_ToolArgsOnlyInOutputItemDone(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		toolItemDone(1, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	require.Len(t, calls, 2)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
	assertChatToolCall(t, calls[1], "call_b", "search_codebase", toolArgsSearch)
}

// 官方顺序：增量发完整参数，随后 done 再带一次完整参数——不能重复发。
func TestResponsesEventToChatChunks_ToolArgsDeltaThenDoneNoDuplicate(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(1, "fc_a", "call_a", "run_commands"),
		toolArgsDelta(1, "fc_a", `{"commands":["git status`),
		toolArgsDelta(1, "fc_a", ` --short","git log --oneline -5"]}`),
		toolArgsDone(1, "fc_a", toolArgsRunCommands),
		toolItemDone(1, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		toolAdded(2, "fc_b", "call_b", "search_codebase"),
		toolArgsDelta(2, "fc_b", toolArgsSearch),
		toolArgsDone(2, "fc_b", toolArgsSearch),
		toolItemDone(2, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	require.Len(t, calls, 2)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
	assertChatToolCall(t, calls[1], "call_b", "search_codebase", toolArgsSearch)
}

// 增量只给了前半段，done 带完整参数：只补发缺的后半段。
func TestResponsesEventToChatChunks_ToolArgsPartialDeltaCompletedByDone(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolArgsDelta(0, "fc_a", `{"commands":["git`),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		completedEvent(),
	})
	require.Len(t, calls, 1)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
}

// 上游没给 output_index（都按 0 解析）：增量和 done 按 item_id / call_id 对到调用上。
func TestResponsesEventToChatChunks_ToolArgsMissingOutputIndex(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(0, "fc_b", "call_b", "search_codebase"),
		toolArgsDelta(0, "fc_a", `{"commands":`),
		toolArgsDelta(0, "fc_b", `{"queries":`),
		// 不带任何标识、又有两个未结束调用的增量无法归属，丢弃后由 done 补齐。
		toolArgsDelta(0, "", `["ambiguous"]`),
		{Type: "response.function_call_arguments.done", CallID: "call_a", Arguments: toolArgsRunCommands},
		toolItemDone(0, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	require.Len(t, calls, 2)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
	assertChatToolCall(t, calls[1], "call_b", "search_codebase", toolArgsSearch)
}

// 参数事件的 output_index 和 added 对不上，也没有 item_id：只有一个未结束调用时归给它。
func TestResponsesEventToChatChunks_ToolArgsMismatchedOutputIndexSingleOpenCall(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(1, "", "call_a", "run_commands"),
		toolArgsDelta(0, "", `{"commands":["git status --short",`),
		toolArgsDelta(0, "", `"git log --oneline -5"]}`),
		toolArgsDone(0, "", toolArgsRunCommands),
		completedEvent(),
	})
	require.Len(t, calls, 1)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
}

// 没见过 added 的调用在 output_item.done 里整段出现：作为新调用发出。
func TestResponsesEventToChatChunks_ToolCallOnlyInOutputItemDone(t *testing.T) {
	calls, finish := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		completedEvent(),
	})
	require.Len(t, calls, 1)
	assertChatToolCall(t, calls[0], "call_a", "run_commands", toolArgsRunCommands)
	assert.Equal(t, "tool_calls", finish)
}

// custom_tool_call 的输入只在 custom_tool_call_input.done 里出现。
func TestResponsesEventToChatChunks_CustomToolInputOnlyInDone(t *testing.T) {
	calls, _ := runChatToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "custom_tool_call", ID: "ctc_a", CallID: "call_patch", Name: "apply_patch"}},
		{Type: "response.custom_tool_call_input.done", OutputIndex: 0, ItemID: "ctc_a", Input: "*** Begin Patch\n*** End Patch"},
		completedEvent(),
	})
	require.Len(t, calls, 1)
	assert.Equal(t, "*** Begin Patch\n*** End Patch", calls[0].Arguments)
}

// 非流式累加器也要从 done 事件拿到参数，且增量 + done 不重复。
func TestBufferedResponseAccumulator_ToolArgsFromDoneEvents(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	for _, evt := range []ResponsesStreamEvent{
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		toolArgsDelta(1, "fc_b", `{"queries":`),
		toolArgsDone(1, "fc_b", toolArgsSearch),
		toolItemDone(1, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		toolItemDone(2, "fc_c", "call_c", "ask_question", `{"question":"ok?"}`),
	} {
		acc.ProcessEvent(&evt)
	}
	out := acc.BuildOutput()
	require.Len(t, out, 3)
	assert.Equal(t, "call_a", out[0].CallID)
	assert.Equal(t, toolArgsRunCommands, out[0].Arguments)
	assert.Equal(t, "call_b", out[1].CallID)
	assert.Equal(t, toolArgsSearch, out[1].Arguments)
	assert.Equal(t, "call_c", out[2].CallID)
	assert.Equal(t, "ask_question", out[2].Name)
	assert.Equal(t, `{"question":"ok?"}`, out[2].Arguments)
}
