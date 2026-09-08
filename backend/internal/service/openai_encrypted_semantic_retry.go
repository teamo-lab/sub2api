package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIEncryptedSemanticRetryKey = "openai_encrypted_semantic_retry"
const openAIEncryptedSemanticRetryBudget = 30 * time.Second

type openAIEncryptedSemanticRetryState struct {
	eligible bool
	tried    bool
	applied  bool
	budget   *openAIEncryptedRetryBudget
}

type openAIEncryptedRetryBudget struct {
	mu         sync.Mutex
	ctx        context.Context
	parent     context.Context
	cancel     context.CancelFunc
	stopParent func() bool
	timer      *time.Timer
	deadline   time.Time
	output     bool
}

// This is a separate attempt-local budget, not a new error-rule state. A stricter
// outer recovery deadline wins and its budget is never renewed or overwritten.
func newOpenAIEncryptedRetryBudget(c *gin.Context, parent context.Context, maximum time.Duration) *openAIEncryptedRetryBudget {
	deadline := time.Now().Add(maximum)
	if existing, ok := parent.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	if outer := recoveryState(c); outer != nil {
		outer.mu.Lock()
		if !outer.output && outer.deadline.Before(deadline) {
			deadline = outer.deadline
		}
		outer.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	ctx = context.WithValue(ctx, recoveryContextKey{}, true)
	budget := &openAIEncryptedRetryBudget{ctx: ctx, parent: parent, cancel: cancel, deadline: deadline}
	cancelBeforeOutput := func() {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if !budget.output {
			cancel()
		}
	}
	budget.timer = time.AfterFunc(time.Until(deadline), cancelBeforeOutput)
	budget.stopParent = context.AfterFunc(parent, cancelBeforeOutput)
	return budget
}

func openAIEncryptedSemanticRetryContext(c *gin.Context, original context.Context) context.Context {
	state := openAIEncryptedSemanticRetryStateFor(c)
	if state == nil || !state.applied {
		return original
	}
	if state.budget == nil {
		state.budget = newOpenAIEncryptedRetryBudget(c, original, openAIEncryptedSemanticRetryBudget)
	}
	return state.budget.ctx
}

func completeOpenAIEncryptedSemanticRetry(c *gin.Context) {
	state := openAIEncryptedSemanticRetryStateFor(c)
	if state == nil || state.budget == nil {
		return
	}
	budget := state.budget
	budget.mu.Lock()
	defer budget.mu.Unlock()
	budget.output = true
	budget.timer.Stop()
	budget.stopParent()
}

func finishOpenAIEncryptedSemanticRetry(c *gin.Context) error {
	state := openAIEncryptedSemanticRetryStateFor(c)
	if state == nil || state.budget == nil {
		return nil
	}
	budget := state.budget
	budget.mu.Lock()
	interrupted := !budget.output && (!time.Now().Before(budget.deadline) || budget.ctx.Err() != nil)
	budget.mu.Unlock()
	budget.timer.Stop()
	budget.stopParent()
	budget.cancel()
	state.budget = nil
	state.applied = false
	if !interrupted {
		return nil
	}
	// A client cancellation or an existing recovery budget remains owned by its
	// original handler. Do not emit a second response or replace that rule's state.
	if err := budget.parent.Err(); err != nil {
		return err
	}
	if RecoveryBudgetExpired(c) {
		return context.DeadlineExceeded
	}
	if IsResponseCommitted(c) {
		return nil
	}
	const message = "Encrypted reasoning recovery timed out before output"
	const payload = `{"error":{"type":"upstream_error","code":"encrypted_reasoning_recovery_timeout","message":"Encrypted reasoning recovery timed out before output"}}`
	setOpsUpstreamError(c, http.StatusGatewayTimeout, message, payload)
	if c.Writer.Written() {
		_, _ = fmt.Fprint(c.Writer, buildOpenAIResponseFailedSSE("", "", []byte(payload), message))
		c.Writer.Flush()
	} else {
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusGatewayTimeout, "application/json", []byte(payload))
	}
	MarkResponseCommitted(c)
	return errors.New(message)
}

type openAIEncryptedSemanticRetrySignal struct{}

func (*openAIEncryptedSemanticRetrySignal) Error() string {
	return "upstream rejected encrypted reasoning before output"
}

