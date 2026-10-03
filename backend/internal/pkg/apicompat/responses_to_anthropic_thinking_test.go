package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newThinkingTestRequest(model, effort string) *ResponsesRequest {
	return &ResponsesRequest{
		Model:     model,
		Input:     json.RawMessage(`[{"role":"user","content":"hello"}]`),
		Reasoning: &ResponsesReasoning{Effort: effort},
	}
}

func TestResponsesToAnthropicRequest_AdaptiveOnlyClaudeUsesAdaptiveThinking(t *testing.T) {
	models := []string{
		"claude-opus-5-5", "claude-opus-5", "claude-opus-4-7", "claude-opus-4-8",
		"claude-sonnet-5", "claude-sonnet-5-5", "claude-opus-5-5[1m]", "Claude-Sonnet-5-5",
	}
	for _, model := range models {
		for _, effort := range []string{"medium", "high", "xhigh"} {
			out, err := ResponsesToAnthropicRequest(newThinkingTestRequest(model, effort))
			require.NoError(t, err)
			require.NotNil(t, out.Thinking, "%s/%s", model, effort)
			assert.Equal(t, "adaptive", out.Thinking.Type, "%s/%s", model, effort)
			assert.Zero(t, out.Thinking.BudgetTokens, "%s/%s", model, effort)
			require.NotNil(t, out.OutputConfig, "%s/%s", model, effort)
			assert.Equal(t, mapResponsesEffortToAnthropic(effort), out.OutputConfig.Effort)

			raw, err := json.Marshal(out.Thinking)
			require.NoError(t, err)
			assert.JSONEq(t, `{"type":"adaptive"}`, string(raw))
		}
	}
}

func TestResponsesToAnthropicRequest_LegacyClaudeKeepsManualBudget(t *testing.T) {
	models := []string{
		"claude-opus-4-6", "claude-sonnet-4-6", "claude-sonnet-4-20250514",
		"claude-opus-4-20250514", "claude-opus-4-1", "claude-haiku-4-5", "claude-fable-5",
	}
	for _, model := range models {
		out, err := ResponsesToAnthropicRequest(newThinkingTestRequest(model, "high"))
		require.NoError(t, err)
		require.NotNil(t, out.Thinking, model)
		assert.Equal(t, "enabled", out.Thinking.Type, model)
		assert.Equal(t, defaultThinkingBudget("high"), out.Thinking.BudgetTokens, model)
	}
}

func TestResponsesToAnthropicRequest_LowEffortOmitsThinkingForAdaptiveOnlyClaude(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(newThinkingTestRequest("claude-opus-5-5", "low"))
	require.NoError(t, err)
	assert.Nil(t, out.Thinking)
	require.NotNil(t, out.OutputConfig)
	assert.Equal(t, "low", out.OutputConfig.Effort)
}

func TestClaudeRequiresAdaptiveThinking(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-5-5", true},
		{"claude-opus-5", true},
		{"claude-opus-4-7", true},
		{"claude-opus-4.8", true},
		{"claude-sonnet-5", true},
		{"claude-sonnet-6-1", true},
		{"claude-opus-4-6", false},
		{"claude-opus-4-20250514", false},
		{"claude-sonnet-4-6", false},
		{"claude-sonnet-4-5", false},
		{"claude-haiku-4-5", false},
		{"claude-fable-5", false},
		{"gpt-6-astra", false},
		{"claude-opus-", false},
		{"claude-sonnet-x", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, claudeRequiresAdaptiveThinking(tc.model), tc.model)
	}
}
