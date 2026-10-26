package service

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// 上下文窗口超限（context_length_exceeded）按「这个号接不住」处理：换同组里还没试过的号，
// 全部用尽才把错误回给客户端。
//
// 同一个模型在不同上游的窗口可能差一倍以上。例：sv-1 group 2 的 gpt-6.1-sol，Zero 成功接过
// 91 万 token 的输入，codebot 超过约 40 万就回 response.failed context_length_exceeded（被包成
// HTTP 502）。原先超窗口一律不换号，长会话只要落到窗口小的号就直接失败。
//
// 超窗口跟账号健康无关：不同号重试、不冷却、不计入调度失败和 API key 健康熔断。
const openAIContextWindowExceededReason = GatewayFailureReason("openai_context_window_exceeded")

const openAIContextWindowExceededDefaultMessage = "Your input exceeds the context window of this model. Please adjust your input and try again."

// markOpenAIContextWindowExceeded 把 failover 错误标成「这个号的上下文窗口不够」。
func markOpenAIContextWindowExceeded(failoverErr *UpstreamFailoverError, upstreamMsg string, responseBody []byte) {
	if failoverErr == nil {
		return
	}
	failoverErr.RetryableOnSameAccount = false
	failoverErr.RequestScopedTransient = false
	failoverErr.Scope = GatewayFailureScopeAccount
	failoverErr.Reason = openAIContextWindowExceededReason
	failoverErr.NextAccountAction = NextAccountRetry
	failoverErr.ClientStatusCode = http.StatusBadRequest
	failoverErr.ClientMessage = openAIContextWindowClientMessage(upstreamMsg, responseBody)
}

func openAIContextWindowClientMessage(upstreamMsg string, body []byte) string {
	for _, candidate := range []string{
		upstreamMsg,
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	} {
		candidate = sanitizeUpstreamErrorMessage(strings.TrimSpace(candidate))
		if candidate != "" && isOpenAIContextWindowError(candidate, nil) {
			return candidate
		}
	}
	return openAIContextWindowExceededDefaultMessage
}

// IsOpenAIContextWindowExceeded reports whether the selected account rejected
// the request because its context window is smaller than the input; another
// account in the group may still accept it.
func (e *UpstreamFailoverError) IsOpenAIContextWindowExceeded() bool {
	return e != nil && e.Reason == openAIContextWindowExceededReason
}
