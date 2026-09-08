package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/relay"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// scanCCStreamWithRelay guards CC chunks before handing them to the existing
// Responses/Messages adapters. A role or half a tool call must not cause those
// adapters to leak response.created/message_start and pin a failing attempt.
func (s *OpenAIGatewayService) scanCCStreamWithRelay(
	c *gin.Context, resp *http.Response, account *Account, requestID string,
	startTime time.Time, reasoningEffort *string, emit func(*apicompat.ChatCompletionsChunk),
) ccStreamScanState {
	markTeamoRelayPath(c, "chat_to_client_adapter")
	var st ccStreamScanState
	var observer relay.ChatObserver
	gate := relay.NewCommitGate(openAIFirstOutputStageMaxBytes)
	defer gate.Close()
	// Wire repair/conversion remains in Sub; Gate only owns held bytes.
	emitPrefix := func(src io.Reader) error {
		decoder := json.NewDecoder(src)
		for {
			var chunk apicompat.ChatCompletionsChunk
			if err := decoder.Decode(&chunk); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			emit(&chunk)
		}
	}
	process := func(payload string) bool {
		payload = strings.TrimSpace(payload)
		if payload == "" || strings.EqualFold(payload, "SSE-Keep-Alive") {
			return true
		}
		if modelObserver := upstreamResponseModelObserverFromContext(c); modelObserver != nil {
			modelObserver.ObserveOpenAI([]byte(payload), "")
		}
		if u := extractCCStreamUsage(payload); u != nil {
			st.Usage = *u
		}
		if err := observer.Observe(payload); err != nil {
			st.Err = err
			return false
		}
		if len(observer.Failure) > 0 {
			st.Err = s.teamoRelayChatFailure(c, resp, account, requestID, observer.Failure, st.Usage, gate.Committed())
			return false
		}
		if payload == "[DONE]" {
			st.SawDone = true
			if !gate.Committed() {
				st.Err = gate.Commit(emitPrefix)
			}
			return false
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			st.Err = relay.ErrMalformedFrame
			return false
		}
		if st.FirstTokenMs == nil && !isOpenAIChatUsageOnlyStreamChunk(payload) && chatChunkStartsResponsesOutput(&chunk) {
			ms := int(time.Since(startTime).Milliseconds())
			st.FirstTokenMs = &ms
		}
		if gate.Committed() {
			emit(&chunk)
			return true
		}
		if _, err := gate.WriteString(payload + "\n"); err != nil {
			st.Err = err
			return false
		}
		if observer.Ready {
			st.Err = gate.Commit(emitPrefix)
		}
		return st.Err == nil
	}
	effort := ""
	if reasoningEffort != nil {
		effort = *reasoningEffort
	}
	scanner := s.newTeamoRelayScanner(c, resp, gate.Committed, startTime, effort)
	defer scanner.Close()
	var parser openAICompatSSEFrameParser
	stopped := false
	frameBytes := 0
	for scanner.Scan() {
		frameBytes += len(scanner.Text()) + 1
		if frameBytes > openAIFirstOutputStageMaxBytes {
			st.Err = relay.ErrLimit
			stopped = true
			break
		}
		if scanner.Text() == "" {
			frameBytes = 0
		}
		if frame, ok := parser.AddLine(scanner.Text()); ok && !process(frame.Data) {
			stopped = true
			break
		}
	}
	if !stopped && scanner.Err() == nil {
		if frame, ok := parser.Finish(); ok {
			process(frame.Data)
		}
	}
	if st.Err == nil && scanner.Err() != nil {
		st.Err = scanner.Err()
	}
	if st.Err == nil && !observer.Complete() {
		st.Err = ErrOpenAIUpstreamStreamTruncated
	}
	if st.Err != nil && !gate.Committed() && c.Request.Context().Err() == nil &&
		!errors.Is(st.Err, context.Canceled) && !errors.Is(st.Err, context.DeadlineExceeded) {
		// Explicit upstream verdicts are already classified by Sub policy.
		if len(observer.Failure) == 0 {
			st.Err = newOpenAIRawStreamTruncatedFailoverError(c, account, requestID, st.Err)
		}
	}
	st.Committed = gate.Committed()
	return st
}

