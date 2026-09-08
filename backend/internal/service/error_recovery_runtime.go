package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"strings"
	"sync"
	"time"
)

const recoveryStateKey = "error_recovery_state"

type recoveryContextKey struct{}

type ErrorRecoveryAction int

const (
	ErrorRecoveryDefault ErrorRecoveryAction = iota
	ErrorRecoveryRetry
	ErrorRecoverySwitch
	ErrorRecoveryStop
)

type errorRecoveryState struct {
	mu         sync.Mutex
	rule       *model.ErrorPassthroughRule
	parent     context.Context
	cancel     context.CancelFunc
	stopParent func() bool
	timer      *time.Timer
	deadline   time.Time
	output     bool
	written    bool
	retries    map[int64]int
	switches   int
	code       string
	failure    *UpstreamFailoverError
}

func recoveryState(c *gin.Context) *errorRecoveryState {
	if c == nil {
		return nil
	}
	v, _ := c.Get(recoveryStateKey)
	s, _ := v.(*errorRecoveryState)
	return s
}
func recoveryContains(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), value) {
			return true
		}
	}
	return false
}
func recoveryErrorCode(body []byte) string {
	if code := recoveryErrorCodeFromJSON(body); code != "" {
		return code
	}
	// 中转类上游会把错误装在 SSE 帧里连同 5xx 状态码一起返回
	// （`event: error\ndata: {...}`）。逐个 data 载荷取 code，取最后一个
	// 非空值：错误帧总在流尾。
	if !bytes.Contains(body, []byte("data:")) {
		return ""
	}
	code := ""
	for _, line := range bytes.Split(body, []byte("\n")) {
		payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		if v := recoveryErrorCodeFromJSON(bytes.TrimSpace(payload)); v != "" {
			code = v
		}
	}
	return code
}

func recoveryErrorCodeFromJSON(body []byte) string {
	// 上游可以只返回结构化 error.type。它仅在所有 code 字段都缺失时兜底，
	// 不能读取顶层/response.type：那些字段通常是 SSE 事件或响应元数据。
	for _, path := range []string{"error.code", "response.error.code", "code", "error.type", "response.error.type"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String && v.String() != "" {
			return v.String()
		}
	}
	return ""
}
func recoveryFailureErrorCode(failure *UpstreamFailoverError) string {
	if failure.RecoveryErrorCode != nil {
		return *failure.RecoveryErrorCode
	}
	return recoveryErrorCode(failure.ResponseBody)
}

func (s *ErrorPassthroughService) matchRecoveryRule(account *Account, requestedModel string, failure *UpstreamFailoverError) *model.ErrorPassthroughRule {
	if s == nil || account == nil {
		return nil
	}
	code := recoveryFailureErrorCode(failure)
	if code == "" {
		return nil
	}
	lower := strings.ToLower(account.Platform)
	var text string
	var done bool
	for _, r := range s.getCachedRules() {
		p := r.RecoveryPolicy
		if !r.Enabled || p == nil || p.Mode == "default" || p.Validate() != nil {
			continue
		}
		if !s.platformMatchesCached(r, lower) || !recoveryContains(p.AccountTypes, account.Type) || !recoveryContains(p.Models, requestedModel) || !recoveryContains(p.UpstreamCodes, code) {
			continue
		}
		if s.ruleMatchesOptimized(r, failure.StatusCode, failure.ResponseBody, &text, &done) {
			return r.ErrorPassthroughRule
		}
	}
	return nil
}

// ApplyErrorRecovery is called only where replay is still safe, before default
// retry/switch handling. The first matching rule owns one immutable request budget.
func ApplyErrorRecovery(c *gin.Context, account *Account, requestedModel string, failure *UpstreamFailoverError) ErrorRecoveryAction {
	return applyErrorRecovery(c, account, requestedModel, failure, nil)
}

// ApplyErrorRecoveryAfterAttempt uses the same rule, timer and switch counter as
// ApplyErrorRecovery. The optional HK43 experiment can only skip the first
// account's retry; it never changes the cached policy or starts a second budget.
func ApplyErrorRecoveryAfterAttempt(c *gin.Context, account *Account, requestedModel string, failure *UpstreamFailoverError, attempt ErrorRecoveryAttempt) ErrorRecoveryAction {
	return applyErrorRecovery(c, account, requestedModel, failure, &attempt)
}

