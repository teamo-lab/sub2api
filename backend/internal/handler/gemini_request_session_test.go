//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestChatCompletionsSelectionSessionHash(t *testing.T) {
	explicit := service.GeminiRequestSessionHash("6f1c2a64-6d1e-5b8a-9c3e-1f2a3b4c5d6e")

	// Gemini：显式会话头优先，且带 gemini: 前缀。
	require.Equal(t, "gemini:"+explicit,
		chatCompletionsSelectionSessionHash(service.PlatformGemini, "content-hash", "6f1c2a64-6d1e-5b8a-9c3e-1f2a3b4c5d6e"))
	// 同一个会话 ID、不同内容 hash（多轮对话）得到同一个键。
	require.Equal(t,
		chatCompletionsSelectionSessionHash(service.PlatformGemini, "turn-1", "sid"),
		chatCompletionsSelectionSessionHash(service.PlatformGemini, "turn-2", "sid"))
	// Gemini 无显式头：沿用内容 hash。
	require.Equal(t, "gemini:content-hash", chatCompletionsSelectionSessionHash(service.PlatformGemini, "content-hash", ""))
	require.Equal(t, "", chatCompletionsSelectionSessionHash(service.PlatformGemini, "", ""))
	// 其他平台不受显式头影响。
	require.Equal(t, "content-hash", chatCompletionsSelectionSessionHash(service.PlatformAnthropic, "content-hash", "sid"))
}
