package middleware

import (
	"net/http"
	"strings"
)

func isProfiledInferenceRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") {
		return false
	}
	if r.Method != "POST" && !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	return path == "/chat/completions" || path == "/messages/count_tokens" || strings.HasPrefix(path, "/backend-api/codex/responses") || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.Contains(path, "/v1/") || strings.Contains(path, "/v1beta/") || path == "/responses" || strings.HasPrefix(path, "/responses/")
}
