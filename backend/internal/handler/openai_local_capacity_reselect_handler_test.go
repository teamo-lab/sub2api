//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
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
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type localCapacityHandlerBinding struct {
	key             string
	accountID       int64
	slotHeld        bool
	contextCanceled bool
}

type localCapacityHandlerSticky struct {
	service.GatewayCache
	mu        sync.Mutex
	accountID int64
	values    map[string]int64
	sets      []localCapacityHandlerBinding
	deletes   int
	hasSlot   func(int64) bool
}

func (s *localCapacityHandlerSticky) GetSessionAccountID(_ context.Context, _ int64, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	if strings.HasPrefix(key, "openai:") && strings.Count(key, ":") == 1 {
		return s.accountID, nil
	}
	return 0, service.ErrStickySessionNotFound
}
func (s *localCapacityHandlerSticky) SetSessionAccountID(ctx context.Context, _ int64, key string, accountID int64, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, localCapacityHandlerBinding{key: key, accountID: accountID, slotHeld: s.hasSlot(accountID), contextCanceled: ctx.Err() != nil})
	s.values[key] = accountID
	if strings.HasPrefix(key, "openai:") && strings.Count(key, ":") == 1 {
		s.accountID = accountID
	}
	return nil
}
func (*localCapacityHandlerSticky) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}
func (s *localCapacityHandlerSticky) DeleteSessionAccountID(_ context.Context, _ int64, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.values, key)
	if strings.HasPrefix(key, "openai:") && strings.Count(key, ":") == 1 {
		s.accountID = 0
	}
	return nil
}

type localCapacityHandlerCache struct {
	helperConcurrencyCacheStub
	queueAllowed     bool
	backupFull       bool
	primaryFree      bool
	accountIDs       []int64
	queueIDs         []int64
	leftQueueIDs     []int64
	onQueue          func()
	onAccountAcquire func(int64)
	heldSlots        map[int64]int
}

func (s *localCapacityHandlerCache) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountIDs = append(s.accountIDs, id)
	if s.onAccountAcquire != nil {
		s.onAccountAcquire(id)
	}
	acquired := (id == 23 && s.primaryFree) || (id == 32 && !s.backupFull)
	if acquired {
		s.heldSlots[id]++
	}
	return acquired, nil
}
func (s *localCapacityHandlerCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountReleaseCalls++
	s.heldSlots[id]--
	return nil
}
func (s *localCapacityHandlerCache) GetAccountConcurrency(_ context.Context, id int64) (int, error) {
	if id == 23 {
		return 100, nil
	}
	return 0, nil
}
func (s *localCapacityHandlerCache) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}
func (s *localCapacityHandlerCache) IncrementAccountWaitCount(_ context.Context, id int64, _ int) (bool, error) {
	s.queueIDs = append(s.queueIDs, id)
	if s.onQueue != nil {
		s.onQueue()
	}
	return s.queueAllowed, nil
}
func (s *localCapacityHandlerCache) DecrementAccountWaitCount(_ context.Context, id int64) error {
	s.leftQueueIDs = append(s.leftQueueIDs, id)
	return nil
}

type localCapacityHandlerUsage struct {
	service.UsageLogRepository
	rows []*service.UsageLog
}

func (s *localCapacityHandlerUsage) Create(_ context.Context, row *service.UsageLog) (bool, error) {
	s.rows = append(s.rows, row)
	return true, nil
}

type localCapacityHandlerUpstream struct {
	service.HTTPUpstream
	accountIDs       []int64
	failBeforeOutput bool
	failAfterOutput  bool
}

