package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type nativeAuditBody struct {
	reader *strings.Reader
	tail   error
}

func (r *nativeAuditBody) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF && r.tail != nil {
		return 0, r.tail
	}
	return n, err
}

func (r *nativeAuditBody) Close() error { return nil }

// OFF retains the audited 658 behavior. ON recovers held reasoning without
// leaking the abandoned attempt; other streaming adapters keep their policy.
func TestNativeRelayOffOnCommitBoundary(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_native_audit"}}`
	reasoning := `{"type":"response.reasoning_summary_text.delta","delta":"AUDIT_REASONING"}`
	answer := `{"type":"response.output_text.delta","delta":"AUDIT_ANSWER"}`
	failed := `{"type":"response.failed","response":{"id":"resp_native_audit","error":{"code":"server_error","message":"upstream temporarily unavailable"}}}`
	errFrame := `{"type":"error","error":{"status_code":503,"code":"server_error","message":"temporarily unavailable"}}`
	complete := `{"type":"response.completed","response":{"id":"resp_native_audit","status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`
	readFailure := errors.New("audit upstream connection reset")
	cases := []struct {
		name       string
		events     []string
		tail       error
		offRetry   bool
		onRetry    bool
		wantError  bool
		wantOutput bool
	}{
		{"preamble_failed", []string{created, failed}, nil, true, true, true, false},
		{"preamble_error503", []string{created, errFrame}, nil, true, true, true, false},
		{"preamble_eof", []string{created}, nil, true, true, true, false},
		{"preamble_read_error", []string{created}, readFailure, true, true, true, false},
		{"reasoning_failed", []string{created, reasoning, failed}, nil, true, true, true, true},
		{"reasoning_eof", []string{created, reasoning}, nil, true, true, true, true},
		{"reasoning_read_error", []string{created, reasoning}, readFailure, false, true, true, true},
		{"answer_failed", []string{created, answer, failed}, nil, false, false, true, true},
		{"reasoning_answer_success", []string{created, reasoning, answer, complete}, nil, false, false, false, true},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relay_%t", tc.name, enabled), func(t *testing.T) {
				groupID := int64(3)
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: enabled, TeamoRelayGroupIDs: []int64{groupID}}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
				var wire strings.Builder
				for _, value := range tc.events {
					var event struct {
						Type string `json:"type"`
					}
					require.NoError(t, json.Unmarshal([]byte(value), &event))
					fmt.Fprintf(&wire, "event: %s\ndata: %s\n\n", event.Type, value)
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &nativeAuditBody{reader: strings.NewReader(wire.String()), tail: tc.tail}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
				var failover *UpstreamFailoverError
				isFailover := errors.As(err, &failover)
				wantRetry := tc.offRetry
				if enabled {
					wantRetry = tc.onRetry
				}
				require.Equal(t, tc.wantError, err != nil)
				require.Equal(t, wantRetry, isFailover, "unexpected failover result: %v", err)
				wantOutput := tc.wantOutput && !(enabled && wantRetry)
				require.Equal(t, wantOutput, rec.Body.Len() > 0)
				eventCounts := map[string]int{}
				for _, line := range strings.Split(rec.Body.String(), "\n") {
					if payload, ok := strings.CutPrefix(line, "data: "); ok {
						eventCounts[gjson.Get(payload, "type").String()]++
					}
				}
				row := map[string]any{"case": tc.name, "relay_enabled": enabled, "failover_proposed": isFailover, "safe_after_write": failover != nil && failover.SafeToFailoverAfterWrite, "writer_bytes": rec.Body.Len(), "reasoning_delta_events": eventCounts["response.reasoning_summary_text.delta"], "answer_delta_events": eventCounts["response.output_text.delta"], "completed_events": eventCounts["response.completed"], "error": err != nil, "first_token_known": result != nil && result.firstTokenMs != nil}
				encoded, _ := json.Marshal(row)
				t.Logf("NATIVE_OFF_ON_AUDIT %s", encoded)
			})
		}
	}
}
