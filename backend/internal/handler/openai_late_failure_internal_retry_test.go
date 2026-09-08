//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type internalRetryCounterexampleUpstream struct {
	service.HTTPUpstream
	route           string
	ids             []int64
	paths           []string
	maxOutputFields []bool
}

func (u *internalRetryCounterexampleUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.ids = append(u.ids, id)
	u.paths = append(u.paths, req.URL.Path)
	body, _ := io.ReadAll(req.Body)
	u.maxOutputFields = append(u.maxOutputFields, gjson.GetBytes(body, "max_output_tokens").Exists())
	n := len(u.ids)
	if id == 22 {
		if n > 2 {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		time.Sleep(35 * time.Second)
		status := http.StatusBadGateway
		payload := `{"error":{"type":"upstream_error","code":"upstream_error","message":"synthetic second short upstream failure"}}`
		if u.route == "chat/completions" {
			status = http.StatusServiceUnavailable
		}
		if n == 1 {
			status = http.StatusBadRequest
			payload = `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`
			if u.route == "chat/completions" {
				status = 404
				payload = `{"error":{"type":"invalid_request_error","message":"Responses endpoint unsupported"}}`
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	}
	time.Sleep(2 * time.Second)
	payload := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic B success\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_B\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
}

func TestOpenAILateFailureSwitchHandler_InternalRetriesDoNotQualify(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, enabled := range []bool{false, true} {
			name := route + "/off"
			if enabled {
				name = route + "/on"
			}
			t.Run(name, func(t *testing.T) {
				u := &internalRetryCounterexampleUpstream{route: route}
				cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true}}}
				concurrency := service.NewConcurrencyService(cache)
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3, StickySessionWaitTimeout: time.Second, FallbackMaxWaiting: 3, FallbackWaitTimeout: time.Second}
				accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
					{ID: 22, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1,
						Extra:       map[string]any{"openai_late_failure_switch_enabled": enabled, "openai_late_failure_switch_group_ids": []int64{3}},
						Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid", "model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}}},
					{ID: 23, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 10,
						Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid", "model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}}},
				}}
				usage := &lateFailureUsage{}
				h := newOpenAIResponsesFailoverTestHandler(t, u, service.AccountTypeAPIKey)
				h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, u,
					service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
				h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
				h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: lateFailureRule(1)}, nil)
				synctest.Test(t, func(t *testing.T) {
					payload := `{"model":"gpt-5.6-sol","stream":false,"max_output_tokens":100,"input":"synthetic"}`
					if route == "chat/completions" {
						payload = `{"model":"gpt-5.6-sol","stream":false,"messages":[{"role":"user","content":"synthetic"}]}`
					}
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/"+route, bytes.NewBufferString(payload))
					c.Request.Header.Set("Content-Type", "application/json")
					groupID := int64(3)
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 901, UserID: 902, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}, User: &service.User{ID: 902}})
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 902, Concurrency: 1})
					started := time.Now()
					if route == "responses" {
						h.Responses(c)
					} else {
						h.ChatCompletions(c)
					}
					elapsed := time.Since(started)
					require.Equal(t, []int64{22, 22}, u.ids[:2])
					if route == "responses" {
						require.True(t, u.maxOutputFields[0])
						require.False(t, u.maxOutputFields[1])
					} else {
						require.Equal(t, "/v1/responses", u.paths[0])
						require.Equal(t, "/v1/chat/completions", u.paths[1])
					}
					require.Equal(t, []int64{22, 22, 22}, u.ids, "two 35-second dispatches must not qualify as one >=60-second attempt")
					require.Equal(t, 100*time.Second, elapsed)
					require.Equal(t, 3, service.OpenAIUpstreamAttemptFactsFromContext(c.Request.Context()).Count)
					t.Logf("route=%s enabled=%t wire_attempts=%v paths=%v A_wire_waits=[35s,35s] aggregate=70s elapsed=%s status=%d", route, enabled, u.ids, u.paths, elapsed, w.Code)
				})
			})
		}
	}
}