func (u *localCapacityHandlerUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, id)
	var payload map[string]any
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		return nil, err
	}
	stream, _ := payload["stream"].(bool)
	chat := strings.Contains(req.URL.Path, "chat/completions")
	if u.failBeforeOutput {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"upstream_error","code":"upstream_error","message":"synthetic provider B failure"}}`))}, nil
	}
	if u.failAfterOutput {
		partial := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial content\"}\n\n"
		if chat {
			partial = "data: {\"id\":\"chatcmpl_partial\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-5.6-terra\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic partial content\"},\"finish_reason\":null}]}\n\n"
		}
		failure := "data: {\"type\":\"error\",\"error\":{\"code\":\"upstream_error\",\"type\":\"upstream_error\",\"message\":\"synthetic provider failure after output\"}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(partial + failure))}, nil
	}
	contentType := "application/json"
	body := `{"id":"resp_capacity_backup","created_at":1700000000,"object":"response","status":"completed","model":"gpt-5.6-terra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"P10 ready"}]}],"usage":{"input_tokens":5,"output_tokens":1}}`
	if chat {
		body = `{"id":"chatcmpl_capacity_backup","object":"chat.completion","model":"gpt-5.6-terra","choices":[{"index":0,"message":{"role":"assistant","content":"P10 ready"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	}
	if stream {
		contentType = "text/event-stream"
		if chat {
			body = "data: {\"id\":\"chatcmpl_capacity_backup\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6-terra\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"P10 ready\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_capacity_backup\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\ndata: [DONE]\n\n"
		} else {
			body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_capacity_backup\",\"created_at\":1700000000,\"model\":\"gpt-5.6-terra\",\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"P10 ready\"}\n\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
}

type localCapacityHandlerFixture struct {
	h              *OpenAIGatewayHandler
	cache          *localCapacityHandlerCache
	upstream       *localCapacityHandlerUpstream
	usage          *localCapacityHandlerUsage
	keyRateLimit5h float64
	ops            *service.OpsService
	sticky         *localCapacityHandlerSticky
	gatewayConfig  *config.Config
}

func newLocalCapacityHandlerFixture(t *testing.T, queueAllowed, backupFull bool, extra map[string]any) localCapacityHandlerFixture {
	t.Helper()
	cache := &localCapacityHandlerCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true}}, queueAllowed: queueAllowed, backupFull: backupFull, heldSlots: make(map[int64]int)}
	sticky := &localCapacityHandlerSticky{accountID: 23, values: make(map[string]int64), hasSlot: func(id int64) bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.heldSlots[id] > 0
	}}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling = config.GatewaySchedulingConfig{LoadBatchEnabled: true, StickySessionMaxWaiting: 3,
		StickySessionWaitTimeout: 20 * time.Millisecond, FallbackMaxWaiting: 3, FallbackWaitTimeout: 20 * time.Millisecond}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: 23, Name: "synthetic-P1-full", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive,
			Schedulable: true, Concurrency: 100, Priority: 1, Extra: extra,
			Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid", "pool_mode": true, "pool_mode_retry_count": 0, "model_mapping": map[string]any{"gpt-5.6-terra": "gpt-5.6-terra"}}},
		{ID: 32, Name: "synthetic-P10-free", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive,
			Schedulable: true, Concurrency: 10, Priority: 10,
			Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid", "pool_mode": true, "pool_mode_retry_count": 0, "model_mapping": map[string]any{"gpt-5.6-terra": "gpt-5.6-terra"}}},
	}}
	upstream := &localCapacityHandlerUpstream{}
	usage := &localCapacityHandlerUsage{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, sticky, cfg,
		nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, upstream,
		service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatClaude, time.Millisecond)
	return localCapacityHandlerFixture{h: h, cache: cache, upstream: upstream, usage: usage, sticky: sticky, gatewayConfig: cfg}
}

func localCapacityEnabledExtra() map[string]any {
	return map[string]any{"openai_local_capacity_reselect_enabled": true, "openai_local_capacity_reselect_group_ids": []any{float64(2)}}
}

func (f localCapacityHandlerFixture) request(t *testing.T, route string, stream bool, groupID int64, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	payload := fmt.Sprintf(`{"model":"gpt-5.6-terra","stream":%t,"prompt_cache_key":"synthetic-sticky","input":"synthetic"}`, stream)
	if route == "chat/completions" {
		payload = fmt.Sprintf(`{"model":"gpt-5.6-terra","stream":%t,"prompt_cache_key":"synthetic-sticky","messages":[{"role":"user","content":"synthetic"}]}`, stream)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+route, bytes.NewBufferString(payload))
	if ctx != nil {
		c.Request = c.Request.WithContext(ctx)
	}
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 901, UserID: 902, GroupID: &groupID,
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}, User: &service.User{ID: 902}, RateLimit5h: f.keyRateLimit5h})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 902, Concurrency: 1})
	invoke := func(c *gin.Context) {
		if route == "responses" {
			f.h.Responses(c)
		} else {
			f.h.ChatCompletions(c)
		}
	}
	if f.ops == nil {
		invoke(c)
	} else {
		prepared := c
		router := gin.New()
		router.Use(OpsErrorLoggerMiddleware(f.ops))
		router.POST("/v1/"+route, func(actual *gin.Context) {
			for key, value := range prepared.Keys {
				actual.Set(key, value)
			}
			c = actual
			invoke(actual)
		})
		router.ServeHTTP(recorder, prepared.Request)
	}
	return c, recorder
}

func TestOpenAILocalCapacityReselectHandler_EnabledUsesFreeBackup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, wait := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream_%t/wait_%t", route, stream, wait), func(t *testing.T) {
					f := newLocalCapacityHandlerFixture(t, wait, false, localCapacityEnabledExtra())
					_, recorder := f.request(t, route, stream, 2, nil)
					require.Equal(t, []int64{32}, f.upstream.accountIDs, "admitted request must reach exactly one healthy fallback")
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Contains(t, recorder.Body.String(), "P10 ready")
					require.NotContains(t, recorder.Body.String(), gatewayQueueFullCode)
					require.NotContains(t, recorder.Body.String(), gatewayConcurrencyLimitCode)
					require.Equal(t, 1, f.cache.userAcquireCalls)
					require.Equal(t, 1, f.cache.userReleaseCalls)
					require.Equal(t, []int64{23}, f.cache.queueIDs, "only the original sticky account may enter waiting")
					if wait {
						require.Equal(t, []int64{23}, f.cache.leftQueueIDs)
					} else {
						require.Empty(t, f.cache.leftQueueIDs)
					}
					require.Len(t, f.usage.rows, 1, "successful fallback submits exactly one usage record; no financial DB is involved")
					require.Equal(t, int64(32), f.usage.rows[0].AccountID)
				})
			}
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_DisabledOrOutsideTrustedGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"default_off", "explicit_false", "group_mismatch", "missing_group_allowlist"} {
				t.Run(fmt.Sprintf("%s/stream_%t/%s", route, stream, mode), func(t *testing.T) {
					extra, groupID := localCapacityEnabledExtra(), int64(2)
					switch mode {
					case "default_off":
						extra = nil
					case "explicit_false":
						extra["openai_local_capacity_reselect_enabled"] = false
					case "group_mismatch":
						groupID = 3
					case "missing_group_allowlist":
						delete(extra, "openai_local_capacity_reselect_group_ids")
					}
					f := newLocalCapacityHandlerFixture(t, false, false, extra)
					_, recorder := f.request(t, route, stream, groupID, nil)
					require.Equal(t, http.StatusTooManyRequests, recorder.Code)
					require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
					require.Empty(t, f.upstream.accountIDs)
					require.NotContains(t, f.cache.accountIDs, int64(32))
					require.Equal(t, []int64{23}, f.cache.queueIDs)
					require.Equal(t, 1, f.cache.userAcquireCalls)
					require.Equal(t, 1, f.cache.userReleaseCalls)
					require.Empty(t, f.usage.rows)
				})
			}
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_BackupFullPreservesFirstFailureWithoutWaitingAgain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, wait := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream_%t/wait_%t", route, stream, wait), func(t *testing.T) {
					f := newLocalCapacityHandlerFixture(t, wait, true, localCapacityEnabledExtra())
					_, recorder := f.request(t, route, stream, 2, nil)
					require.Contains(t, f.cache.accountIDs, int64(32), "the bounded alternative must actually be checked")
					require.Equal(t, []int64{23}, f.cache.queueIDs, "no fallback account may start a second wait")
					require.Empty(t, f.upstream.accountIDs)
					require.Empty(t, f.usage.rows)
					require.Equal(t, 1, f.cache.userAcquireCalls)
					require.Equal(t, 1, f.cache.userReleaseCalls)
					if wait {
						require.Contains(t, recorder.Body.String(), gatewayConcurrencyLimitCode)
						require.Equal(t, []int64{23}, f.cache.leftQueueIDs)
					} else {
						require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
						require.Empty(t, f.cache.leftQueueIDs)
					}
					if !stream || !wait {
						require.Equal(t, http.StatusTooManyRequests, recorder.Code)
					}
				})
			}
		}
	}
}

type localCapacityHandlerBillingCache struct{ service.BillingCache }

func (localCapacityHandlerBillingCache) GetUserBalance(context.Context, int64) (float64, error) {
	return 10, nil
}
func (localCapacityHandlerBillingCache) GetAPIKeyRateLimit(context.Context, int64) (*service.APIKeyRateLimitCacheData, error) {
	now := time.Now().Unix()
	return &service.APIKeyRateLimitCacheData{Usage5h: 2, Window5h: now, Window1d: now, Window7d: now}, nil
}

func TestOpenAILocalCapacityReselectHandler_ClientQuotaIsNotBypassed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, limitation := range []string{"user_concurrency", "api_key_5h_quota"} {
			t.Run(route+"/"+limitation, func(t *testing.T) {
				f := newLocalCapacityHandlerFixture(t, false, false, localCapacityEnabledExtra())
				if limitation == "user_concurrency" {
					f.cache.userSeq = []bool{false}
				} else {
					f.keyRateLimit5h = 1
					billing := service.NewBillingCacheService(localCapacityHandlerBillingCache{}, nil, nil, nil, nil, nil, &config.Config{}, nil)
					t.Cleanup(billing.Stop)
					f.h.billingCacheService = billing
				}
				_, recorder := f.request(t, route, false, 2, nil)
				require.Equal(t, http.StatusTooManyRequests, recorder.Code)
				require.Empty(t, f.cache.accountIDs)
				require.Empty(t, f.cache.queueIDs)
				require.Empty(t, f.upstream.accountIDs)
				require.Empty(t, f.usage.rows)
				if limitation == "api_key_5h_quota" {
					require.Contains(t, recorder.Body.String(), "5小时限额已用完")
				}
			})
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_CanceledOrExpiredRequestCannotReselect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, ended := range []string{"cancel_during_wait", "deadline"} {
				t.Run(fmt.Sprintf("%s/stream_%t/%s", route, stream, ended), func(t *testing.T) {
					f := newLocalCapacityHandlerFixture(t, true, false, localCapacityEnabledExtra())
					var ctx context.Context
					var cancel context.CancelFunc
					if ended == "deadline" {
						ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
					} else {
						ctx, cancel = context.WithCancel(context.Background())
						f.cache.onQueue = cancel
					}
					defer cancel()
					f.request(t, route, stream, 2, ctx)
					require.Error(t, ctx.Err())
					require.NotContains(t, f.cache.accountIDs, int64(32))
					require.Empty(t, f.upstream.accountIDs)
					require.Empty(t, f.usage.rows)
					require.LessOrEqual(t, len(f.cache.queueIDs), 1)
				})
			}
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_ImmediatePrimarySuccessIsByteIdentical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", route, stream), func(t *testing.T) {
				fixtures := make([]localCapacityHandlerFixture, 0, 2)
				for _, enabled := range []bool{false, true} {
					extra := localCapacityEnabledExtra()
					extra["openai_local_capacity_reselect_enabled"] = enabled
					f := newLocalCapacityHandlerFixture(t, false, false, extra)
					f.cache.primaryFree = true
					fixtures = append(fixtures, f)
				}
				// Freeze the bridge's generated created timestamp while comparing
				// the real response bytes; do not normalize either output body.
				// Construct background cache janitors outside the clock bubble.
				synctest.Test(t, func(t *testing.T) {
					var baseline []byte
					for index, f := range fixtures {
						_, recorder := f.request(t, route, stream, 2, nil)
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Equal(t, []int64{23}, f.upstream.accountIDs)
						require.Empty(t, f.cache.queueIDs)
						require.NotContains(t, f.cache.accountIDs, int64(32))
						require.Equal(t, 1, f.cache.userAcquireCalls)
						require.Equal(t, 1, f.cache.userReleaseCalls)
						require.Len(t, f.usage.rows, 1)
						require.Equal(t, int64(23), f.usage.rows[0].AccountID)
						if index == 1 {
							require.Equal(t, baseline, recorder.Body.Bytes(), "flag must not alter an immediately admitted request's response bytes")
						} else {
							baseline = append([]byte(nil), recorder.Body.Bytes()...)
						}
					}
				})
			})
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_BackupProviderFailureOwnsFinalOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", route, stream), func(t *testing.T) {
				setupOpsErrorLogTestQueue(t, 4)
				t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
				repo := &ingressRejectOpsRepo{}
				f := newLocalCapacityHandlerFixture(t, false, false, localCapacityEnabledExtra())
				f.ops = service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				f.upstream.failBeforeOutput = true
				f.h.maxAccountSwitches = 1 // A's local admission already consumes the single switch.
				_, recorder := f.request(t, route, stream, 2, nil)
				require.Equal(t, []int64{32}, f.upstream.accountIDs)
				require.Equal(t, http.StatusBadGateway, recorder.Code)
				require.NotContains(t, recorder.Body.String(), gatewayQueueFullCode)
				require.NotContains(t, recorder.Body.String(), gatewayConcurrencyLimitCode)
				require.Equal(t, []int64{23}, f.cache.queueIDs)
				require.Equal(t, 1, f.cache.userAcquireCalls)
				require.Equal(t, 1, f.cache.userReleaseCalls)
				require.Empty(t, f.usage.rows)
				require.Equal(t, 1, len(opsErrorLogQueue))
				flushOpsErrorLogBatch([]opsErrorLogJob{<-opsErrorLogQueue})
				require.Len(t, repo.entries, 1)
				entry := repo.entries[0]
				require.NotNil(t, entry.AccountID)
				require.Equal(t, int64(32), *entry.AccountID)
				require.Equal(t, http.StatusBadGateway, entry.StatusCode)
				require.False(t, entry.IsBusinessLimited)
				require.Equal(t, "provider", entry.ErrorOwner)
				require.Equal(t, "upstream", entry.ErrorPhase)
				require.NotNil(t, entry.UpstreamErrorMessage)
				require.Contains(t, *entry.UpstreamErrorMessage, "synthetic provider B failure")
			})
		}
	}
}

func TestOpenAILocalCapacityReselectHandler_PrimaryOutputPreventsCapacityReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		t.Run(route, func(t *testing.T) {
			f := newLocalCapacityHandlerFixture(t, false, false, localCapacityEnabledExtra())
			f.cache.primaryFree = true
			f.upstream.failAfterOutput = true
			_, recorder := f.request(t, route, true, 2, nil)
			require.Equal(t, http.StatusOK, recorder.Code, "partial SSE already committed headers")
			require.Contains(t, recorder.Body.String(), "synthetic partial content")
			require.Contains(t, recorder.Body.String(), "synthetic provider failure after output")
			require.Equal(t, []int64{23}, f.upstream.accountIDs)
			require.NotContains(t, f.cache.accountIDs, int64(32))
			require.Empty(t, f.cache.queueIDs)
			require.Equal(t, 1, f.cache.userAcquireCalls)
			require.Equal(t, 1, f.cache.userReleaseCalls)
		})
	}
}

func TestOpenAILocalCapacityReselectHandler_CancellationAsBackupSlotIsAcquired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		t.Run(route, func(t *testing.T) {
			f := newLocalCapacityHandlerFixture(t, false, false, localCapacityEnabledExtra())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.cache.onAccountAcquire = func(id int64) {
				if id == 32 {
					cancel()
				}
			}
			f.request(t, route, false, 2, ctx)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Contains(t, f.cache.accountIDs, int64(32))
			require.Empty(t, f.upstream.accountIDs, "cancellation during backup slot acquisition must prevent Forward")
			require.Equal(t, 1, f.cache.accountReleaseCalls, "the newly acquired backup slot must be released exactly once")
			require.Equal(t, 1, f.cache.userReleaseCalls)
			require.Empty(t, f.usage.rows)
		})
	}
}

func TestOpenAILocalCapacityReselectHandler_ProbeBindingRequiresLiveAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"responses", "chat/completions"} {
		for _, loadBatch := range []bool{false, true} {
			for _, outcome := range []string{"backup_full", "backup_canceled", "backup_success"} {
				t.Run(fmt.Sprintf("%s/load_batch_%t/%s", route, loadBatch, outcome), func(t *testing.T) {
					f := newLocalCapacityHandlerFixture(t, false, outcome == "backup_full", localCapacityEnabledExtra())
					f.gatewayConfig.Gateway.Scheduling.LoadBatchEnabled = loadBatch
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if outcome == "backup_canceled" {
						f.cache.onAccountAcquire = func(id int64) {
							if id == 32 {
								cancel()
							}
						}
					}
					_, recorder := f.request(t, route, false, 2, ctx)
					require.Contains(t, f.cache.accountIDs, int64(32), "control must actually probe the alternative")
					require.Equal(t, []int64{23}, f.cache.queueIDs)
					backupBindings := 0
					for _, binding := range f.sticky.sets {
						if binding.accountID == 32 && strings.HasPrefix(binding.key, "openai:") && strings.Count(binding.key, ":") == 1 {
							backupBindings++
							require.True(t, binding.slotHeld, "binding requires an actually acquired backup slot")
							require.False(t, binding.contextCanceled, "canceled requests cannot bind their probes")
						}
					}
					switch outcome {
					case "backup_success":
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Equal(t, []int64{32}, f.upstream.accountIDs)
						require.Greater(t, backupBindings, 0, "successful admission may commit the existing binding policy")
						require.Equal(t, int64(32), f.sticky.accountID)
					case "backup_full":
						require.Equal(t, http.StatusTooManyRequests, recorder.Code)
						require.Contains(t, recorder.Body.String(), gatewayQueueFullCode)
						require.Zero(t, backupBindings, "failed probe must not migrate the sticky binding")
						require.Empty(t, f.upstream.accountIDs)
					default:
						require.Zero(t, backupBindings)
						require.Empty(t, f.upstream.accountIDs)
						require.Equal(t, 1, f.cache.accountReleaseCalls)
					}
					require.Zero(t, f.sticky.deletes, "capacity probing alone must not delete the existing binding")
					require.Equal(t, 1, f.cache.userAcquireCalls)
					require.Equal(t, 1, f.cache.userReleaseCalls)
				})
			}
		}
	}
}
