//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type combinedRecoveryUpstream struct {
	service.HTTPUpstream
	calls    []int64
	bodies   [][]byte
	hang     bool
	canceled bool
}

func (u *combinedRecoveryUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	body, _ := io.ReadAll(req.Body)
	u.bodies = append(u.bodies, body)
	status, contentType := http.StatusOK, "text/event-stream"
	responseBody := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_combined\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	switch len(u.calls) {
	case 1:
		status, contentType = http.StatusServiceUnavailable, "application/json"
		responseBody = `{"error":{"type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`
	case 2:
		responseBody = "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"invalid_encrypted_content\",\"type\":\"invalid_request_error\",\"message\":\"Encrypted content could not be verified\"}}\n\n"
	default:
		if u.hang {
			select {
			case <-req.Context().Done():
				u.canceled = true
				return nil, req.Context().Err()
			case <-time.After(3 * time.Second):
				return nil, errors.New("outer recovery budget was not preserved")
			}
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
}

// Cross-fix regression: outer type-only recovery switches accounts;
// inner encrypted repair retries that second account without renewing the budget.
func TestCombinedErrorRecoveryAndEncryptedRepair(t *testing.T) {
	for _, hang := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "outer_deadline_wins"}[hang], func(t *testing.T) {
			u := &combinedRecoveryUpstream{hang: hang}
			h := newOpenAIResponsesFailoverTestHandler(t, u, "apikey")
			rule := &model.ErrorPassthroughRule{ID: 31, Enabled: true, Name: "combined integration", MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{"apikey"}, Models: []string{"gpt-5.1"}, UpstreamCodes: []string{"service_unavailable_error"}, AccountSwitches: 1, BudgetSeconds: 1}}
			h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
			c, w := newOpenAIResponsesFailoverTestContext(t, context.Background())
			c.Request.Body = io.NopCloser(bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-cipher","summary":[{"type":"summary_text","text":"keep summary"}]},{"type":"message","role":"user","content":"synthetic followup"}]}`))
			start := time.Now()
			h.Responses(c)
			require.Less(t, time.Since(start), 2*time.Second)
			require.Equal(t, []int64{1, 2, 2}, u.calls)
			require.True(t, gjson.GetBytes(u.bodies[1], "input.0.encrypted_content").Exists())
			require.False(t, gjson.GetBytes(u.bodies[2], "input.0.encrypted_content").Exists())
			require.Equal(t, "keep summary", gjson.GetBytes(u.bodies[2], "input.0.summary.0.text").String())
			require.Equal(t, "synthetic followup", gjson.GetBytes(u.bodies[2], "input.1.content").String())
			if hang {
				require.True(t, u.canceled)
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool())
				require.Equal(t, 1, strings.Count(w.Body.String(), "recovery_exhausted"))
				require.NotContains(t, w.Body.String(), "encrypted_reasoning_recovery_timeout")
			} else {
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), "resp_combined")
				require.Equal(t, 1, strings.Count(w.Body.String(), `"type":"response.completed"`))
				require.Equal(t, 1, strings.Count(w.Body.String(), `"type":"response.output_text.delta"`))
				require.Contains(t, w.Body.String(), `"delta":"recovered answer"`)
				require.NotContains(t, w.Body.String(), "recovery_exhausted")
				require.NotContains(t, w.Body.String(), "invalid_encrypted_content")
			}
		})
	}
}
