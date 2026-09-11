//go:build unit

package handler

import (
	"bytes"
	"context"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"testing"
	"time"
)

type recoveryRuleRepo struct {
	service.ErrorPassthroughRepository
	rule *model.ErrorPassthroughRule
}

func (r recoveryRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return []*model.ErrorPassthroughRule{r.rule}, nil
}

type recoveryUpstream struct {
	service.HTTPUpstream
	calls           []int64
	hang            bool
	sse             bool
	typeOnly        bool
	sseHTTPError    bool
	succeedOnSecond bool
	firstSSEPayload string
	firstSSEPrefix  string
}

func (u *recoveryUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	if u.succeedOnSecond && id == 2 {
		body := `{"id":"resp_recovered","object":"response","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
		contentType := "application/json"
		if u.sse {
			contentType = "text/event-stream"
			body = "data: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
	}
	if u.hang && len(u.calls) > 1 {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	b := `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`
	if u.typeOnly {
		b = `{"error":{"type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`
	}
	status := 503
	ct := "application/json"
	if u.sse {
		status = 200
		ct = "text/event-stream"
		b = "event: error\ndata: " + b + "\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded.\"}}}\n\n"
		if u.typeOnly {
			b = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}\n\n"
		}
		if u.sseHTTPError {
			status = 503
		}
		if u.firstSSEPayload != "" {
			b = u.firstSSEPrefix + "event: error\ndata: " + u.firstSSEPayload + "\n\n"
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(bytes.NewBufferString(b))}, nil
}

func TestErrorRecoveryHandlerBareErrorBoundary(t *testing.T) {
	for _, kind := range []string{"apikey", "oauth"} {
		for _, tc := range []struct {
			name, payload, prefix string
			wantRecovery          bool
		}{
			{"server_code_before_output", `{"type":"error","error":{"code":"server_error","message":"Internal server error"}}`, "", true},
			{"upstream_code_before_output", `{"type":"error","error":{"code":"upstream_error","message":"Internal server error"}}`, "", true},
			{"server_type_before_output", `{"type":"error","error":{"type":"server_error","message":"Internal server error"}}`, "", true},
			{"server_error_after_visible_output", `{"type":"error","error":{"code":"server_error","message":"Internal server error"}}`, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n", false},
			{"unknown_error", `{"type":"error","error":{"code":"unknown_error","message":"Internal server error"}}`, "", false},
			{"invalid_request", `{"type":"error","error":{"code":"invalid_request_error","type":"server_error","message":"Bad request"}}`, "", false},
			{"policy_error", `{"type":"error","error":{"code":"server_error","type":"content_policy_violation","message":"Blocked by content policy"}}`, "", false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				u := &recoveryUpstream{sse: true, succeedOnSecond: true, firstSSEPayload: tc.payload, firstSSEPrefix: tc.prefix}
				h := newOpenAIResponsesFailoverTestHandler(t, u, kind)
				rule := &model.ErrorPassthroughRule{ID: 11, Enabled: true, Name: "bare transient", MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{502}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{kind}, Models: []string{"gpt-5.1"}, UpstreamCodes: []string{"server_error", "upstream_error"}, AccountSwitches: 1, BudgetSeconds: 10}}
				h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
				c, w := newOpenAIResponsesFailoverTestContext(t, nil)
				c.Request.Body = io.NopCloser(bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
				h.Responses(c)
				if tc.wantRecovery {
					require.Equal(t, []int64{1, 2}, u.calls)
					require.Equal(t, http.StatusOK, w.Code)
					require.Contains(t, w.Body.String(), `"id":"resp_recovered"`)
					require.Contains(t, w.Body.String(), `"status":"completed"`)
					require.NotContains(t, w.Body.String(), "Internal server error")
					return
				}
				require.Equal(t, []int64{1}, u.calls, "a permanent/unknown error or visible client output must not cause replay")
				require.NotContains(t, w.Body.String(), "resp_recovered")
				if tc.prefix != "" {
					require.Contains(t, w.Body.String(), "partial answer")
					require.Contains(t, w.Body.String(), "Internal server error")
				}
			})
		}
	}
}

func TestErrorRecoveryHandlerTypeOnly(t *testing.T) {
	for _, tc := range []struct {
		name, kind                            string
		sse, sseHTTPError, streaming, recover bool
	}{
		{"apikey_json", "apikey", false, false, false, false},
		{"oauth_json", "oauth", false, false, false, false},
		{"apikey_http503_sse", "apikey", true, true, false, false},
		{"oauth_http503_sse", "oauth", true, true, false, false},
		{"apikey_http200_sse_to_json", "apikey", true, false, false, false},
		{"oauth_http200_sse_to_json", "oauth", true, false, false, false},
		{"apikey_http200_streaming_sse", "apikey", true, false, true, false},
		{"oauth_http200_streaming_sse", "oauth", true, false, true, false},
		{"apikey_json_recovers", "apikey", false, false, false, true},
		{"oauth_sse_recovers", "oauth", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &recoveryUpstream{typeOnly: true, sse: tc.sse, sseHTTPError: tc.sseHTTPError, succeedOnSecond: tc.recover}
			h := newOpenAIResponsesFailoverTestHandler(t, u, tc.kind)
			rule := &model.ErrorPassthroughRule{ID: 9, Enabled: true, Name: "type-only transient", MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{tc.kind}, Models: []string{"gpt-5.1"}, UpstreamCodes: []string{"service_unavailable_error"}, AccountSwitches: 1, BudgetSeconds: 10}}
			h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
			c, w := newOpenAIResponsesFailoverTestContext(t, nil)
			if tc.streaming {
				c.Request.Body = io.NopCloser(bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
			}
			h.Responses(c)
			require.Equal(t, []int64{1, 2}, u.calls)
			if tc.recover {
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), `"status":"completed"`)
				require.NotContains(t, w.Body.String(), "service_unavailable_error")
				require.NotContains(t, w.Body.String(), "recovery_exhausted")
				return
			}
			require.Equal(t, 503, w.Code)
			require.Equal(t, "service_unavailable_error", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
			require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool(), "type-only failures must use the explicitly configured recovery budget")
		})
	}
}
func TestErrorRecoveryHandlerAttemptsAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		retries    int
		hang, sse  bool
		want       []int64
	}{
		{"apikey_json", "apikey", 0, false, false, []int64{1, 2}},
		{"oauth_json", "oauth", 1, false, false, []int64{1, 1, 2, 2}},
		{"oauth_sse", "oauth", 1, false, true, []int64{1, 1, 2, 2}},
		{"apikey_deadline", "apikey", 0, true, false, []int64{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &recoveryUpstream{hang: tc.hang, sse: tc.sse}
			h := newOpenAIResponsesFailoverTestHandler(t, u, tc.kind)
			budget := 10
			if tc.hang {
				budget = 1
			}
			rule := &model.ErrorPassthroughRule{ID: 9, Enabled: true, Name: "test", MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{tc.kind}, UpstreamCodes: []string{"server_is_overloaded"}, SameAccountRetries: tc.retries, AccountSwitches: 1, BudgetSeconds: budget}}
			h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
			c, w := newOpenAIResponsesFailoverTestContext(t, nil)
			start := time.Now()
			h.Responses(c)
			require.Less(t, time.Since(start), 5*time.Second)
			require.Equal(t, tc.want, u.calls)
			require.Equal(t, 503, w.Code)
			require.Equal(t, "server_is_overloaded", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
			require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool())
		})
	}
}

func TestErrorRecoveryHandlerNoNextAccountPreservesError(t *testing.T) {
	u := &recoveryUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, u, "apikey", "single")
	rule := &model.ErrorPassthroughRule{ID: 9, Enabled: true, Name: "test", MatchMode: "all", Platforms: []string{"openai"}, ErrorCodes: []int{503}, PassthroughCode: true, PassthroughBody: true, RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{"apikey"}, UpstreamCodes: []string{"server_is_overloaded"}, SameAccountRetries: 0, AccountSwitches: 1, BudgetSeconds: 10}}
	h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
	c, w := newOpenAIResponsesFailoverTestContext(t, nil)
	h.Responses(c)
	require.Equal(t, []int64{1}, u.calls)
	require.Equal(t, 503, w.Code)
	require.Equal(t, "server_is_overloaded", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
}
