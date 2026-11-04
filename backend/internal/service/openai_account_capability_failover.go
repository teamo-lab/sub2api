package service

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// 下面三类上游错误只说明「这个号接不住这次请求」，和 openai_context_window_failover.go 的超窗口
// 同一个处理方式：换同组里还没试过的号，全部用尽才把错误回给客户端；不同号重试、不冷却、
// 不进临时不可调度、不计入调度失败和 API key 健康熔断。
//
//   - 404 模型不支持：上游本身是 sub2api（如 codebot）时，它的分组里没有号接这个模型会回
//     404 {"error":{"type":"model_not_found","message":"Model \"gpt-6.1-terra\" is not supported
//     by any configured account in this group"}}。2026-11-03 这条没被认出来，p2/p3 能接的号一直
//     没被试到，模型挂了 2 小时。
//   - tool_choice 违规：codebot 偶发回 502 "... response violates tool_choice"（也可能是流内
//     response.failed），换号通常就能成功，跟账号健康无关。
//   - Cloudflare 403 "error code: 1010"：纯文本、按请求/边缘节点拦截，同一个号其它请求都正常。
const (
	openAIModelUnsupportedReason    = GatewayFailureReason("openai_model_unsupported")
	openAIToolChoiceViolationReason = GatewayFailureReason("openai_tool_choice_violation")
	openAIEdgeBlockedReason         = GatewayFailureReason("openai_edge_blocked")
)

const (
	openAIModelUnsupportedDefaultMessage    = "The requested model is not supported."
	openAIToolChoiceViolationDefaultMessage = "Upstream response violates tool_choice"
)

// cloudflareEdgeBlockPattern 匹配 Cloudflare 1xxx 拦截页的纯文本形态，例如 "error code: 1010"。
var cloudflareEdgeBlockPattern = regexp.MustCompile(`error code: 10[0-9]+`)

func markOpenAIAccountCapabilityMiss(failoverErr *UpstreamFailoverError, reason GatewayFailureReason, clientStatus int, clientMessage string) {
	if failoverErr == nil {
		return
	}
	failoverErr.RetryableOnSameAccount = false
	failoverErr.RequestScopedTransient = false
	failoverErr.Scope = GatewayFailureScopeAccount
	failoverErr.Reason = reason
	failoverErr.NextAccountAction = NextAccountRetry
	failoverErr.ClientStatusCode = clientStatus
	failoverErr.ClientMessage = clientMessage
}

// IsOpenAIAccountCapabilityMiss reports whether the selected account only
// could not serve this request (unsupported model, tool_choice violation, edge
// block); another account in the group may still serve it.
func (e *UpstreamFailoverError) IsOpenAIAccountCapabilityMiss() bool {
	if e == nil {
		return false
	}
	switch e.Reason {
	case openAIModelUnsupportedReason, openAIToolChoiceViolationReason, openAIEdgeBlockedReason:
		return true
	}
	return false
}

// IsOpenAIModelUnsupported reports whether the selected account answered 404
// model-not-found / model-not-supported.
func (e *UpstreamFailoverError) IsOpenAIModelUnsupported() bool {
	return e != nil && e.Reason == openAIModelUnsupportedReason
}

// IsOpenAIToolChoiceViolation reports whether the selected account's upstream
// produced a response that violated the request's tool_choice.
func (e *UpstreamFailoverError) IsOpenAIToolChoiceViolation() bool {
	return e != nil && e.Reason == openAIToolChoiceViolationReason
}

// isOpenAIToolChoiceViolation 只看错误字段；JSON 里回显的请求内容不算。纯文本错误才扫整个 body。
func isOpenAIToolChoiceViolation(upstreamMsg string, body []byte) bool {
	match := func(text string) bool {
		return strings.Contains(strings.ToLower(text), "violates tool_choice")
	}
	if match(upstreamMsg) {
		return true
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if !gjson.ValidBytes(body) {
		return match(string(body))
	}
	for _, path := range []string{"error.message", "response.error.message", "message", "detail"} {
		if value := gjson.GetBytes(body, path); value.Type == gjson.String && match(value.String()) {
			return true
		}
	}
	return false
}

// isCloudflareEdgeBlock 认 Cloudflare 边缘拦截的纯文本 403（"error code: 1010" 之类）。
// JSON 403（额度、权限、账号状态）不在此列，仍走原有处理。
func isCloudflareEdgeBlock(statusCode int, body []byte) bool {
	if statusCode != http.StatusForbidden {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || gjson.ValidBytes(trimmed) {
		return false
	}
	return cloudflareEdgeBlockPattern.Match(bytes.ToLower(trimmed))
}

func markOpenAIModelUnsupported(failoverErr *UpstreamFailoverError, upstreamMsg string, body []byte) {
	markOpenAIAccountCapabilityMiss(failoverErr, openAIModelUnsupportedReason, http.StatusNotFound,
		openAICapabilityClientMessage(upstreamMsg, body, openAIModelUnsupportedDefaultMessage))
}

func markOpenAIToolChoiceViolation(failoverErr *UpstreamFailoverError, upstreamMsg string, body []byte) {
	markOpenAIAccountCapabilityMiss(failoverErr, openAIToolChoiceViolationReason, http.StatusBadGateway,
		openAICapabilityClientMessage(upstreamMsg, body, openAIToolChoiceViolationDefaultMessage))
}

func markOpenAIEdgeBlocked(failoverErr *UpstreamFailoverError) {
	// 用尽后沿用 403 的默认映射（502 Upstream access forbidden），这里不带上游文案。
	markOpenAIAccountCapabilityMiss(failoverErr, openAIEdgeBlockedReason, 0, "")
}

func openAICapabilityClientMessage(upstreamMsg string, body []byte, fallback string) string {
	for _, candidate := range []string{
		upstreamMsg,
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	} {
		if candidate = sanitizeUpstreamErrorMessage(strings.TrimSpace(candidate)); candidate != "" {
			return candidate
		}
	}
	return fallback
}
