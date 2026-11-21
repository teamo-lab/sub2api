package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const GeminiModelNotSupportedReason GatewayFailureReason = "model_not_supported"

// Inspect error fields, never an echoed prompt. Only model-capability 404s
// qualify: content refusals (400) and missing HTTP routes keep their policy.
func isGeminiModelNotSupported404(status int, body []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	message := string(body)
	if gjson.ValidBytes(body) {
		for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "code"} {
			switch strings.ToLower(gjson.GetBytes(body, path).String()) {
			case "model_not_found", "model_not_supported", "unsupported_model":
				return true
			}
		}
		message = ""
		for _, path := range []string{"error.message", "response.error.message", "message", "detail"} {
			if value := gjson.GetBytes(body, path); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				message = value.String()
				break
			}
		}
	}
	message = normalizeModelNotFoundBody([]byte(message))
	if strings.Contains(message, "所请求的模型暂不受支持") {
		return true
	}
	if !strings.Contains(message, "model") {
		return false
	}
	for _, phrase := range []string{"model not found", "model not supported", "unknown model", "does not exist", "is not supported", "not available"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func (s *GeminiMessagesCompatService) modelNotSupportedFailover(c *gin.Context, account *Account, status int, body []byte, requestID string) *UpstreamFailoverError {
	if !isGeminiModelNotSupported404(status, body) {
		return nil
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: status, UpstreamRequestID: requestID,
		Kind: "failover", Reason: string(GeminiModelNotSupportedReason),
		Message: extractUpstreamErrorMessage(body),
	})
	// Capability cannot improve by retrying the same account. Do not mutate
	// account health or pricing; the exclusion lives only in this request.
	return &UpstreamFailoverError{StatusCode: status, ResponseBody: body, Reason: GeminiModelNotSupportedReason}
}
