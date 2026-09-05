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
	calls []int64
	hang  bool
	sse   bool
}

func (u *recoveryUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	if u.hang && len(u.calls) > 1 {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	b := `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`
	status := 503
	ct := "application/json"
	if u.sse {
		status = 200
		ct = "text/event-stream"
		b = "event: error\ndata: " + b + "\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded.\"}}}\n\n"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(bytes.NewBufferString(b))}, nil
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