type teamoRelayStreamFailure struct {
	status    int
	errorType string
	message   string
	body      []byte
}

func (e *teamoRelayStreamFailure) Error() string { return "upstream response failed: " + e.message }

// Finish an adapter-owned failure in that adapter's protocol and response ID.
// Leaving this to the HTTP handler after emitting response.created would mint a
// second response ID. A precommit failover still returns without writing bytes.
func (s *OpenAIGatewayService) writeTeamoRelayAdapterFailure(c *gin.Context, scan ccStreamScanState, responseID, model string, anthropic bool) bool {
	if !s.teamoRelayEnabled(c) || scan.Err == nil || c.Request.Context().Err() != nil {
		return false
	}
	var failover *UpstreamFailoverError
	if errors.As(scan.Err, &failover) {
		return false
	}
	status, errorType, message := http.StatusBadGateway, "upstream_error", "Upstream stream interrupted"
	var source []byte
	var failure *teamoRelayStreamFailure
	if errors.As(scan.Err, &failure) {
		status, errorType, message, source = failure.status, failure.errorType, failure.message, failure.body
	}
	SetRouterOutcome(c, RouterOutcomeTerminalError)
	if !c.Writer.Written() && !scan.Committed {
		if anthropic {
			writeAnthropicError(c, status, errorType, message)
		} else {
			writeOpenAIResponsesFallbackError(c, status, errorType, message)
		}
		MarkResponseCommitted(c)
		return false
	}
	var err error
	if anthropic {
		payload, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{"type": errorType, "message": message}})
		_, err = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
	} else {
		_, err = io.WriteString(c.Writer, buildOpenAIResponseFailedSSE(responseID, model, source, message))
	}
	// Even a failed write must not be followed by a second attempt to synthesize
	// a terminal on the same broken connection.
	MarkResponseCommitted(c)
	if err == nil {
		c.Writer.Flush()
	}
	return err != nil
}

// Retry policy and account side effects deliberately remain in Sub. A failed
// attempt may have usage; that does not turn its cost into zero or make it safe
// to replay after committing output.
func (s *OpenAIGatewayService) teamoRelayChatFailure(c *gin.Context, resp *http.Response, account *Account, requestID string, body []byte, usage OpenAIUsage, committed bool) error {
	message := extractOpenAISSEErrorMessage(body)
	// Existing handlers do not charge failed failover attempts to the client.
	// Preserve their observed provider usage in OP logs without adding a ledger.
	logger.FromContext(c.Request.Context()).Info("teamo_relay.attempt_failure",
		zap.Bool("committed", committed), zap.Int("upstream_input_tokens", usage.InputTokens),
		zap.Int("upstream_output_tokens", usage.OutputTokens), zap.Int("upstream_cache_read_tokens", usage.CacheReadInputTokens))
	cyberHit := false
	if hit, code, msg := detectOpenAICyberPolicy(body); hit {
		cyberHit = true
		MarkOpsCyberPolicy(c, CyberPolicyMark{Code: code, Message: msg, Body: truncateString(string(body), 4096), UpstreamStatus: http.StatusOK, UpstreamInTok: usage.InputTokens, UpstreamOutTok: usage.OutputTokens})
	}
	if !committed && !cyberHit && c.Request.Context().Err() == nil && openAIStreamFailedEventShouldFailover(body, message) {
		return s.newOpenAIStreamFailoverErrorWithModel(c, account, false, requestID, body, message, c.GetString(OpsUpstreamModelKey), resp.Header)
	}
	status, errorType := openAIStreamFailedEventSemanticStatus(body, message), strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	if status < 400 {
		status = http.StatusBadGateway
	}
	if errorType == "" {
		errorType = "upstream_error"
	}
	if account != nil {
		if code, kind, msg, matched := applyOpenAIStreamFailedErrorPassthroughRule(c, account.Platform, body, message); matched {
			status, errorType, message = code, kind, msg
		}
	}
	message = s.recordOpenAIStreamUpstreamError(c, account, false, requestID, "http_error", body, message)
	return &teamoRelayStreamFailure{status: status, errorType: errorType, message: message, body: append([]byte(nil), body...)}
}
