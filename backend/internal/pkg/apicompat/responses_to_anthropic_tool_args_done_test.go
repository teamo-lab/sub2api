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
	for i := range events {
		for _, out := range ResponsesEventToAnthropicEvents(&events[i], state) {
			switch out.Type {
			case "content_block_start":
				if out.ContentBlock.Type == "tool_use" {
					inputs[*out.Index] = ""
				}
			case "content_block_delta":
				if out.Delta.Type == "input_json_delta" {
					require.False(t, stopped[*out.Index], "delta after content_block_stop on block %d", *out.Index)
					inputs[*out.Index] += out.Delta.PartialJSON
				}
			case "content_block_stop":
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
