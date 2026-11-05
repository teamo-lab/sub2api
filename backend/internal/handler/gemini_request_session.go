package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// chatCompletionsSelectionSessionHash 计算 /v1/chat/completions 的选号粘性键。
//
// Gemini 分组下，显式 X-Request-Session-Id 优先于内容兜底 hash：后者混入了全部消息，
// 每多一轮就变一次，粘不住多轮会话。Gemini 粘性键统一带 "gemini:" 前缀；
// 其他平台保持原样。
func chatCompletionsSelectionSessionHash(groupPlatform, contentHash, explicitSessionID string) string {
	if groupPlatform != service.PlatformGemini {
		return contentHash
	}
	key := contentHash
	if explicit := service.GeminiRequestSessionHash(explicitSessionID); explicit != "" {
		key = explicit
	}
	if key == "" {
		return ""
	}
	return "gemini:" + key
}
