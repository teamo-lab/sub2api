package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeRelayEncryptedSameAccountRetrySharesDeliveryBoundary(t *testing.T) {
	a := nativeSafetyFrame(`{"type":"response.created","response":{"id":"resp_abandoned"}}`) + nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"abandoned_reasoning"}`)
	b := nativeSafetyFrame(`{"type":"response.created","response":{"id":"resp_winner"}}`) + nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"winner_reasoning"}`)
	success := b + nativeSafetyFrame(`{"type":"response.output_text.delta","delta":"winner_answer"}`) + nativeSafetyFrame(`{"type":"response.completed","response":{"id":"resp_winner","status":"completed","usage":{"input_tokens":12,"output_tokens":7}}}`)
	for _, tc := range []struct {
		name, first, second string
		attempts            int
		successful          bool
	}{
		{"held_reasoning_retry", a + encryptedRetryBudgetFailure, success, 2, true},
		{"repeated_rejection", a + encryptedRetryBudgetFailure, b + encryptedRetryBudgetFailure, 2, false},
		{"opaque_tool", a + nativeSafetyFrame(`{"type":"response.web_search_call.in_progress","item_id":"tool_1"}`) + encryptedRetryBudgetFailure, success, 1, false},
		{"answer_delivered", a + nativeSafetyFrame(`{"type":"response.output_text.delta","delta":"already_delivered"}`) + encryptedRetryBudgetFailure, success, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.first))},
				{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.second))},
			}}
			svc, c, rec, account := encryptedBudgetFixture(t, false, context.Background(), upstream)
			svc.cfg.Gateway.TeamoRelayEnabled = true
			svc.cfg.Gateway.TeamoRelayGroupIDs = []int64{3}
			enableTeamoRelayTestGroup(c)
			result, err := svc.Forward(c.Request.Context(), c, account, []byte(encryptedRetryBudgetRequest))
			require.Len(t, upstream.bodies, tc.attempts)
			require.Equal(t, tc.successful, err == nil)
			if tc.attempts == 2 {
				require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.encrypted_content").Exists())
				require.Equal(t, "keep", gjson.GetBytes(upstream.bodies[1], "input.0.summary.0.text").String())
				require.NotContains(t, rec.Body.String(), "abandoned_reasoning")
				require.NotContains(t, rec.Body.String(), "resp_abandoned")
			}
			if tc.successful {
				require.NotNil(t, result)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.NotContains(t, rec.Body.String(), "invalid_encrypted_content")
				completedEvents := 0
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if payload, ok := strings.CutPrefix(line, "data:"); ok && gjson.Get(strings.TrimSpace(payload), "type").String() == "response.completed" {
						completedEvents++
					}
				}
				require.Equal(t, 1, completedEvents)
			} else {
				require.NotContains(t, rec.Body.String(), "response.completed")
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
			}
		})
	}
}

type nativeBudgetObservedBody struct {
	*io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func (r *nativeBudgetObservedBody) Close() error {
	r.once.Do(func() { close(r.closed) })
	return r.PipeReader.Close()
}

func TestNativeRelayEncryptedBudgetClosesHeldReasoningAndWritesOneTerminal(t *testing.T) {
	for _, keepalive := range []bool{false, true} {
		t.Run(map[bool]string{false: "json_terminal", true: "sse_terminal"}[keepalive], func(t *testing.T) {
			reader, writer := io.Pipe()
			body := &nativeBudgetObservedBody{PipeReader: reader, closed: make(chan struct{})}
			writerDone := make(chan struct{})
			calls := 0
			upstream := &encryptedBudgetUpstream{do: func(_ *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
				}
				go func() {
					defer close(writerDone)
					_, _ = io.WriteString(writer, nativeSafetyFrame(`{"type":"response.reasoning_summary_text.delta","delta":"must_remain_held"}`))
					select {
					case <-body.closed:
					case <-time.After(time.Second):
					}
					_ = writer.CloseWithError(errors.New("fixture cleanup"))
				}()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
			}}
			parent := context.Background()
			svc, c, rec, account := encryptedBudgetFixture(t, false, parent, upstream)
			svc.cfg.Gateway.TeamoRelayEnabled = true
			svc.cfg.Gateway.TeamoRelayGroupIDs = []int64{3}
			enableTeamoRelayTestGroup(c)
			if keepalive {
				c.Header("Content-Type", "text/event-stream")
				n, _ := c.Writer.WriteString(": keepalive\n\n")
				recordOpenAIStreamKeepaliveBytes(c, n)
				c.Writer.Flush()
			}
			initializeOpenAIEncryptedSemanticRetry(c, account, []byte(encryptedRetryBudgetRequest))
			budget := newOpenAIEncryptedRetryBudget(c, parent, 40*time.Millisecond)
			originalDeadline := budget.deadline
			openAIEncryptedSemanticRetryStateFor(c).budget = budget
			started := time.Now()
			_, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
			require.Error(t, err)
			require.Equal(t, 2, calls)
			require.Less(t, time.Since(started), 500*time.Millisecond)
			require.Equal(t, originalDeadline, budget.deadline)
			require.Error(t, budget.ctx.Err())
			select {
			case <-writerDone:
			case <-time.After(time.Second):
				t.Fatal("held reader was not closed")
			}
			require.NotContains(t, rec.Body.String(), "must_remain_held")
			require.NotContains(t, rec.Body.String(), "invalid_encrypted_content")
			require.NotContains(t, rec.Body.String(), "response.completed")
			if keepalive {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
			} else {
				require.Equal(t, http.StatusGatewayTimeout, rec.Code)
				require.True(t, gjson.ValidBytes(rec.Body.Bytes()))
			}
			require.Contains(t, rec.Body.String(), "encrypted_reasoning_recovery_timeout")
		})
	}
}
