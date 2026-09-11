//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type opsCapacityFallbackCache struct {
	helperConcurrencyCacheStub
	accountWaitAllowed bool
	accountWaitEntered int
	accountWaitLeft    int
}

func (s *opsCapacityFallbackCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	s.accountWaitEntered++
	return s.accountWaitAllowed, nil
}

func (s *opsCapacityFallbackCache) DecrementAccountWaitCount(context.Context, int64) error {
	s.accountWaitLeft++
	return nil
}

type opsCapacityFallbackUpstream struct {
	recoveryUpstream
	firstStatus int
}

type opsCapacityProviderHandoffRules struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r opsCapacityProviderHandoffRules) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

type opsCapacityProviderHandoffUpstream struct {
	recoveryUpstream
}

func (u *opsCapacityProviderHandoffUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	if id != 2 {
		return u.recoveryUpstream.Do(req, proxy, id, concurrency)
	}
	u.calls = append(u.calls, id)
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial B answer\"}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"upstream_error\",\"type\":\"upstream_error\",\"message\":\"provider B failed after visible output\"}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(bytes.NewBufferString(body)),
	}, nil
}

func (u *opsCapacityFallbackUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	response, err := u.recoveryUpstream.Do(req, proxy, id, concurrency)
	if err == nil && id == 1 {
		response.StatusCode = u.firstStatus
	}
	return response, err
}

func opsCapacityFallbackRouter(ops *service.OpsService, handler gin.HandlerFunc) *gin.Engine {
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/responses", func(c *gin.Context) {
		groupID := int64(3131)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID: 99, GroupID: &groupID,
			Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
			User:  &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
		handler(c)
	})
	return router
}

func opsCapacityFallbackRequest(stream bool) *http.Request {
	body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, stream)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), ctxkey.RequestID, "capacity-origin-request")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "capacity-origin-client-request")
	return req.WithContext(ctx)
}

func opsCapacityFallbackPersistOne(t *testing.T, repo *ingressRejectOpsRepo) *service.OpsInsertErrorLogInput {
	t.Helper()
	require.Equal(t, 1, len(opsErrorLogQueue), "the request must produce exactly one final/recovered row")
	job := <-opsErrorLogQueue
	flushOpsErrorLogBatch([]opsErrorLogJob{job})
	require.Len(t, repo.entries, 1)
	return repo.entries[0]
}

