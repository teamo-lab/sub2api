package service

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// "does not exist" 覆盖 OpenAI 现行文案：
// "The model `gpt-5.5` does not exist or you do not have access to it."
// 判定同时要求正文含 "model"（见 isUpstreamModelNotFoundError），所以不会把
// 无关的 404 误判成模型缺失。
var upstreamModelNotFoundKeywords = []string{"model not found", "unknown model", "not found", "does not exist"}

func isUpstreamModelNotFoundError(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" || !strings.Contains(normalized, "model") {
		return false
	}
	return containsModelNotFoundKeyword(normalized)
}

func isModelNotFoundError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) || statusCode == http.StatusNotFound
}

// openAICodexPlanGatedModelPhrase matches the deterministic Codex 400 returned
// when a ChatGPT OAuth account's plan cannot serve the requested model, e.g.
// {"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}
// The phrase is compared against the normalized body (lowercased, "_"/"-"
// folded to spaces), so it also matches the same message embedded in
// error.message-style payloads.
const openAICodexPlanGatedModelPhrase = "model is not supported when using codex"

// isOpenAICodexPlanGatedModelError reports whether the upstream response is the
// deterministic Codex rejection of a plan-gated model on a ChatGPT account.
// Unlike transient failures, retrying the same account cannot succeed until the
// account's plan changes, so callers should treat it like model-not-found and
// cool the (account, model) pair down instead of re-selecting the account.
func isOpenAICodexPlanGatedModelError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, openAICodexPlanGatedModelPhrase)
}

func containsModelNotFoundKeyword(normalizedBody string) bool {
	if normalizedBody == "" {
		return false
	}
	for _, keyword := range upstreamModelNotFoundKeywords {
		if strings.Contains(normalizedBody, keyword) {
			return true
		}
	}
	return false
}

func normalizeModelNotFoundBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	normalized := strings.ToLower(string(body))
	normalized = strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(normalized)
	return strings.Join(strings.Fields(normalized), " ")
}

// isOpenAIStreamModelNotFoundEvent 判定流内终止帧是否为"账号不具备该模型"。
//
// 这类失败以 HTTP 200 + SSE 帧到达，语义状态被 openAIStreamFailureStatus 归一成
// 502，因此走不到 HTTP 路径的 model-not-found 处理，账号+模型冷却永远不会写：
// 同一个模型会被反复派给同一个没有权限的账号，每次白等数十秒才失败。
//
// 三种形态都认：response.error.code / error.code（结构化终止帧）、顶层 code
// （裸 error 帧），以及正文文案（部分中转只给 message）。
func isOpenAIStreamModelNotFoundEvent(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 {
		return false
	}
	if code := openAIStreamFailedEventErrorCode(payload); code == "model_not_found" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "code").String()), "model_not_found") {
		return true
	}
	return isUpstreamModelNotFoundError(http.StatusNotFound, payload) ||
		isOpenAICodexPlanGatedModelError(http.StatusBadRequest, payload)
}
