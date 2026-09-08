//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The only execution doubles are storage/Redis and HTTPUpstream. Both entry
// handlers, selectors, profit checks and the production rule timer are intact.
type lateFailureCache struct {
	helperConcurrencyCacheStub
	ids []int64
}

func (c *lateFailureCache) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
	c.accountAcquireCalls++
	return true, nil
}

type lateFailureUsage struct {
	service.UsageLogRepository
	rows []*service.UsageLog
}

func (u *lateFailureUsage) Create(_ context.Context, r *service.UsageLog) (bool, error) {
	u.rows = append(u.rows, r)
	return true, nil
}

type lateFailureCall struct {
	ID int64
	At time.Time
}
type lateFailureUpstream struct {
	service.HTTPUpstream
	mu                   sync.Mutex
	calls                []lateFailureCall
	firstFailureAt       time.Time
	firstDelay           time.Duration
	primaryID            int64
	failureStatus        int
	answerBeforeFailure  bool
	cancelAtFirstFailure context.CancelFunc
}
type lateFailureBody struct {
	reader io.Reader
	delay  time.Duration
	once   sync.Once
	onRead func()
}

func (b *lateFailureBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		time.Sleep(b.delay)
		if b.onRead != nil {
			b.onRead()
		}
	})
	return b.reader.Read(p)
}
func (*lateFailureBody) Close() error { return nil }
func (u *lateFailureUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, lateFailureCall{id, time.Now()})
	n := len(u.calls)
	u.mu.Unlock()
	if id == u.primaryID && n > 1 {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	failure := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_fixture_A\",\"status\":\"failed\",\"error\":{\"code\":\"upstream_error\",\"message\":\"synthetic long upstream failure\"}}}\n\n"
	if id == u.primaryID {
		if u.answerBeforeFailure {
			failure = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"committed fixture answer\",\"output_index\":0,\"content_index\":0}\n\n" + failure
		}
		body := &lateFailureBody{reader: strings.NewReader(failure), delay: u.firstDelay, onRead: func() {
			u.firstFailureAt = time.Now()
			if u.cancelAtFirstFailure != nil {
				u.cancelAtFirstFailure()
			}
		}}
		status := 200
		if u.failureStatus != 0 {
			status = u.failureStatus
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}
	time.Sleep(2 * time.Second)
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"B rescued this request\",\"output_index\":0,\"content_index\":0}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture_B\",\"object\":\"response\",\"created_at\":1700000000,\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"B rescued this request\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func (u *lateFailureUpstream) ids() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	r := []int64{}
	for _, c := range u.calls {
		r = append(r, c.ID)
	}
	return r
}

func lateFailureRule(retries int) *model.ErrorPassthroughRule {
	return &model.ErrorPassthroughRule{ID: 4, Name: "production rule4 fixture", Enabled: true, Priority: 4, MatchMode: "any", Platforms: []string{"openai"}, ErrorCodes: []int{502, 503}, PassthroughCode: true, PassthroughBody: true,
		RecoveryPolicy: &model.ErrorRecoveryPolicy{Mode: "limited", AccountTypes: []string{"apikey"}, Models: []string{}, UpstreamCodes: []string{"upstream_error", "server_error", "stream_read_error", "stream_timeout", "context_canceled", "service_unavailable_error", "bad_gateway", "gateway_timeout"}, SameAccountRetries: retries, AccountSwitches: 3, BudgetSeconds: 30}}
}

func newLateFailureFixture(t *testing.T, enabled bool, scenario, route string) func(*testing.T) (*lateFailureUpstream, *httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	u := &lateFailureUpstream{primaryID: 22, firstDelay: 5 * time.Minute, answerBeforeFailure: scenario == "answer_committed"}
	if scenario == "account_mismatch" {
		u.primaryID = 24
	}
	if scenario == "short_failure" {
		u.firstDelay = 59 * time.Second
	}
	if scenario == "floor_equal" {
		u.firstDelay = 60 * time.Second
	}
	if scenario == "budget_above_floor" {
		u.firstDelay = 119 * time.Second
	}
	if scenario == "budget_equal" {
		u.firstDelay = 120 * time.Second
	}
	if scenario == "429" {
		u.failureStatus = 429
	}
	if scenario == "503" {
		u.failureStatus = 503
	}
	if scenario == "504" {
		u.failureStatus = 504
	}
	cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{scenario != "user_rejected"}}}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: time.Second, FallbackMaxWaiting: 3, FallbackWaitTimeout: time.Second}
	aRate, bRate := 0.25, 0.20
	if scenario == "profit_rejected" {
		bRate = 0.9
	}
	extra := map[string]any{"openai_late_failure_switch_enabled": enabled, "openai_late_failure_switch_group_ids": []int64{3}}
	if scenario == "account_mismatch" {
		extra = nil
	}
	if scenario == "missing_switch" {
		delete(extra, "openai_late_failure_switch_enabled")
	}
	if scenario == "malformed_boolean" {
		extra["openai_late_failure_switch_enabled"] = "true"
	}
	if scenario == "malformed_groups" {
		extra["openai_late_failure_switch_group_ids"] = []string{"3"}
	}
	if scenario == "allowlist_mismatch" {
		extra["openai_late_failure_switch_group_ids"] = []int64{2}
	}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: u.primaryID, Name: "synthetic A", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1, RateMultiplier: &aRate, Extra: extra, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}}},
		{ID: 23, Name: "synthetic B", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 10, RateMultiplier: &bRate, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}}},
	}}
	usage := &lateFailureUsage{}
	h := newOpenAIResponsesFailoverTestHandler(t, u, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, u, service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
	rule := lateFailureRule(1)
	if scenario == "no_switch" {
		rule.RecoveryPolicy.AccountSwitches = 0
	}
	if scenario == "handler_no_switch" {
		h.maxAccountSwitches = 0
	}
	if scenario == "budget_above_floor" || scenario == "budget_equal" {
		rule.RecoveryPolicy.BudgetSeconds = 120
	}
	if scenario == "504" {
		rule.ErrorCodes = append(rule.ErrorCodes, 504)
	}
	h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: rule}, nil)
	// Production constructors own background cache janitors. Keep them outside
	// synctest; only the real request and its own timers run in virtual time.
	return func(t *testing.T) (*lateFailureUpstream, *httptest.ResponseRecorder, time.Duration) {
		groupID := int64(3)
		if scenario == "group_mismatch" {
			groupID = 2
		}
		group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 0.56, SubscriptionType: service.SubscriptionTypeStandard, ProfitControlEnabled: true, ProfitMinMargin: 0.46}
		ctx := context.WithValue(context.Background(), ctxkey.Group, group)
		if scenario == "client_canceled" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			u.cancelAtFirstFailure = cancel
		}
		stream := scenario == "answer_committed"
		payload := fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"messages":[{"role":"user","content":"synthetic only"}]}`, stream)
		if route == "responses" {
			payload = fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"input":"synthetic only"}`, stream)
		}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+route, bytes.NewBufferString(payload)).WithContext(ctx)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 901, UserID: 902, GroupID: &groupID, Group: group, User: &service.User{ID: 902}})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 902, Concurrency: 1})
		started := time.Now()
		if route == "responses" {
			h.Responses(c)
		} else {
			h.ChatCompletions(c)
		}
		elapsed := time.Since(started)
		require.Equal(t, 1, cache.userAcquireCalls)
		if scenario == "user_rejected" {
			require.Equal(t, 0, cache.userReleaseCalls)
		} else {
			require.Equal(t, 1, cache.userReleaseCalls)
		}
		require.Equal(t, cache.accountAcquireCalls, cache.accountReleaseCalls, "all acquired slots released exactly once")
		if strings.Contains(w.Body.String(), "B rescued this request") {
			require.Len(t, usage.rows, 1)
			require.Equal(t, int64(23), usage.rows[0].AccountID)
		} else if scenario == "answer_committed" && route == "chat/completions" {
			require.Len(t, usage.rows, 1, "preserve the original partial result once; never invent a B usage row")
			require.Equal(t, int64(22), usage.rows[0].AccountID)
		} else {
			require.Empty(t, usage.rows)
		}
		if scenario == "client_canceled" {
			require.Equal(t, statusClientClosedRequest, c.Writer.Status())
		}
		afterFailure := time.Duration(0)
		if !u.firstFailureAt.IsZero() {
			afterFailure = time.Since(u.firstFailureAt)
		}
		t.Logf("route=%s scenario=%s enabled=%t calls=%v first_wait=%s elapsed=%s after_first_failure=%s status=%d", route, scenario, enabled, u.ids(), u.firstDelay, elapsed, afterFailure, c.Writer.Status())
		return u, w, elapsed
	}
}

