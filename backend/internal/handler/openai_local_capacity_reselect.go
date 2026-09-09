package handler

import (
	"context"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	localCapacityReselectEnabledKey = "openai_local_capacity_reselect_enabled"
	localCapacityReselectGroupsKey  = "openai_local_capacity_reselect_group_ids"
	localCapacityWaitMillisKey      = "openai_local_capacity_wait_ms"
)

func accountAllowsLocalCapacityReselect(account *service.Account, groupID *int64) bool {
	if account == nil || groupID == nil || *groupID <= 0 {
		return false
	}
	enabled, _ := account.Extra[localCapacityReselectEnabledKey].(bool)
	if !enabled {
		return false
	}
	// Strict integer arrays only: strings, fractional IDs and malformed values
	// must not turn an operator opt-in into a wildcard for other tenants.
	raw, err := json.Marshal(account.Extra[localCapacityReselectGroupsKey])
	if err != nil {
		return false
	}
	var groups []int64
	if json.Unmarshal(raw, &groups) != nil {
		return false
	}
	matched := false
	for _, id := range groups {
		if id <= 0 {
			return false
		}
		if id == *groupID {
			matched = true
		}
	}
	return matched
}

func localCapacityReselectSafe(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Writer == nil || c.Request.Context().Err() != nil || service.IsResponseCommitted(c) {
		return false
	}
	if c.Writer.Written() && c.Writer.Status() >= 400 {
		return false
	}
	written := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
	if value, ok := c.Get(gatewayStreamHeartbeatBytesKey); ok {
		if heartbeatBytes, valid := value.(int); valid && heartbeatBytes > 0 && written > 0 {
			written -= heartbeatBytes
		}
	}
	return written <= 0
}

type localCapacityRejection struct {
	accountID int64
	platform  string
	status    int
	errType   string
	code      string
	message   string
	waitMs    int64
}

// This is request-local admission state, not an upstream attempt or a new
// recovery policy. Only the first selected account retains its normal wait.
type openAILocalCapacityReselect struct {
	originalContext context.Context
	groupID         *int64
	excluded        map[int64]struct{}
	switchCount     *int
	maxSwitches     int
	immediate       bool
	pending         *localCapacityRejection
	origin          *localCapacityRejection
	localSwitches   int
}

func newOpenAILocalCapacityReselect(ctx context.Context, groupID *int64, excluded map[int64]struct{}, switchCount *int, maxSwitches int) *openAILocalCapacityReselect {
	return &openAILocalCapacityReselect{originalContext: ctx, groupID: groupID, excluded: excluded, switchCount: switchCount, maxSwitches: maxSwitches}
}

func (r *openAILocalCapacityReselect) requestActive(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	for _, ctx := range []context.Context{r.originalContext, c.Request.Context()} {
		if ctx == nil {
			continue
		}
		if ctx.Err() != nil {
			return false
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return false
		}
	}
	return !service.RecoveryBudgetExpired(c)
}

func (r *openAILocalCapacityReselect) selectionContext(ctx context.Context) context.Context {
	if r.immediate {
		return service.WithOpenAILocalCapacityProbe(ctx)
	}
	return ctx
}

func (r *openAILocalCapacityReselect) writePending(h *OpenAIGatewayHandler, c *gin.Context, streamStarted bool) bool {
	if r.pending == nil {
		return false
	}
	pending := r.pending
	r.pending = nil
	if c.Request == nil || c.Request.Context().Err() != nil || (r.originalContext != nil && r.originalContext.Err() != nil) {
		return true
	}
	setOpsSelectedAccount(c, pending.accountID, pending.platform)
	service.MarkOpsLocalCapacityFailure(c)
	h.handleStreamingAwareErrorWithCode(c, pending.status, pending.errType, pending.code, pending.message, streamStarted, false)
	return true
}

func (r *openAILocalCapacityReselect) handleSelectionError(h *OpenAIGatewayHandler, c *gin.Context, err error, streamStarted bool) bool {
	if r.pending == nil {
		return false
	}
	if isOpsNoAvailableAccountError(err) {
		return r.writePending(h, c, streamStarted)
	}
	// A selector/dependency error after a local skip is not an upstream failure
	// or a reason to keep skipping accounts. Preserve a local platform outcome.
	r.pending = nil
	service.MarkOpsLocalCapacityFailure(c)
	h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "Service temporarily unavailable, please retry later", streamStarted)
	return true
}

