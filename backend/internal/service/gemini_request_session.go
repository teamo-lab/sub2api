package service

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
)

// GeminiRequestSessionHeader 是调用方显式声明的会话标识。
//
// TeamoRouter 会给发往 Sub2API Gemini 分组的请求带上它（按 uid+模型+分片号生成的
// 固定 UUID）。Sub2API 用它做两件事：
//  1. 分组内选号的粘性键，同一会话前后几轮落在同一个号上，提高上游缓存命中；
//  2. 原样透传给 Gemini api_key 号的上游，部分中转（如 geminimodel anti）会按它
//     在自己的账号池里做会话亲和。
//
// 只作用于 Gemini 平台；其他平台的选号与转发不受影响。
const GeminiRequestSessionHeader = "X-Request-Session-Id"

// geminiRequestSessionHashPrefix 区分显式会话键与内容摘要/CLI 会话键，避免互相串号。
const geminiRequestSessionHashPrefix = "rsid:"

// GeminiRequestSessionID 读取并清洗客户端传入的 X-Request-Session-Id；
// 缺失、含控制字符或超长时返回空串。
func GeminiRequestSessionID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return sanitizeSessionID(c.GetHeader(GeminiRequestSessionHeader))
}

// GeminiRequestSessionHash 把显式会话 ID 转成定长的粘性会话 hash（不含原值），
// 空输入返回空串。调用方仍按既有约定再加 "gemini:" 前缀。
func GeminiRequestSessionHash(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return geminiRequestSessionHashPrefix + hex.EncodeToString(sum[:16])
}

// setGeminiRequestSessionHeader 在发往 Gemini api_key 上游的请求上带上已清洗的会话头。
func setGeminiRequestSessionHeader(req *http.Request, sessionID string) {
	if req == nil || sessionID == "" {
		return
	}
	req.Header.Set(GeminiRequestSessionHeader, sessionID)
}
