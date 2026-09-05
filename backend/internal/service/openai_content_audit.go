package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpenAIContentAuditRejectedReason = GatewayFailureReason("openai_content_audit_rejected")

// A content rejection belongs to the request, not the credential. Only apply
// this policy to GPT models on the OpenAI platform, and only to explicit 403s.
// Inspect the structured error, never arbitrary request/response text.
func isOpenAIGPTContentAuditRejection(account *Account, model string, status int, body []byte) bool {
	if account == nil || account.Platform != PlatformOpenAI || status != http.StatusForbidden ||
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-") {
		return false
	}
	err := gjson.GetBytes(body, "error")
	if !err.IsObject() {
		err = gjson.GetBytes(body, "response.error")
	}
	if !err.IsObject() {
		return false
	}
	for _, field := range []string{"code", "type"} {
		switch strings.ToLower(strings.TrimSpace(err.Get(field).String())) {
		case "content_policy_violation", "content_filter", "content_filter_error", "content_moderation_blocked":
			return true
		}
	}
	return strings.Contains(err.Get("message").String(), "内容审计命中风险规则")
}

func newOpenAIContentAuditRejection(c *gin.Context, account *Account, model string, status int, body []byte) *UpstreamFailoverError {
	if !isOpenAIGPTContentAuditRejection(account, model, status, body) {
		return nil
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	setOpsUpstreamError(c, http.StatusForbidden, message, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID,
		UpstreamStatusCode: http.StatusForbidden, Kind: "content_audit_rejected", Message: message,
	})
	// Use the handler's existing terminal upstream-error path without entering
	// either retry loop, recording a switch, or updating account health.
	return &UpstreamFailoverError{
		StatusCode: http.StatusForbidden, ResponseBody: body,
		Scope: GatewayFailureScopeRequest, Reason: OpenAIContentAuditRejectedReason,
		NextAccountAction: NextAccountStop,
		ClientStatusCode:  http.StatusForbidden, ClientMessage: message,
	}
}
