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

const contextWindowFixtureMessage = "Your input exceeds the context window of this model. Please adjust your input and try again."

// contextWindowUpstream: accounts listed in small reject the request the way
// the sv-1 codebot account did (HTTP 200 stream with response.failed, or a
// plain HTTP 400 JSON error); every other account answers normally.
type contextWindowUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	calls    []int64
	small    map[int64]bool
	httpJSON bool
}

func (u *contextWindowUpstream) Do(_ *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, id)
	u.mu.Unlock()
	if u.small[id] {
		if u.httpJSON {
			body := `{"error":{"message":"` + contextWindowFixtureMessage + `","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_small\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_small\",\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"" + contextWindowFixtureMessage + "\",\"param\":\"input\",\"type\":\"invalid_request_error\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"large window answered\",\"output_index\":0,\"content_index\":0}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_large\",\"object\":\"response\",\"created_at\":1700000000,\"model\":\"gpt-6.1-sol\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"large window answered\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (u *contextWindowUpstream) ids() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...)
}

func runContextWindowFailover(t *testing.T, u *contextWindowUpstream, route string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true}}}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: time.Second, FallbackMaxWaiting: 3, FallbackWaitTimeout: time.Second}
	mapping := map[string]any{"gpt-6.1-sol": "gpt-6.1-sol"}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: 31, Name: "small window", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": mapping}},
		{ID: 32, Name: "large window", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 10, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid", "model_mapping": mapping}},
	}}
	usage := &lateFailureUsage{}
	h := newOpenAIResponsesFailoverTestHandler(t, u, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, u, service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)

	groupID := int64(2)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	payload := fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"messages":[{"role":"user","content":"long conversation"}]}`, stream)
	if route == "responses" {
		payload = fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"input":"long conversation"}`, stream)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+route, bytes.NewBufferString(payload)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 911, UserID: 912, GroupID: &groupID, Group: group, User: &service.User{ID: 912}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 912, Concurrency: 1})
	if route == "responses" {
		h.Responses(c)
	} else {
		h.ChatCompletions(c)
	}
	require.Equal(t, cache.accountAcquireCalls, cache.accountReleaseCalls, "all acquired slots released exactly once")
	return w
}

func TestOpenAIContextWindowFailover_SwitchesToLargerWindowAccount(t *testing.T) {
	for _, tc := range []struct {
		route    string
		stream   bool
		httpJSON bool
	}{
		{route: "chat/completions", stream: true},
		{route: "chat/completions", stream: false},
		{route: "chat/completions", stream: true, httpJSON: true},
		{route: "responses", stream: true},
		{route: "responses", stream: false, httpJSON: true},
	} {
		t.Run(fmt.Sprintf("%s_stream_%t_http_json_%t", tc.route, tc.stream, tc.httpJSON), func(t *testing.T) {
			u := &contextWindowUpstream{small: map[int64]bool{31: true}, httpJSON: tc.httpJSON}
			w := runContextWindowFailover(t, u, tc.route, tc.stream)

			require.Equal(t, []int64{31, 32}, u.ids(), "the small-window account is tried once, then the next account")
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), "large window answered")
			require.NotContains(t, w.Body.String(), "exceeds the context window")
		})
	}
}

func TestOpenAIContextWindowFailover_ReturnsUpstreamMessageAfterEveryAccountFails(t *testing.T) {
	for _, route := range []string{"chat/completions", "responses"} {
		t.Run(route, func(t *testing.T) {
			u := &contextWindowUpstream{small: map[int64]bool{31: true, 32: true}}
			w := runContextWindowFailover(t, u, route, false)

			require.Equal(t, []int64{31, 32}, u.ids(), "each account is tried once; no same-account retry")
			require.Equal(t, http.StatusBadRequest, w.Code)
			body := w.Body.String()
			require.Equal(t, "invalid_request_error", gjson.Get(body, "error.type").String())
			require.Equal(t, "context_length_exceeded", gjson.Get(body, "error.code").String())
			require.Equal(t, contextWindowFixtureMessage, gjson.Get(body, "error.message").String())
		})
	}
}