// Keep actual upstream forwarding and actual B admission in one Gin request.
// Only the external HTTP transport and Redis capacity replies are doubles. A's
// error history must survive, but it must not become B's final error origin.
func TestOpsCapacityOriginFallback_UpstreamFailureThenLocalAccountAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, firstStatus := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		for _, stream := range []bool{false, true} {
			for _, waitAllowed := range []bool{false, true} {
				name := fmt.Sprintf("upstream_%d/stream_%t/wait_allowed_%t", firstStatus, stream, waitAllowed)
				t.Run(name, func(t *testing.T) {
					setupOpsErrorLogTestQueue(t, 4)
					t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
					repo := &ingressRejectOpsRepo{}
					ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					upstream := &opsCapacityFallbackUpstream{firstStatus: firstStatus}
					h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
					cache := &opsCapacityFallbackCache{accountWaitAllowed: waitAllowed}
					h.concurrencyHelper = NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, time.Millisecond)

					router := opsCapacityFallbackRouter(ops, func(c *gin.Context) {
						groupID := int64(3131)
						c.Set(opsModelKey, "gpt-5.1")
						c.Set(opsStreamKey, stream)
						selectionA, _, err := h.gatewayService.SelectAccountWithScheduler(c.Request.Context(), &groupID,
							"", "", "gpt-5.1", nil, service.OpenAIUpstreamTransportAny, false)
						require.NoError(t, err)
						require.Equal(t, int64(1), selectionA.Account.ID)
						setOpsSelectedAccount(c, selectionA.Account.ID, service.PlatformOpenAI)
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, stream))
						_, err = h.gatewayService.Forward(c.Request.Context(), c, selectionA.Account, body)
						var failover *service.UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.Equal(t, firstStatus, failover.StatusCode)
						require.False(t, c.Writer.Written(), "A failed before writing client output")

						selectionB, _, err := h.gatewayService.SelectAccountWithScheduler(c.Request.Context(), &groupID,
							"", "", "gpt-5.1", map[int64]struct{}{1: {}}, service.OpenAIUpstreamTransportAny, false)
						require.NoError(t, err)
						require.Equal(t, int64(2), selectionB.Account.ID)
						// Present a bounded wait plan to exercise the real admission path:
						// B's Redis slots became occupied after account selection.
						if selectionB.ReleaseFunc != nil {
							selectionB.ReleaseFunc()
						}
						selectionB.Acquired = false
						selectionB.ReleaseFunc = nil
						selectionB.WaitPlan = &service.AccountWaitPlan{AccountID: 2, MaxConcurrency: 1, MaxWaiting: 1, Timeout: 15 * time.Millisecond}
						setOpsSelectedAccount(c, selectionB.Account.ID, service.PlatformOpenAI)
						streamStarted := false
						release, result := h.acquireOpenAIAccountSlot(c, &groupID, "", selectionB,
							stream, &streamStarted, zap.NewNop(), nil)
						require.Nil(t, release)
						require.Equal(t, openAISlotAcquireFailed, result)
					})

					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, opsCapacityFallbackRequest(stream))
					require.Equal(t, []int64{1}, upstream.calls, "B must not receive an upstream request")
					require.Equal(t, 1, cache.accountWaitEntered)
					if waitAllowed {
						require.Equal(t, 1, cache.accountWaitLeft, "timeout must release B's waiting counter")
						require.Contains(t, recorder.Body.String(), gatewayConcurrencyLimitCode)
					} else {
						require.Zero(t, cache.accountWaitLeft)
						require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
					}
					entry := opsCapacityFallbackPersistOne(t, repo)
					require.NotNil(t, entry.AccountID)
					require.Equal(t, int64(2), *entry.AccountID)
					require.Equal(t, http.StatusTooManyRequests, entry.StatusCode)
					require.False(t, entry.IsBusinessLimited, "B's account capacity failure must count against request SLA")
					require.Equal(t, "routing", entry.ErrorPhase)
					require.Equal(t, "platform", entry.ErrorOwner)
					require.Equal(t, "gateway", entry.ErrorSource)
					require.Nil(t, entry.UpstreamStatusCode, "A's 5xx is history, not B's final status")
					require.Nil(t, entry.UpstreamErrorMessage)
					require.Nil(t, entry.UpstreamErrorDetail)
					require.NotNil(t, entry.UpstreamErrorsJSON)
					events, err := service.ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
					require.NoError(t, err)
					require.NotEmpty(t, events)
					for _, event := range events {
						require.Equal(t, int64(1), event.AccountID)
						require.Equal(t, firstStatus, event.UpstreamStatusCode)
					}
				})
			}
		}
	}
}

func TestOpsCapacityOriginFallback_UpstreamFailureThenHandlerRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, firstStatus := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("upstream_%d/stream_%t", firstStatus, stream), func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 4)
				t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
				repo := &ingressRejectOpsRepo{}
				ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				upstream := &opsCapacityFallbackUpstream{
					recoveryUpstream: recoveryUpstream{succeedOnSecond: true, sse: stream}, firstStatus: firstStatus,
				}
				h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
				h.errorPassthroughService = service.NewErrorPassthroughService(recoveryRuleRepo{rule: &model.ErrorPassthroughRule{
					ID: 91, Enabled: true, Name: "capacity-origin-recovery-control", MatchMode: "all",
					Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{firstStatus},
					PassthroughCode: true, PassthroughBody: true,
					RecoveryPolicy: &model.ErrorRecoveryPolicy{
						Mode: "limited", AccountTypes: []string{service.AccountTypeAPIKey}, Models: []string{"gpt-5.1"},
						UpstreamCodes: []string{"server_is_overloaded"}, AccountSwitches: 1, BudgetSeconds: 10,
					},
				}}, nil)
				router := opsCapacityFallbackRouter(ops, h.Responses)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, opsCapacityFallbackRequest(stream))
				require.Equal(t, []int64{1, 2}, upstream.calls)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), `"id":"resp_recovered"`)
				require.Contains(t, recorder.Body.String(), `"status":"completed"`)
				require.NotContains(t, recorder.Body.String(), "overloaded")
				entry := opsCapacityFallbackPersistOne(t, repo)
				require.Equal(t, http.StatusOK, entry.StatusCode, "successful B recovery cannot become a final SLA failure")
				require.Equal(t, "upstream", entry.ErrorPhase)
				require.Equal(t, "provider", entry.ErrorOwner)
				require.Contains(t, entry.ErrorMessage, "Recovered upstream error")
				require.NotNil(t, entry.UpstreamErrorsJSON)
				events, err := service.ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
				require.NoError(t, err)
				require.NotEmpty(t, events)
				for _, event := range events {
					require.Equal(t, int64(1), event.AccountID)
					require.Equal(t, firstStatus, event.UpstreamStatusCode)
				}
			})
		}
	}
}

