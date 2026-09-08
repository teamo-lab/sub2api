package service

import (
	"bytes"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIEncryptedSemanticRetryKey = "openai_encrypted_semantic_retry"

type openAIEncryptedSemanticRetryState struct {
	eligible bool
	tried    bool
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
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, UpstreamStatusCode: 400,
		Kind: "same_account_retry", Message: "invalid_encrypted_content: preserving reasoning summary for one retry",
		Detail: `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error"}}`,
	})
	return retryBody, true
}