func TestOpenAILateFailureSwitchHandler_RealRule4Counterexample(t *testing.T) {
	for _, route := range []string{"responses", "chat/completions"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled_%t", route, enabled), func(t *testing.T) {
				run := newLateFailureFixture(t, enabled, "normal", route)
				synctest.Test(t, func(t *testing.T) {
					u, w, elapsed := run(t)
					if !enabled {
						require.Equal(t, []int64{22, 22}, u.ids())
						require.Equal(t, 502, w.Code)
						require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool())
						require.Equal(t, int64(4), gjson.GetBytes(w.Body.Bytes(), "error.recovery_rule_id").Int())
						require.Equal(t, 5*time.Minute+30*time.Second, elapsed)
						require.Equal(t, 500*time.Millisecond, u.calls[1].At.Sub(u.firstFailureAt))
					} else {
						require.Equal(t, []int64{22, 23}, u.ids())
						require.Equal(t, 200, w.Code)
						require.Contains(t, w.Body.String(), "B rescued this request")
						require.NotContains(t, w.Body.String(), "recovery_exhausted")
						require.Equal(t, 5*time.Minute+2*time.Second, elapsed)
					}
				})
			})
		}
	}
}

func TestOpenAILateFailureSwitchHandler_ScopeAndDuration(t *testing.T) {
	for _, route := range []string{"responses", "chat/completions"} {
		for _, scenario := range []string{"missing_switch", "malformed_boolean", "malformed_groups", "allowlist_mismatch", "group_mismatch", "account_mismatch", "short_failure", "floor_equal", "budget_above_floor", "budget_equal", "503", "504", "no_switch", "handler_no_switch"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				run := newLateFailureFixture(t, true, scenario, route)
				synctest.Test(t, func(t *testing.T) {
					u, w, elapsed := run(t)
					qualifies := scenario == "floor_equal" || scenario == "budget_equal" || scenario == "503" || scenario == "504"
					if qualifies {
						require.Equal(t, []int64{22, 23}, u.ids())
						require.Equal(t, 200, w.Code)
						require.Equal(t, u.firstDelay+2*time.Second, elapsed)
					} else {
						require.Equal(t, []int64{u.primaryID, u.primaryID}, u.ids())
						require.Equal(t, 502, w.Code)
						budget := 30 * time.Second
						if scenario == "budget_above_floor" {
							budget = 120 * time.Second
						}
						require.Equal(t, u.firstDelay+budget, elapsed)
						require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool())
					}
				})
			})
		}
	}
}