func applyErrorRecovery(c *gin.Context, account *Account, requestedModel string, failure *UpstreamFailoverError, attempt *ErrorRecoveryAttempt) ErrorRecoveryAction {
	if c == nil || c.Request == nil || account == nil || failure == nil {
		return ErrorRecoveryDefault
	}
	s := recoveryState(c)
	firstFailure := s == nil
	if s == nil {
		svc := getBoundErrorPassthroughService(c)
		rule := svc.matchRecoveryRule(account, requestedModel, failure)
		if rule == nil {
			return ErrorRecoveryDefault
		}
		parent := c.Request.Context()
		ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
		ctx = context.WithValue(ctx, recoveryContextKey{}, true)
		s = &errorRecoveryState{rule: rule, parent: parent, cancel: cancel, deadline: time.Now().Add(time.Duration(rule.RecoveryPolicy.BudgetSeconds) * time.Second), retries: make(map[int64]int), code: recoveryFailureErrorCode(failure), failure: failure}
		s.timer = time.AfterFunc(time.Until(s.deadline), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.output {
				cancel()
			}
		})
		s.stopParent = context.AfterFunc(parent, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.output {
				cancel()
			}
		})
		c.Set(recoveryStateKey, s)
		c.Request = c.Request.WithContext(ctx)
	}
	s.failure = failure
	if s.rule.RecoveryPolicy.Mode == "return" || RecoveryBudgetExpired(c) {
		return ErrorRecoveryStop
	}
	p := s.rule.RecoveryPolicy
	skipRetry := firstFailure && s.switches < p.AccountSwitches && openAILateFailureSwitchAllowed(c, account, failure, p, attempt)
	if !skipRetry && s.retries[account.ID] < p.SameAccountRetries {
		s.retries[account.ID]++
		delay := 500 * time.Millisecond
		for i := 1; i < s.retries[account.ID] && delay < 8*time.Second; i++ {
			delay *= 2
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-c.Request.Context().Done():
			return ErrorRecoveryStop
		case <-timer.C:
			return ErrorRecoveryRetry
		}
	}
	if s.switches >= p.AccountSwitches {
		return ErrorRecoveryStop
	}
	s.switches++
	if skipRetry {
		logger.FromContext(c.Request.Context()).Info("openai.late_failure_retry_skipped",
			zap.Int64("account_id", account.ID), zap.Int64("group_id", openAILateFailureSwitchGroup(c)),
			zap.Int64("rule_id", s.rule.ID), zap.Int("upstream_status", failure.StatusCode),
			zap.Int64("attempt_elapsed_ms", attempt.Elapsed.Milliseconds()),
			zap.Int64("minimum_wait_ms", openAILateFailureSwitchMinimum(p).Milliseconds()),
			zap.Int("switch_count", s.switches), zap.Int("budget_seconds", p.BudgetSeconds))
	}
	return ErrorRecoverySwitch
}
func RecoveryBudgetExpired(c *gin.Context) bool {
	s := recoveryState(c)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.output && !time.Now().Before(s.deadline)
}

// ReserveErrorRecoveryAccountSwitch shares an existing request's switch budget
// with a local admission reselect. It never creates a recovery state, renews its
// deadline, replaces its upstream failure, or spends a same-account retry.
func ReserveErrorRecoveryAccountSwitch(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Context().Err() != nil {
		return false
	}
	s := recoveryState(c)
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rule == nil || s.rule.RecoveryPolicy == nil || s.rule.RecoveryPolicy.Mode != "limited" || s.output || !time.Now().Before(s.deadline) || s.parent.Err() != nil {
		return false
	}
	if s.switches >= s.rule.RecoveryPolicy.AccountSwitches {
		return false
	}
	s.switches++
	return true
}

// Semantic output ends recovery. Keep the successful stream alive beyond budget.
func CompleteErrorRecovery(c *gin.Context) {
	completeOpenAIEncryptedSemanticRetry(c)
	s := recoveryState(c)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = true
	s.timer.Stop()
	s.stopParent()
}
func CloseErrorRecovery(c *gin.Context) {
	s := recoveryState(c)
	if s == nil {
		return
	}
	s.timer.Stop()
	s.stopParent()
	s.cancel()
	if c.Request != nil {
		c.Request = c.Request.WithContext(s.parent)
	}
}
func WriteRecoveryBudgetError(c *gin.Context) bool {
	if !RecoveryBudgetExpired(c) {
		return false
	}
	WriteErrorRecoveryExhausted(c)
	return true
}
func WriteActiveErrorRecovery(c *gin.Context) bool {
	if recoveryState(c) == nil {
		return false
	}
	WriteErrorRecoveryExhausted(c)
	return true
}
func WriteErrorRecoveryExhausted(c *gin.Context) {
	s := recoveryState(c)
	if s == nil || s.parent.Err() != nil || s.written {
		return
	}
	s.written = true
	s.timer.Stop()
	status := s.failure.ClientStatusCode
	if status < 400 {
		status = s.failure.StatusCode
	}
	if status < 400 {
		status = 502
	}
	rule := s.rule
	if !rule.PassthroughCode && rule.ResponseCode != nil {
		status = *rule.ResponseCode
	}
	message := ExtractUpstreamErrorMessage(s.failure.ResponseBody)
	if message == "" {
		message = s.failure.ClientMessage
	}
	if message == "" {
		message = "Upstream request failed"
	}
	if !rule.PassthroughBody && rule.CustomMessage != nil {
		message = *rule.CustomMessage
	}
	payload := gin.H{"error": gin.H{"type": "server_error", "code": s.code, "message": message, "recovery_exhausted": true, "recovery_rule_id": rule.ID}}
	// Retain diagnostics and SLA accounting; never cool down or disable an account.
	c.Set("error_recovery_rule_id", rule.ID)
	if c.Writer.Written() {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		c.Writer.Flush()
	} else {
		c.JSON(status, payload)
	}
	MarkResponseCommitted(c)
}