func TestOpsCapacityOriginFallback_SkippedHistoricalFailureCannotHideLocalQueueFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
			repo := &ingressRejectOpsRepo{}
			ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			upstream := &opsCapacityFallbackUpstream{firstStatus: http.StatusServiceUnavailable}
			h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
			cache := &opsCapacityFallbackCache{accountWaitAllowed: false}
			h.concurrencyHelper = NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatClaude, time.Millisecond)
			router := opsCapacityFallbackRouter(ops, func(c *gin.Context) {
				groupID := int64(3131)
				setOpsRequestContext(c, "gpt-5.1", stream)
				selectionA, _, err := h.gatewayService.SelectAccountWithScheduler(c.Request.Context(), &groupID,
					"", "", "gpt-5.1", nil, service.OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, int64(1), selectionA.Account.ID)
				setOpsSelectedAccount(c, selectionA.Account.ID, service.PlatformOpenAI)
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, stream))
				_, err = h.gatewayService.Forward(c.Request.Context(), c, selectionA.Account, body)
				var failover *service.UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusServiceUnavailable, failover.StatusCode)
				require.False(t, c.Writer.Written())

				// Simulate A's reviewed passthrough rule on a real failure event.
				// Its exclusion remains auditable history, not B's final decision.
				rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
				require.True(t, ok)
				events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
				require.True(t, ok)
				require.NotEmpty(t, events)
				for _, event := range events {
					require.Equal(t, int64(1), event.AccountID)
					event.SkipMonitoring = true
				}
				c.Set(service.OpsSkipPassthroughKey, true)

				selectionB, _, err := h.gatewayService.SelectAccountWithScheduler(c.Request.Context(), &groupID,
					"", "", "gpt-5.1", map[int64]struct{}{1: {}}, service.OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, int64(2), selectionB.Account.ID)
				if selectionB.ReleaseFunc != nil {
					selectionB.ReleaseFunc()
				}
				selectionB.Acquired = false
				selectionB.ReleaseFunc = nil
				selectionB.WaitPlan = &service.AccountWaitPlan{AccountID: 2, MaxConcurrency: 1, MaxWaiting: 1, Timeout: 15 * time.Millisecond}
				setOpsSelectedAccount(c, selectionB.Account.ID, service.PlatformOpenAI)
				streamStarted := stream
				if stream {
					// A scheduling heartbeat may already have committed HTTP 200;
					// exercise a real in-band Responses failure rather than JSON.
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString(": ping\n\n")
					require.NoError(t, err)
					c.Writer.Flush()
				}
				release, result := h.acquireOpenAIAccountSlot(c, &groupID, "", selectionB,
					stream, &streamStarted, zap.NewNop(), nil)
				require.Nil(t, release)
				require.Equal(t, openAISlotAcquireFailed, result)
				for _, event := range events {
					require.True(t, event.SkipMonitoring, "B's marker must not mutate A's historical rule decision")
				}
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, opsCapacityFallbackRequest(stream))
			require.Equal(t, []int64{1}, upstream.calls, "B's rejected admission must not contact its upstream")
			require.Equal(t, 1, cache.accountWaitEntered)
			require.Zero(t, cache.accountWaitLeft)
			require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
			if stream {
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), "response.failed")
			} else {
				require.Equal(t, http.StatusTooManyRequests, recorder.Code)
			}
			entry := opsCapacityFallbackPersistOne(t, repo)
			require.NotNil(t, entry.AccountID)
			require.Equal(t, int64(2), *entry.AccountID)
			require.Equal(t, http.StatusTooManyRequests, entry.StatusCode)
			require.False(t, entry.IsBusinessLimited)
			require.Equal(t, "routing", entry.ErrorPhase)
			require.Equal(t, "platform", entry.ErrorOwner)
			require.Equal(t, "gateway", entry.ErrorSource)
			require.Nil(t, entry.UpstreamStatusCode)
			require.Nil(t, entry.UpstreamErrorMessage)
			require.NotNil(t, entry.UpstreamErrorsJSON)
			events, err := service.ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
			require.NoError(t, err)
			require.NotEmpty(t, events)
			for _, event := range events {
				require.Equal(t, int64(1), event.AccountID)
				require.Equal(t, http.StatusServiceUnavailable, event.UpstreamStatusCode)
				require.Contains(t, event.Message, "overloaded")
				// SkipMonitoring is intentionally request-local (json:"-"). Its
				// preservation is checked above before queue serialization.
			}
		})
	}
}

