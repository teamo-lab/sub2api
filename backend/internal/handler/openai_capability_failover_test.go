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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 2026-11-03 codebot（上游本身是 sub2api）对 gpt-6.1-terra 回的原文。
const capabilityModelUnsupportedMessage = `Model "gpt-6.1-terra" is not supported by any configured account in this group`

const capabilityToolChoiceMessage = "basispoints response violates tool_choice"

type capabilityFailure string

const (
	capabilityModel404         capabilityFailure = "model_404"
	capabilityToolChoiceHTTP   capabilityFailure = "tool_choice_http_502"
	capabilityToolChoiceStream capabilityFailure = "tool_choice_stream"
	capabilityEdgeBlock403     capabilityFailure = "cloudflare_403_1010"
)

// capabilityUpstream: accounts listed in failing answer with the given
// "this account can't serve this request" error; every other account answers.
type capabilityUpstream struct {
	service.HTTPUpstream
	mu      sync.Mutex
	calls   []int64
	failing map[int64]bool
	failure capabilityFailure
}

func (u *capabilityUpstream) Do(_ *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, id)
	u.mu.Unlock()
	if u.failing[id] {
		switch u.failure {
		case capabilityModel404:
			body := `{"error":{"type":"model_not_found","message":"Model \"gpt-6.1-terra\" is not supported by any configured account in this group"}}`
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case capabilityToolChoiceHTTP:
			body := `{"error":{"type":"upstream_error","message":"` + capabilityToolChoiceMessage + `"}}`
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case capabilityToolChoiceStream:
			body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_tc\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_tc\",\"status\":\"failed\",\"output\":[],\"error\":{\"type\":\"upstream_error\",\"message\":\"" + capabilityToolChoiceMessage + "\"}}}\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		case capabilityEdgeBlock403:
			return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"text/plain; charset=UTF-8"}}, Body: io.NopCloser(strings.NewReader("error code: 1010"))}, nil
		}
	}
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"healthy account answered\",\"output_index\":0,\"content_index\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"created_at\":1700000000,\"model\":\"gpt-6.1-terra\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"healthy account answered\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (u *capabilityUpstream) ids() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...)
}

// capabilityPenaltyRepo records every account-state write the rate limiter
// could make, so the tests can assert the failing account was not punished.
type capabilityPenaltyRepo struct {
	openAIImagesFailoverAccountRepo
	mu        sync.Mutex
	penalties []string
}

func (r *capabilityPenaltyRepo) record(kind string, id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.penalties = append(r.penalties, fmt.Sprintf("%s:%d", kind, id))
}

func (r *capabilityPenaltyRepo) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	r.record("temp_unschedulable", id)
	return nil
}

func (r *capabilityPenaltyRepo) SetError(_ context.Context, id int64, _ string) error {
	r.record("error", id)
	return nil
}

func (r *capabilityPenaltyRepo) SetModelRateLimit(_ context.Context, id int64, _ string, _ time.Time, _ ...string) error {
	r.record("model_rate_limit", id)
	return nil
}

func (r *capabilityPenaltyRepo) SetOverloaded(_ context.Context, id int64, _ time.Time) error {
	r.record("overloaded", id)
	return nil
}

func (r *capabilityPenaltyRepo) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.penalties...)
}

type capabilityHarness struct {
	h    *OpenAIGatewayHandler
	repo *capabilityPenaltyRepo
}