// The new SSE/passthrough recovery never discards opaque conversation state.
// It only removes a rejected reasoning cipher when its plaintext summary exists.
// Keep every message, summary and tool item byte-for-byte otherwise.
func openAIEncryptedReasoningSummaryRetryBody(body []byte) ([]byte, bool) {
	paths, eligible := openAIEncryptedReasoningSummaryRetryPaths(body)
	if !eligible {
		return body, false
	}
	updated := body
	for _, path := range paths {
		var err error
		updated, err = sjson.DeleteBytes(updated, path)
		if err != nil {
			return body, false
		}
	}
	return updated, true
}

func openAIEncryptedReasoningSummaryRetryPaths(body []byte) ([]string, bool) {
	if !bytes.Contains(body, []byte(`"encrypted_content"`)) || !gjson.ValidBytes(body) || strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return nil, false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil, false
	}
	var paths []string
	for i, item := range input.Array() {
		kind := item.Get("type").String()
		if kind == "compaction" || kind == "item_reference" {
			return nil, false
		}
		cipher := item.Get("encrypted_content")
		if !cipher.Exists() || cipher.Type == gjson.Null || cipher.String() == "" {
			continue
		}
		if kind != "reasoning" || cipher.Type != gjson.String {
			return nil, false
		}
		summary := item.Get("summary")
		if !summary.IsArray() || len(summary.Array()) == 0 {
			return nil, false
		}
		for _, part := range summary.Array() {
			text := part.Get("text")
			if part.Get("type").String() != "summary_text" || text.Type != gjson.String || strings.TrimSpace(text.String()) == "" {
				return nil, false
			}
		}
		paths = append(paths, "input."+strconv.Itoa(i)+".encrypted_content")
	}
	return paths, len(paths) > 0
}

func initializeOpenAIEncryptedSemanticRetry(c *gin.Context, account *Account, body []byte) {
	if c == nil {
		return
	}
	state := openAIEncryptedSemanticRetryStateFor(c)
	if state == nil {
		state = &openAIEncryptedSemanticRetryState{}
		c.Set(openAIEncryptedSemanticRetryKey, state)
	}
	state.eligible = false
	if state.tried || account == nil || !account.IsOpenAI() {
		return
	}
	_, eligible := openAIEncryptedReasoningSummaryRetryPaths(body)
	state.eligible = eligible
}

func openAIEncryptedSemanticRetryStateFor(c *gin.Context) *openAIEncryptedSemanticRetryState {
	if c == nil {
		return nil
	}
	v, _ := c.Get(openAIEncryptedSemanticRetryKey)
	state, _ := v.(*openAIEncryptedSemanticRetryState)
	return state
}

func newOpenAIEncryptedSemanticRetrySignal(c *gin.Context, payload []byte) error {
	state := openAIEncryptedSemanticRetryStateFor(c)
	if state == nil || !state.eligible || state.tried || c.Request == nil || c.Request.Context().Err() != nil {
		return nil
	}
	if !gjson.ValidBytes(payload) {
		kind, terminal, ok := extractOpenAISSETerminalEvent(string(payload))
		if !ok || (kind != "response.failed" && kind != "error") {
			return nil
		}
		payload = terminal
	}
	for _, path := range []string{"error.code", "response.error.code"} {
		if gjson.GetBytes(payload, path).String() == "invalid_encrypted_content" {
			return &openAIEncryptedSemanticRetrySignal{}
		}
	}
	return nil
}

func consumeOpenAIEncryptedSemanticRetry(c *gin.Context, body []byte, failure error, account *Account) ([]byte, bool) {
	var signal *openAIEncryptedSemanticRetrySignal
	state := openAIEncryptedSemanticRetryStateFor(c)
	if !errors.As(failure, &signal) || state == nil || state.tried || account == nil || c.Request == nil || c.Request.Context().Err() != nil {
		return body, false
	}
	state.tried = true
	retryBody, changed := openAIEncryptedReasoningSummaryRetryBody(body)
	if !changed {
		return body, false
	}
	state.applied = true
	if !c.Writer.Written() {
		// The rejected SSE attempt staged this header without writing bytes.
		// Let the retry's actual response choose JSON versus SSE afresh.
		c.Writer.Header().Del("Content-Type")
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, UpstreamStatusCode: 400,
		Kind: "same_account_retry", Message: "invalid_encrypted_content: preserving reasoning summary for one retry",
		Detail: `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error"}}`,
	})
	return retryBody, true
}