func TestOpsCapacityOriginFallback_SkippedProviderACannotHideProviderBAfterOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupOpsErrorLogTestQueue(t, 4)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
	repo := &ingressRejectOpsRepo{}
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	upstream := &opsCapacityProviderHandoffUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
	rules := opsCapacityProviderHandoffRules{rules: []*model.ErrorPassthroughRule{
		{
			ID: 92, Enabled: true, Name: "reviewed-provider-A-503-exclusion", MatchMode: "all",
			Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{http.StatusServiceUnavailable},
			PassthroughCode: true, PassthroughBody: true, SkipMonitoring: true,
		},
		{
			ID: 93, Enabled: true, Name: "bounded-A-to-B-recovery", MatchMode: "all",
			Platforms: []string{service.PlatformOpenAI}, ErrorCodes: []int{http.StatusServiceUnavailable},
			PassthroughCode: true, PassthroughBody: true, SkipMonitoring: false,
			RecoveryPolicy: &model.ErrorRecoveryPolicy{
				Mode: "limited", AccountTypes: []string{service.AccountTypeAPIKey}, Models: []string{"gpt-5.1"},
				UpstreamCodes: []string{"server_is_overloaded"}, AccountSwitches: 1, BudgetSeconds: 10,
			},
		},
	}}
	h.errorPassthroughService = service.NewErrorPassthroughService(rules, nil)
	router := opsCapacityFallbackRouter(ops, func(c *gin.Context) {
		h.Responses(c)
		rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
		require.True(t, ok)
		events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
		require.True(t, ok)
		require.Len(t, events, 2)
		require.Equal(t, int64(1), events[0].AccountID)
		require.True(t, events[0].SkipMonitoring, "A's actual 503 rule must mark only A's history")
		require.Equal(t, int64(2), events[1].AccountID)
		require.False(t, events[1].SkipMonitoring, "B's 502 has no exclusion rule and cannot inherit A's skip")
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, opsCapacityFallbackRequest(true))
	require.Equal(t, []int64{1, 2}, upstream.calls, "B's partial output prohibits further retry or fallback")
	require.Equal(t, http.StatusOK, recorder.Code, "partial SSE output already committed wire status")
	require.Contains(t, recorder.Body.String(), "partial B answer")
	require.Contains(t, recorder.Body.String(), "provider B failed after visible output")
	require.NotContains(t, recorder.Body.String(), "Our servers are currently overloaded")
	entry := opsCapacityFallbackPersistOne(t, repo)
	require.NotNil(t, entry.AccountID)
	require.Equal(t, int64(2), *entry.AccountID)
	require.Equal(t, http.StatusBadGateway, entry.StatusCode)
	require.False(t, entry.IsBusinessLimited)
	require.Equal(t, "upstream", entry.ErrorPhase)
	require.Equal(t, "provider", entry.ErrorOwner)
	require.Equal(t, "upstream_http", entry.ErrorSource)
	require.NotNil(t, entry.UpstreamStatusCode)
	require.Equal(t, http.StatusBadGateway, *entry.UpstreamStatusCode)
	require.NotNil(t, entry.UpstreamErrorMessage)
	require.Contains(t, *entry.UpstreamErrorMessage, "provider B failed after visible output")
	require.NotNil(t, entry.UpstreamErrorsJSON)
	events, err := service.ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, int64(1), events[0].AccountID)
	require.Equal(t, http.StatusServiceUnavailable, events[0].UpstreamStatusCode)
	require.Contains(t, events[0].Message, "overloaded")
	require.Equal(t, int64(2), events[1].AccountID)
	require.Equal(t, http.StatusBadGateway, events[1].UpstreamStatusCode)
	require.Contains(t, events[1].Message, "provider B failed after visible output")
}
