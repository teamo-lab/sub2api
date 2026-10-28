package apicompat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runAnthropicToolStream 按 content block index 拼出 tool_use 块的 input_json_delta。
func runAnthropicToolStream(t *testing.T, events []ResponsesStreamEvent) map[int]string {
	t.Helper()
	state := NewResponsesEventToAnthropicState()
	inputs := map[int]string{}
	stopped := map[int]bool{}
	open := -1
	for i := range events {
		for _, out := range ResponsesEventToAnthropicEvents(&events[i], state) {
			switch out.Type {
			case "content_block_start":
				require.Equal(t, -1, open, "block %d started while block %d is still open", *out.Index, open)
				open = *out.Index
				if out.ContentBlock.Type == "tool_use" {
					inputs[*out.Index] = ""
				}
			case "content_block_delta":
				if out.Delta.Type == "input_json_delta" {
					require.False(t, stopped[*out.Index], "delta after content_block_stop on block %d", *out.Index)
					inputs[*out.Index] += out.Delta.PartialJSON
				}
			case "content_block_stop":
				require.Equal(t, open, *out.Index, "stop for a block that is not open")
				open = -1
				stopped[*out.Index] = true
			}
		}
	}
	return inputs
}

// 参数只在 function_call_arguments.done / output_item.done 里给出，没有增量。
func TestResponsesEventToAnthropic_ToolArgsOnlyInDoneEvents(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolItemDone(1, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands, 1: toolArgsSearch}, inputs)
}

// 增量的 output_index 和 added 对不上：归到当前打开的 tool_use 块，done 不重复发。
func TestResponsesEventToAnthropic_ToolArgsMismatchedOutputIndexNoDuplicate(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(1, "fc_a", "call_a", "run_commands"),
		toolArgsDelta(5, "", `{"commands":["git status --short",`),
		toolArgsDone(5, "", toolArgsRunCommands),
		toolItemDone(1, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands}, inputs)
}

// custom_tool_call 的输入只在 custom_tool_call_input.done 里出现。
func TestResponsesEventToAnthropic_CustomToolInputOnlyInDone(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		{Type: "response.output_item.added", OutputIndex: 0, Item: &ResponsesOutput{Type: "custom_tool_call", ID: "ctc_a", CallID: "call_patch", Name: "apply_patch"}},
		{Type: "response.custom_tool_call_input.done", OutputIndex: 0, ItemID: "ctc_a", Input: `{"patch":"x"}`},
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: `{"patch":"x"}`}, inputs)
}

// 并行调用先连发 added、参数稍后才到（output_index 还重复）：B 排队等 A 的块结束，
// A 的 done 写进 A，B 的参数写进 B，互不串。
func TestResponsesEventToAnthropic_InterleavedAddedThenDoneEvents(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(0, "fc_b", "call_b", "search_codebase"),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		{Type: "response.function_call_arguments.done", OutputIndex: 0, CallID: "call_b", Arguments: toolArgsSearch},
		toolItemDone(0, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands, 1: toolArgsSearch}, inputs)
}

// B 的参数先于 A 的到达：B 的参数先缓冲，A 的块结束后再给 B 开块补发。
func TestResponsesEventToAnthropic_InterleavedLaterCallFinishesFirst(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolArgsDone(1, "fc_b", toolArgsSearch),
		toolItemDone(1, "fc_b", "call_b", "search_codebase", toolArgsSearch),
		toolArgsDelta(7, "fc_a", `{"commands":["git status`),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands, 1: toolArgsSearch}, inputs)
}

// 两个调用的参数增量交替到达：各自归位，done 不重复补发。
func TestResponsesEventToAnthropic_InterleavedDeltas(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolArgsDelta(0, "fc_a", `{"commands":["git status --short",`),
		toolArgsDelta(1, "fc_b", `{"queries":["ResponsesEventToChatChunks"],`),
		toolArgsDelta(0, "fc_a", `"git log --oneline -5"]}`),
		toolArgsDelta(1, "fc_b", `"path":"backend"}`),
		toolArgsDone(1, "fc_b", toolArgsSearch),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands, 1: toolArgsSearch}, inputs)
}

// 排队的调用一直没等到参数事件：结束时照样开块关块，调用本身不丢。
func TestResponsesEventToAnthropic_PendingToolFlushedAtCompletion(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolAdded(1, "fc_b", "call_b", "search_codebase"),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		completedEvent(),
	})
	assert.Equal(t, map[int]string{0: toolArgsRunCommands, 1: ""}, inputs)
}

// 官方顺序（增量完整 + done）仍然只发一次。
func TestResponsesEventToAnthropic_ToolArgsDeltaThenDoneNoDuplicate(t *testing.T) {
	inputs := runAnthropicToolStream(t, []ResponsesStreamEvent{
		createdEvent(),
		toolAdded(0, "fc_a", "call_a", "run_commands"),
		toolArgsDelta(0, "fc_a", toolArgsRunCommands),
		toolArgsDone(0, "fc_a", toolArgsRunCommands),
		toolItemDone(0, "fc_a", "call_a", "run_commands", toolArgsRunCommands),
		completedEvent(),
	})
	require.Len(t, inputs, 1)
	assert.JSONEq(t, toolArgsRunCommands, inputs[0])
	assert.Equal(t, toolArgsRunCommands, inputs[0])
}
