package service

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
)

const (
	openAILateFailureSwitchEnabledKey = "openai_late_failure_switch_enabled"
	openAILateFailureSwitchGroupsKey  = "openai_late_failure_switch_group_ids"
	openAILateFailureSwitchComponent  = "service.openai.error_recovery"
	openAILateFailureSwitchOrigin     = "late_upstream_failure"
	openAILateFailureSwitchEvent      = "openai.late_failure_retry_skipped"
	// The first HK43 experiment enables only account22/group3 in its manifest.
	// Application identity comes from selected Account.extra + authenticated
	// key group, never from database numbers that could alias on another host.
	OpenAILateFailureSwitchMinWait = 60 * time.Second
)

// ErrorRecoveryAttempt contains server-observed facts from the entry handler.
// It is supplied only after the existing no-answer / safe-replay checks pass.
// Elapsed starts at the central upstream dispatch, not outer Forward entry.
// Local admission probes do not count; internal HTTP dispatches do count.
type ErrorRecoveryAttempt struct {
	First                    bool
	Elapsed                  time.Duration
	UpstreamAttempts         int
	OriginalContext          context.Context
	HandlerCanSwitch         bool
	WrittenSizeBeforeForward int
}

func openAILateFailureSwitchAllowed(c *gin.Context, account *Account, failure *UpstreamFailoverError, p *model.ErrorRecoveryPolicy, attempt *ErrorRecoveryAttempt) bool {
	if attempt == nil || !attempt.First || attempt.UpstreamAttempts != 1 || !attempt.HandlerCanSwitch || attempt.OriginalContext == nil || attempt.OriginalContext.Err() != nil ||
		c == nil || c.Request == nil || c.Writer == nil || c.Request.Context().Err() != nil || IsResponseCommitted(c) ||
		account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey ||
		failure == nil || p == nil || p.Mode != "limited" || p.SameAccountRetries <= 0 || p.AccountSwitches <= 0 {
		return false
	}
	if failure.StatusCode != http.StatusBadGateway && failure.StatusCode != http.StatusServiceUnavailable && failure.StatusCode != http.StatusGatewayTimeout {
		return false
	}
	// Match the existing handler boundary, including a service-certified
	// reasoning-only failure. Ordinary written answer bytes never qualify.
	if OpenAICompactKeepaliveAdjustedWrittenSize(c) != attempt.WrittenSizeBeforeForward && !failure.SafeToFailoverAfterWrite {
		return false
	}
	// This is the authenticated middleware entry, not a request body/header or
	// a suggested group. Keep the experiment's account and group conjunctive.
	groupID := openAILateFailureSwitchGroup(c)
	if groupID <= 0 {
		return false
	}
	enabled, _ := account.Extra[openAILateFailureSwitchEnabledKey].(bool)
	if !enabled {
		return false
	}
	raw, err := json.Marshal(account.Extra[openAILateFailureSwitchGroupsKey])
	if err != nil {
		return false
	}
	var groups []int64
	if json.Unmarshal(raw, &groups) != nil {
		return false
	}
	matched := false
	for _, allowedGroup := range groups {
		if allowedGroup <= 0 {
			return false
		}
		matched = matched || allowedGroup == groupID
	}
	return matched && attempt.Elapsed >= openAILateFailureSwitchMinimum(p)
}

func openAILateFailureSwitchGroup(c *gin.Context) int64 {
	value, ok := c.Get("api_key")
	key, valid := value.(*APIKey)
	if !ok || !valid || key == nil || key.GroupID == nil {
		return 0
	}
	return *key.GroupID
}

func openAILateFailureSwitchMinimum(p *model.ErrorRecoveryPolicy) time.Duration {
	minimum := time.Duration(p.BudgetSeconds) * time.Second
	if minimum < OpenAILateFailureSwitchMinWait {
		minimum = OpenAILateFailureSwitchMinWait
	}
	return minimum
}
