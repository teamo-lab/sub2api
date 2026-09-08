package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Router contract v2 is an opt-in, internal protocol used by Teamo Router's
// direct data plane. Ordinary Sub2API callers never receive these headers.
const (
	RouterContractHeader = "x-teamo-router-contract"
	RouterContractV2     = "v2"
	RouterOutcomeHeader  = "x-teamo-router-outcome"
	RouterAttemptsHeader = "x-teamo-upstream-attempts"
	routerAttemptsKey    = "router_upstream_attempts"
	maxRouterAttempts    = 64
)

type RouterUpstreamAttempt struct {
	AccountID   int64  `json:"account_id"`
	StartedAtMs int64  `json:"started_at_ms,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	Status      int    `json:"status,omitempty"`
	Result      string `json:"result"`
	ErrorType   string `json:"error_type,omitempty"`
}

type routerUpstreamAttempts struct {
	startedAt time.Time
	attempts  []RouterUpstreamAttempt
}

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
	if c == nil || c.Request == nil {
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

// BeginRouterUpstreamAttempts publishes the explicit unknown sentinel before
// a streaming keepalive can commit the response headers. A selected account
// replaces it with a negative account id.
func BeginRouterUpstreamAttempts(c *gin.Context) {
	if !RouterContractV2Requested(c) {
		return
	}
	state := &routerUpstreamAttempts{
		startedAt: time.Now(),
		attempts:  []RouterUpstreamAttempt{{AccountID: 0, Result: "pending"}},
	}
	c.Set(routerAttemptsKey, state)
	publishRouterUpstreamAttempts(c, state)
}

// RecordRouterUpstreamAccount starts one physical Sub2API account attempt.
// Negative ids keep this flat list disjoint from Router channel ids.
func RecordRouterUpstreamAccount(c *gin.Context, accountID int64) {
	if accountID <= 0 || !RouterContractV2Requested(c) {
		return
	}
	state := routerAttemptsState(c)
	if state == nil {
		BeginRouterUpstreamAttempts(c)
		state = routerAttemptsState(c)
	}
	if state == nil {
		return
	}
	nowMs := time.Since(state.startedAt).Milliseconds()
	last := len(state.attempts) - 1
	if last >= 0 && state.attempts[last].Result == "pending" {
		if state.attempts[last].AccountID == 0 {
			state.attempts[last].AccountID = -accountID
			state.attempts[last].StartedAtMs = nowMs
			publishRouterUpstreamAttempts(c, state)
			return
		}
		if state.attempts[last].AccountID == -accountID {
			return
		}
		state.attempts[last].Result = "failed"
		state.attempts[last].DurationMs = maxInt64(0, nowMs-state.attempts[last].StartedAtMs)
		state.attempts[last].ErrorType = "account_reselected"
	}
	if len(state.attempts) >= maxRouterAttempts {
		return
	}
	state.attempts = append(state.attempts, RouterUpstreamAttempt{
		AccountID:   -accountID,
		StartedAtMs: nowMs,
		Result:      "pending",
	})
	publishRouterUpstreamAttempts(c, state)
}

// MarkRouterUpstreamAttemptFailed completes the latest matching account
// attempt. The successful final attempt remains pending in the header and the
// Router fills its result/status/duration from the enclosing HTTP attempt.
func MarkRouterUpstreamAttemptFailed(c *gin.Context, accountID int64, status int, errorType string, atUnixMs int64) {
	if !RouterContractV2Requested(c) {
		return
	}
	state := routerAttemptsState(c)
	if state == nil {
		return
	}
	completedMs := time.Since(state.startedAt).Milliseconds()
	if atUnixMs > 0 {
		completedMs = atUnixMs - state.startedAt.UnixMilli()
	}
	index := -1
	for i := len(state.attempts) - 1; i >= 0; i-- {
		if state.attempts[i].Result != "pending" {
			continue
		}
		if accountID <= 0 || state.attempts[i].AccountID == -accountID {
			index = i
			break
		}
	}
	if index < 0 && accountID > 0 && len(state.attempts) < maxRouterAttempts {
		state.attempts = append(state.attempts, RouterUpstreamAttempt{
			AccountID: -accountID,
			Result:    "pending",
		})
		index = len(state.attempts) - 1
	}
	if index < 0 {
		return
	}
	attempt := &state.attempts[index]
	attempt.DurationMs = maxInt64(0, completedMs-attempt.StartedAtMs)
	attempt.Status = status
	attempt.Result = "failed"
	attempt.ErrorType = compactRouterErrorType(errorType)
	publishRouterUpstreamAttempts(c, state)
}

func routerAttemptsState(c *gin.Context) *routerUpstreamAttempts {
	if c == nil {
		return nil
	}
	value, ok := c.Get(routerAttemptsKey)
	if !ok {
		return nil
	}
	state, _ := value.(*routerUpstreamAttempts)
	return state
}

func publishRouterUpstreamAttempts(c *gin.Context, state *routerUpstreamAttempts) {
	if c == nil || c.Writer == nil || state == nil {
		return
	}
	payload, err := json.Marshal(state.attempts)
	if err != nil {
		return
	}
	setRouterPrivateHeader(c, RouterAttemptsHeader, string(payload))
}

func setRouterPrivateHeader(c *gin.Context, name, value string) {
	if !RouterContractV2Requested(c) || c.Writer == nil {
		return
	}
	if writer, ok := c.Writer.(*openAICompactKeepaliveWriter); ok && writer.k != nil && writer.ResponseWriter != nil {
		writer.k.mu.Lock()
		defer writer.k.mu.Unlock()
		if !writer.ResponseWriter.Written() {
			writer.ResponseWriter.Header().Set(name, value)
		}
		return
	}
	if !c.Writer.Written() {
		c.Writer.Header().Set(name, value)
	}
}

func compactRouterErrorType(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
