package apicompat

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/codebot_tool_args_done 是 2026-10-28 对 codebot（ai.codebot.one）/v1/responses
// 回放三条 Cline 请求抓到的原始 SSE：事件类型、顺序、output_index、item/call id 原样保留，
// 文本和参数内容换成占位值。codebot 不发 function_call_arguments.delta，
// 参数只在 function_call_arguments.done 和 output_item.done 里。
func loadCodebotFixture(t *testing.T, name string) []ResponsesStreamEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "codebot_tool_args_done", name))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var events []ResponsesStreamEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var evt ResponsesStreamEvent
		require.NoError(t, json.Unmarshal([]byte(data), &evt))
		events = append(events, evt)
	}
	require.NoError(t, scanner.Err())
	return events
}

// fixtureToolArgs 返回 fixture 里各工具调用（按出现顺序）在 function_call_arguments.done 中的完整参数。
func fixtureToolArgs(t *testing.T, events []ResponsesStreamEvent) []string {
	t.Helper()
	var args []string
	for _, evt := range events {
		require.NotEqual(t, "response.function_call_arguments.delta", evt.Type, "fixture should reproduce the done-only upstream")
		if evt.Type == "response.function_call_arguments.done" {
			args = append(args, evt.Arguments)
		}
	}
	require.Len(t, args, 2)
	return args
}

var codebotFixtures = []string{
	"gpt6sol_text_then_two_tools.sse",
	"gpt61terra_two_tools.sse",
	"gpt61sol_reasoning_two_tools.sse",
}

func TestCodebotDoneOnlyToolArgs_ChatStream(t *testing.T) {
	for _, name := range codebotFixtures {
		t.Run(name, func(t *testing.T) {
			events := loadCodebotFixture(t, name)
			want := fixtureToolArgs(t, events)
			calls, finish := runChatToolStream(t, events)
			require.Len(t, calls, 2)
			assert.Equal(t, "run_commands", calls[0].Name)
			assert.Equal(t, "search_codebase", calls[1].Name)
			for i, args := range want {
				assert.NotEmpty(t, calls[i].ID)
				assert.Equal(t, args, calls[i].Arguments)
				assert.True(t, json.Valid([]byte(calls[i].Arguments)))
			}
			assert.Equal(t, "tool_calls", finish)
		})
	}
}

func TestCodebotDoneOnlyToolArgs_AnthropicStream(t *testing.T) {
	for _, name := range codebotFixtures {
		t.Run(name, func(t *testing.T) {
			events := loadCodebotFixture(t, name)
			want := fixtureToolArgs(t, events)
			inputs := runAnthropicToolStream(t, events)
			require.Len(t, inputs, 2)
			var got []string
			for idx := 0; len(got) < len(inputs) && idx < 10; idx++ {
				if in, ok := inputs[idx]; ok {
					got = append(got, in)
				}
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestCodebotDoneOnlyToolArgs_BufferedAccumulator(t *testing.T) {
	for _, name := range codebotFixtures {
		t.Run(name, func(t *testing.T) {
			events := loadCodebotFixture(t, name)
			want := fixtureToolArgs(t, events)
			acc := NewBufferedResponseAccumulator()
			for i := range events {
				acc.ProcessEvent(&events[i])
			}
			var got []string
			for _, item := range acc.BuildOutput() {
				if item.Type == "function_call" {
					got = append(got, item.Arguments)
				}
			}
			assert.Equal(t, want, got)
		})
	}
}
