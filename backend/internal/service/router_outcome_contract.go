package service

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// Router contract v2 is an opt-in, internal protocol used by Teamo Router's
// direct data plane. Ordinary Sub2API callers never receive these headers.
const (
	RouterContractHeader = "x-teamo-router-contract"
	RouterContractV2     = "v2"
	RouterOutcomeHeader  = "x-teamo-router-outcome"
)

type RouterOutcome string

const (
	RouterOutcomeBusinessCommit RouterOutcome = "business_commit"
	RouterOutcomeRetryableAbort RouterOutcome = "retryable_abort"
	RouterOutcomeTerminalError  RouterOutcome = "terminal_error"
)

// RouterContractV2Requested reports whether this request opted into the
// internal direct-relay outcome contract. The header is intentionally not
// trusted from public clients; the caller must already be an authenticated
// internal Gateway request before using this helper.
func RouterContractV2Requested(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c.GetHeader(RouterContractHeader)), RouterContractV2)
}

// SetRouterOutcome stamps an internal outcome before the response is written.
// It is a no-op for ordinary callers and never overwrites an existing outcome.
func SetRouterOutcome(c *gin.Context, outcome RouterOutcome) {
	if !RouterContractV2Requested(c) || c.Writer == nil || c.Writer.Written() {
		return
	}
	if c.Writer.Header().Get(RouterOutcomeHeader) != "" {
		return
	}
	c.Header(RouterContractHeader, RouterContractV2)
	c.Header(RouterOutcomeHeader, string(outcome))
}