func (r *openAILocalCapacityReselect) acquire(h *OpenAIGatewayHandler, c *gin.Context, sessionHash string,
	selection *service.AccountSelectionResult, reqStream bool, streamStarted *bool, reqLog *zap.Logger,
) (func(), openAISlotAcquireResult) {
	if r.immediate && !r.requestActive(c) {
		if selection != nil && selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		service.WriteRecoveryBudgetError(c)
		return nil, openAISlotAcquireFailed
	}
	if selection == nil || selection.Account == nil || (!r.immediate && !accountAllowsLocalCapacityReselect(selection.Account, r.groupID)) {
		return h.acquireResponsesAccountSlot(c, r.groupID, sessionHash, selection, reqStream, streamStarted, reqLog)
	}
	var rejected *localCapacityRejection
	c.Set(localCapacityWaitMillisKey, int64(0))
	writeError := func(status int, errType, code, message string) {
		if service.RecoveryBudgetExpired(c) {
			r.pending = nil
			c.Set(service.OpsLocalCapacityFailureKey, false)
			service.WriteRecoveryBudgetError(c)
			return
		}
		if status == http.StatusTooManyRequests && (code == gatewayQueueFullCode || code == gatewayConcurrencyLimitCode) && localCapacityReselectSafe(c) && r.requestActive(c) {
			rejected = &localCapacityRejection{accountID: selection.Account.ID, platform: selection.Account.Platform, status: status, errType: errType, code: code, message: message, waitMs: c.GetInt64(localCapacityWaitMillisKey)}
			return
		}
		// A dependency failure or cancellation is not another capacity candidate.
		r.pending = nil
		h.handleStreamingAwareErrorWithCode(c, status, errType, code, message, *streamStarted, false)
	}
	release, result := h.acquireOpenAIAccountSlot(c, r.groupID, sessionHash, selection, reqStream, streamStarted, reqLog, writeError, r.immediate)
	if result == openAISlotAcquireOK {
		if r.immediate && !r.requestActive(c) {
			if release != nil {
				release()
			}
			service.WriteRecoveryBudgetError(c)
			return nil, openAISlotAcquireFailed
		}
		if r.immediate {
			// Both eager-selector and helper binding were deferred during the
			// probe. Commit only after admission and the final cancellation check.
			ctx := service.ContextWithSelectionProfitGate(c.Request.Context(), selection)
			if err := h.gatewayService.BindStickySessionAfterProfitAdmission(ctx, r.groupID, sessionHash, selection.Account.ID); err != nil {
				reqLog.Warn("openai.local_capacity_binding_failed", zap.Int64("account_id", selection.Account.ID), zap.Error(err))
			}
			if !r.requestActive(c) {
				if release != nil {
					release()
				}
				service.WriteRecoveryBudgetError(c)
				return nil, openAISlotAcquireFailed
			}
		}
		// From here onward the actual Forward result owns the request outcome.
		if r.immediate && r.origin != nil {
			requestprofile.LocalReselect(c.Request.Context(), r.origin.accountID, 0, selection.Account.ID, r.origin.waitMs)
			reqLog.Info("openai.local_capacity_reselect_admitted",
				zap.String("origin", "local_account_admission"),
				zap.Int64("source_account_id", r.origin.accountID),
				zap.Int64("selected_account_id", selection.Account.ID),
				zap.String("reason_code", r.origin.code),
				zap.Int64("original_wait_ms", r.origin.waitMs),
				zap.Int("local_reselect_count", r.localSwitches))
		}
		r.pending = nil
		c.Set(service.OpsLocalCapacityFailureKey, false)
		return release, result
	}
	if rejected == nil {
		return release, result
	}
	if r.pending == nil {
		r.pending = rejected
	}
	if r.origin == nil {
		r.origin = rejected
		reqLog.Info("openai.local_capacity_reselect_eligible",
			zap.String("origin", "local_account_admission"),
			zap.Int64("source_account_id", rejected.accountID),
			zap.String("reason_code", rejected.code),
			zap.Int64("original_wait_ms", rejected.waitMs))
	}
	if !localCapacityReselectSafe(c) || !r.requestActive(c) || *r.switchCount >= r.maxSwitches || !service.ReserveErrorRecoveryAccountSwitch(c) {
		r.writePending(h, c, *streamStarted)
		return nil, openAISlotAcquireFailed
	}
	r.excluded[selection.Account.ID] = struct{}{}
	*r.switchCount++
	r.immediate = true
	r.localSwitches++
	// No terminal was sent. A later successful account must not inherit a local
	// failed-request marker; the deferred original rejection restores it if needed.
	c.Set(service.OpsLocalCapacityFailureKey, false)
	requestprofile.LocalReselect(c.Request.Context(), r.origin.accountID, selection.Account.ID, 0, r.origin.waitMs)
	reqLog.Info("openai.local_capacity_reselect",
		zap.String("origin", "local_account_admission"),
		zap.Int64("source_account_id", r.origin.accountID),
		zap.Int64("rejected_account_id", selection.Account.ID),
		zap.String("reason_code", rejected.code),
		zap.Int64("wait_ms", rejected.waitMs),
		zap.Int("local_reselect_count", r.localSwitches),
		zap.Int("request_switch_count", *r.switchCount))
	return nil, openAISlotAcquireReselect
}