func TestOpenAILateFailureSwitchHandler_RetainsExistingGates(t *testing.T) {
	for _, route := range []string{"responses", "chat/completions"} {
		for _, scenario := range []string{"answer_committed", "client_canceled", "user_rejected", "profit_rejected"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				run := newLateFailureFixture(t, true, scenario, route)
				synctest.Test(t, func(t *testing.T) {
					u, w, _ := run(t)
					if scenario == "user_rejected" {
						require.Empty(t, u.ids())
						require.Equal(t, 429, w.Code)
					} else {
						require.Equal(t, []int64{22}, u.ids())
					}
					require.NotContains(t, w.Body.String(), "B rescued this request")
					if scenario == "answer_committed" {
						require.Contains(t, w.Body.String(), "committed fixture answer")
					}
					if scenario == "client_canceled" {
						require.Empty(t, w.Body.String())
					}
					if scenario == "profit_rejected" {
						require.True(t, gjson.GetBytes(w.Body.Bytes(), "error.recovery_exhausted").Bool())
					}
				})
			})
		}
	}
}

func TestOpenAILateFailureSwitchHandler_429Unchanged(t *testing.T) {
	for _, route := range []string{"responses", "chat/completions"} {
		t.Run(route, func(t *testing.T) {
			baseline := newLateFailureFixture(t, false, "429", route)
			enabled := newLateFailureFixture(t, true, "429", route)
			var a, b *lateFailureUpstream
			var aw, bw *httptest.ResponseRecorder
			var ad, bd time.Duration
			// Each bubble starts at the same clock value. The existing Chat
			// bridge generates created locally; compare its actual bytes without
			// stripping timestamps or comparing at different virtual times.
			synctest.Test(t, func(t *testing.T) {
				a, aw, ad = baseline(t)
			})
			synctest.Test(t, func(t *testing.T) { b, bw, bd = enabled(t) })
			require.Equal(t, a.ids(), b.ids())
			require.Equal(t, aw.Code, bw.Code)
			require.Equal(t, aw.Body.String(), bw.Body.String())
			require.Equal(t, ad, bd)
		})
	}
}