func newCapabilityHarness(t *testing.T, u *capabilityUpstream, passthrough bool) *capabilityHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: time.Second, FallbackMaxWaiting: 3, FallbackWaitTimeout: time.Second}
	mapping := map[string]any{"gpt-6.1-terra": "gpt-6.1-terra"}
	// 已知支持 /v1/responses：否则 chat/completions 遇到 404 会先在同一个号上退回 raw chat completions。
	extra := map[string]any{"openai_responses_supported": true}
	if passthrough {
		extra["openai_passthrough"] = true
	}
	repo := &capabilityPenaltyRepo{openAIImagesFailoverAccountRepo: openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: 41, Name: "codebot", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": mapping}, Extra: extra},
		{ID: 42, Name: "p2", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 10, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": mapping}, Extra: extra},
	}}}
	cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true, true, true}}}
	concurrency := service.NewConcurrencyService(cache)
	rateLimit := service.NewRateLimitService(repo, nil, cfg, nil, nil)
	h := newOpenAIResponsesFailoverTestHandler(t, u, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(repo, &lateFailureUsage{}, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), rateLimit, nil, u, service.NewDeferredService(repo, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
	return &capabilityHarness{h: h, repo: repo}
}

func (hs *capabilityHarness) run(t *testing.T, route string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	groupID := int64(2)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	payload := fmt.Sprintf(`{"model":"gpt-6.1-terra","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
	if route == "responses" {
		payload = fmt.Sprintf(`{"model":"gpt-6.1-terra","stream":%t,"input":"hi"}`, stream)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+route, bytes.NewBufferString(payload)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 921, UserID: 922, GroupID: &groupID, Group: group, User: &service.User{ID: 922}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 922, Concurrency: 1})
	if route == "responses" {
		hs.h.Responses(c)
	} else {
		hs.h.ChatCompletions(c)
	}
	return w
}

func TestOpenAICapabilityFailover_SwitchesToNextAccountWithoutPenalty(t *testing.T) {
	type tc struct {
		failure     capabilityFailure
		route       string
		stream      bool
		passthrough bool
	}
	var cases []tc
	for _, failure := range []capabilityFailure{capabilityModel404, capabilityToolChoiceHTTP, capabilityToolChoiceStream, capabilityEdgeBlock403} {
		cases = append(cases,
			tc{failure: failure, route: "responses", stream: true},
			tc{failure: failure, route: "responses", stream: false},
			tc{failure: failure, route: "chat/completions", stream: true},
			tc{failure: failure, route: "responses", stream: true, passthrough: true},
			tc{failure: failure, route: "responses", stream: false, passthrough: true},
		)
	}
	for _, tt := range cases {
		t.Run(fmt.Sprintf("%s_%s_stream_%t_passthrough_%t", tt.failure, tt.route, tt.stream, tt.passthrough), func(t *testing.T) {
			u := &capabilityUpstream{failing: map[int64]bool{41: true}, failure: tt.failure}
			hs := newCapabilityHarness(t, u, tt.passthrough)

			w := hs.run(t, tt.route, tt.stream)
			require.Equal(t, []int64{41, 42}, u.ids(), "the failing account is tried once, then the next account")
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), "healthy account answered")
			require.Empty(t, hs.repo.recorded(), "no cooldown / temp-unschedulable / error marking for the failing account")

			// 没有冷却：下一次请求仍然先派给同一个号。
			hs.run(t, tt.route, tt.stream)
			hs.run(t, tt.route, tt.stream)
			require.Equal(t, []int64{41, 42, 41, 42, 41, 42}, u.ids(), "the failing account stays first in line")
			require.Empty(t, hs.repo.recorded())
		})
	}
}

func TestOpenAICapabilityFailover_ExhaustedResponse(t *testing.T) {
	for _, tt := range []struct {
		failure     capabilityFailure
		passthrough bool
		status      int
		errType     string
		code        string
		message     string
	}{
		{failure: capabilityModel404, status: http.StatusNotFound, errType: "invalid_request_error", code: "model_not_found", message: capabilityModelUnsupportedMessage},
		{failure: capabilityModel404, passthrough: true, status: http.StatusNotFound, errType: "invalid_request_error", code: "model_not_found", message: capabilityModelUnsupportedMessage},
		{failure: capabilityToolChoiceHTTP, status: http.StatusBadGateway, errType: "upstream_error", message: capabilityToolChoiceMessage},
		{failure: capabilityToolChoiceStream, status: http.StatusBadGateway, errType: "upstream_error", message: capabilityToolChoiceMessage},
		{failure: capabilityToolChoiceHTTP, passthrough: true, status: http.StatusBadGateway, errType: "upstream_error", message: capabilityToolChoiceMessage},
		{failure: capabilityEdgeBlock403, status: http.StatusBadGateway, errType: "upstream_error", message: "Upstream access forbidden, please contact administrator"},
		{failure: capabilityEdgeBlock403, passthrough: true, status: http.StatusBadGateway, errType: "upstream_error", message: "Upstream access forbidden, please contact administrator"},
	} {
		for _, route := range []string{"responses", "chat/completions"} {
			t.Run(fmt.Sprintf("%s_%s_passthrough_%t", tt.failure, route, tt.passthrough), func(t *testing.T) {
				u := &capabilityUpstream{failing: map[int64]bool{41: true, 42: true}, failure: tt.failure}
				hs := newCapabilityHarness(t, u, tt.passthrough)

				w := hs.run(t, route, false)
				require.Equal(t, []int64{41, 42}, u.ids(), "each account is tried once; no same-account retry")
				require.Equal(t, tt.status, w.Code, w.Body.String())
				body := w.Body.String()
				require.Equal(t, tt.errType, gjson.Get(body, "error.type").String())
				require.Equal(t, tt.code, gjson.Get(body, "error.code").String())
				require.Equal(t, tt.message, gjson.Get(body, "error.message").String())
				require.Empty(t, hs.repo.recorded())
			})
		}
	}
}
