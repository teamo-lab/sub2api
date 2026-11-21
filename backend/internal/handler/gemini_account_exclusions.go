package handler

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const geminiExcludedAccountsHeader = "X-Teamo-Excluded-Account-Ids"
const maxGeminiExcludedAccounts = 64

// Only the trusted Router's admin-owned key may exclude physical accounts
// failed earlier in the same request on a different Sub2API group.
func installGeminiAccountExclusions(c *gin.Context, apiKey *service.APIKey, fs *FailoverState) {
	if apiKey == nil || apiKey.User == nil || !apiKey.User.IsAdmin() {
		return
	}
	raw := c.GetHeader(geminiExcludedAccountsHeader)
	if raw == "" || len(raw) > 1280 {
		return
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxGeminiExcludedAccounts {
		return
	}
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return // Reject the whole malformed header, not a partial allowlist.
		}
		ids = append(ids, id)
	}
	fs.excludeForRequest(ids...)
}
