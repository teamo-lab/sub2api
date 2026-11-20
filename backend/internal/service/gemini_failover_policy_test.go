//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClassifyGeminiUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   geminiErrorClass
	}{
		{"402 prepayment", 402, `{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`, geminiErrorPaymentRequired},
		{"404 any", 404, `{"error":{"code":404,"message":"Publisher model was not found"}}`, geminiErrorCapabilityMiss},
		{"400 unknown provider", 400, `{"error":{"message":"unknown provider for model gemini-4-carbon (request id: x)","type":"invalid_request_error","code":"model_not_found"}}`, geminiErrorCapabilityMiss},
		{"400 model turn", 400, `{"error":{"code":400,"message":"Requests ending with a model turn are not supported.","status":"INVALID_ARGUMENT"}}`, geminiErrorCapabilityMiss},
		{"400 thinking budget", 400, `{"error":{"code":400,"message":"Thinking budget is not supported for this model. Use thinking level instead."}}`, geminiErrorCapabilityMiss},
		{"400 propertyNames escaped", 400, `{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"propertyNames\" at 'tools[0]': Cannot find field."}}`, geminiErrorCapabilityMiss},
		{"400 const", 400, `{"error":{"message":"Invalid JSON payload received. Unknown name \"const\" at 'tools[0].function_declarations[1]'"}}`, geminiErrorCapabilityMiss},
		{"400 plain request error", 400, `{"error":{"code":400,"message":"Invalid value at 'generation_config.temperature'","status":"INVALID_ARGUMENT"}}`, geminiErrorDefault},
		{"400 safety", 400, `{"error":{"message":"The prompt was rejected by the upstream safety policy"}}`, geminiErrorDefault},
		{"503 relay no accounts", 503, `{"error":{"message":"No available Gemini accounts: no available accounts","type":"upstream_error"}}`, geminiErrorNoCapacity},
		{"503 new-api no channel", 503, `{"error":{"message":"Failed to get available channel for model gemini-3.8-flash under group gemini-anti (distributor)"}}`, geminiErrorNoCapacity},
		{"429 saturated", 429, `{"error":{"code":"upstream_rate_limit","message":"当前分组上游负载已饱和，请稍后再试"}}`, geminiErrorNoCapacity},
		{"503 plain", 503, `{"error":{"code":503,"message":"The service is temporarily unavailable."}}`, geminiErrorDefault},
		{"429 plain", 429, `{"error":{"code":429,"message":"Resource exhausted"}}`, geminiErrorDefault},
		{"403 billing", 403, `{"error":{"code":403,"message":"This API method requires billing to be enabled."}}`, geminiErrorDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyGeminiUpstreamError(tc.status, []byte(tc.body)))
		})
	}
}

func requireGeminiImmediateFailover(t *testing.T, c *gin.Context, err error, status int, reason GatewayFailureReason) *UpstreamFailoverError {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, status, failoverErr.StatusCode)
	require.Equal(t, reason, failoverErr.Reason)
	require.False(t, failoverErr.RetryableOnSameAccount, "these failures switch accounts immediately")
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, c.Writer.Written(), "nothing may reach the client before failover")

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.Equal(t, "failover", last.Kind)
	require.Equal(t, string(reason), last.Reason)
	return failoverErr
}

func geminiPoolAccountWithRetryCodes(codes ...int) *Account {
	acc := geminiPoolModeAPIKeyAccount()
	list := make([]any, 0, len(codes))
	for _, code := range codes {
		list = append(list, float64(code))
	}
	acc.Credentials["pool_mode_retry_status_codes"] = list
	return acc
}

func geminiNonPoolAPIKeyAccount() *Account {
	return &Account{
		ID:          702,
		Platform:    PlatformGemini,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
	}
}

func TestGeminiForwardNative_PoolMode402FailsOverImmediately(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusPaymentRequired, `{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusPaymentRequired, GeminiPaymentRequiredReason)
	require.Equal(t, 1, stub.calls)
}

func TestGeminiForwardNative_PoolMode404FailsOverAsCapabilityMiss(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusNotFound, `{"error":{"code":404,"message":"models/gemini-4-carbon is not found","status":"NOT_FOUND"}}`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-4-carbon", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	fe := requireGeminiImmediateFailover(t, c, err, http.StatusNotFound, GeminiCapabilityMissReason)
	require.True(t, IsGeminiCapabilityMissExhausted(fe))
}

func TestGeminiForwardNative_NonPool400CapabilityMissFailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusBadRequest, `{"error":{"message":"unknown provider for model gemini-4-carbon","type":"invalid_request_error","code":"model_not_found"}}`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiNonPoolAPIKeyAccount(), "gemini-4-carbon", "generateContent", false, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusBadRequest, GeminiCapabilityMissReason)
}

func TestGeminiForwardNative_Plain400StillPassesThrough(t *testing.T) {
	body := `{"error":{"code":400,"message":"Invalid value at 'generation_config.temperature'","status":"INVALID_ARGUMENT"}}`
	svc, _ := newGeminiSkippedWriteService(http.StatusBadRequest, body)
	c, rec := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, body, rec.Body.String())
}

func TestGeminiForwardNative_PoolModeNoCapacitySkipsSameAccountRetry(t *testing.T) {
	// 普通 503 在池模式配置了 503 时会先在同一个账号上重试；中转「没有可用账号」不应该。
	svc, _ := newGeminiSkippedWriteService(http.StatusServiceUnavailable, `{"error":{"message":"No available Gemini accounts: no available accounts"}}`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolAccountWithRetryCodes(429, 503), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusServiceUnavailable, GeminiUpstreamNoCapacityReason)

	svc2, _ := newGeminiSkippedWriteService(http.StatusServiceUnavailable, `{"error":{"code":503,"message":"The service is temporarily unavailable."}}`)
	c2, _ := newGeminiNativeTestContext(t)
	_, err = svc2.ForwardNative(context.Background(), c2, geminiPoolAccountWithRetryCodes(429, 503), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount, "plain 503 keeps the configured pool-mode same-account retry")
}

func TestGeminiForwardNative_NonPoolNoCapacityDoesNotRetryInAccount(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusServiceUnavailable, `{"error":{"message":"Failed to get available channel for model gemini-3.8-flash under group gemini-anti (distributor)"}}`)
	c, _ := newGeminiNativeTestContext(t)
	start := time.Now()
	_, err := svc.ForwardNative(context.Background(), c, geminiNonPoolAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusServiceUnavailable, GeminiUpstreamNoCapacityReason)
	require.Equal(t, 1, stub.calls)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestGeminiForwardNative_NonPool503RetriesInAccountOnlyOnce(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusServiceUnavailable, `{"error":{"code":503,"message":"The service is temporarily unavailable."}}`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiNonPoolAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 2, stub.calls, "one attempt plus one in-account retry")
}

func TestGeminiForwardNative_ConnectionErrorRetriesOnceThenFailsOver(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusOK, "")
	stub.err = errors.New(`Post "https://hk.example.com/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse": dial tcp 38.207.176.210:443: i/o timeout`)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	fe := requireGeminiImmediateFailover(t, c, err, http.StatusBadGateway, GeminiUpstreamRequestErrorReason)
	require.Equal(t, 2, stub.calls)
	require.Contains(t, string(fe.ResponseBody), "Upstream request failed")
}

func TestGeminiForwardNative_ClientCancelStopsRetrying(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusOK, "")
	stub.err = errors.New("dial tcp: connection refused")
	c, _ := newGeminiNativeTestContext(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := svc.ForwardNative(ctx, c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 600*time.Millisecond, "backoff must stop as soon as the client goes away")
	require.Equal(t, 1, stub.calls)
}

// geminiBlockingUpstream 模拟挂住的上游：headersFirst=false 时 Do 一直等到请求 ctx 结束；
// 否则立即返回 200，响应体先吐出 prefix，然后一直等到请求 ctx 结束。
type geminiBlockingUpstream struct {
	headersFirst bool
	prefix       string
	mu           sync.Mutex
	calls        int
}

func (u *geminiBlockingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	ctx := req.Context()
	if !u.headersFirst {
		<-ctx.Done()
		return nil, fmt.Errorf("Post %q: %w", req.URL.String(), ctx.Err())
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(io.MultiReader(strings.NewReader(u.prefix), &geminiCtxBlockedReader{ctx: ctx})),
	}, nil
}

func (u *geminiBlockingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type geminiCtxBlockedReader struct{ ctx context.Context }

func (r *geminiCtxBlockedReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func newGeminiTimeoutService(upstream HTTPUpstream, firstByteSeconds, idleSeconds int) *GeminiMessagesCompatService {
	cfg := &config.Config{}
	cfg.Gateway.GeminiStreamFirstByteTimeout = firstByteSeconds
	cfg.Gateway.StreamDataIntervalTimeout = idleSeconds
	return &GeminiMessagesCompatService{
		httpUpstream:     upstream,
		cfg:              cfg,
		rateLimitService: NewRateLimitService(&errorPolicyRepoStub{}, nil, &config.Config{}, nil, nil),
	}
}

func TestGeminiForwardNative_FirstByteTimeoutBeforeHeadersFailsOver(t *testing.T) {
	upstream := &geminiBlockingUpstream{}
	svc := newGeminiTimeoutService(upstream, 1, 0)
	c, _ := newGeminiNativeTestContext(t)
	start := time.Now()
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	fe := requireGeminiImmediateFailover(t, c, err, http.StatusGatewayTimeout, GeminiFirstByteTimeoutReason)
	require.Equal(t, 1, upstream.calls, "a timeout switches accounts instead of retrying the same one")
	require.Less(t, time.Since(start), 3*time.Second)
	require.Contains(t, string(fe.ResponseBody), "first byte timeout")
}

func TestGeminiForwardNative_FirstByteTimeoutAfterHeadersFailsOver(t *testing.T) {
	upstream := &geminiBlockingUpstream{headersFirst: true}
	svc := newGeminiTimeoutService(upstream, 1, 0)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusGatewayTimeout, GeminiFirstByteTimeoutReason)
}

func TestGeminiForwardNative_StreamIdleBeforeAnswerFailsOver(t *testing.T) {
	// 先吐一个 thought 帧（还没有回答），然后挂住：空闲超时，换号。
	prefix := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thinking\",\"thought\":true}]}}]}\n\n"
	upstream := &geminiBlockingUpstream{headersFirst: true, prefix: prefix}
	svc := newGeminiTimeoutService(upstream, 0, 1)
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusGatewayTimeout, GeminiStreamIdleTimeoutReason)
}

func TestGeminiForwardNative_StreamIdleAfterAnswerEndsStream(t *testing.T) {
	// 回答已经开始转发后挂住：不能换号了，但请求要在空闲超时后结束，而不是一直挂着。
	prefix := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\n"
	upstream := &geminiBlockingUpstream{headersFirst: true, prefix: prefix}
	svc := newGeminiTimeoutService(upstream, 0, 1)
	c, rec := newGeminiNativeTestContext(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the idle timeout")
	}
	require.Contains(t, rec.Body.String(), "Hello")
}

func TestGeminiForwardNative_PeekReadErrorFailsOver(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusOK, "")
	stub.response.Body = io.NopCloser(io.MultiReader(
		strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hmm\",\"thought\":true}]}}]}\n\n"),
		geminiStickyErrReader{err: io.ErrUnexpectedEOF},
	))
	c, _ := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusBadGateway, GeminiStreamReadErrorReason)
}

func TestGeminiForwardNative_NonStreamHasNoFirstByteTimeout(t *testing.T) {
	svc := newGeminiTimeoutService(nil, 1, 1)
	require.Zero(t, svc.geminiStreamFirstByteTimeout(false))
	require.Zero(t, svc.geminiStreamIdleTimeout(false))
	require.Equal(t, time.Second, svc.geminiStreamFirstByteTimeout(true))
	require.Equal(t, time.Second, svc.geminiStreamIdleTimeout(true))
}

func TestGeminiForwardAsChatCompletions_402FailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusPaymentRequired, `{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`)
	c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), []byte(fmt.Sprintf(geminiChatTestBody, "true")))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusPaymentRequired, GeminiPaymentRequiredReason)
}

func TestGeminiForwardAsChatCompletions_ConnectionErrorFailsOver(t *testing.T) {
	svc, stub := newGeminiSkippedWriteService(http.StatusOK, "")
	stub.err = errors.New("dial tcp: i/o timeout")
	c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), []byte(fmt.Sprintf(geminiChatTestBody, "true")))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusBadGateway, GeminiUpstreamRequestErrorReason)
	require.Equal(t, 2, stub.calls)
}

func TestGeminiForwardAsChatCompletions_FirstByteTimeoutFailsOver(t *testing.T) {
	svc := newGeminiTimeoutService(&geminiBlockingUpstream{headersFirst: true}, 1, 0)
	c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), []byte(fmt.Sprintf(geminiChatTestBody, "true")))
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusGatewayTimeout, GeminiFirstByteTimeoutReason)
}

func TestGeminiForwardMessages_404FailsOver(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusNotFound, `{"error":{"code":404,"message":"model not found","status":"NOT_FOUND"}}`)
	c, _ := newGeminiEmptyTestContext("/v1/messages")
	body := []byte(`{"model":"gemini-4-carbon","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	_, err := svc.Forward(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	_ = requireGeminiImmediateFailover(t, c, err, http.StatusNotFound, GeminiCapabilityMissReason)
}

func TestGeminiUpstreamWatchdog_NilAndRelease(t *testing.T) {
	var w *geminiUpstreamWatchdog
	w.observeRead(10)
	w.release()
	_, ok := w.timedOut()
	require.False(t, ok)

	ctx, w2 := newGeminiUpstreamWatchdog(context.Background(), 0, 0)
	require.Nil(t, w2)
	require.NoError(t, ctx.Err())

	ctx, w3 := newGeminiUpstreamWatchdog(context.Background(), 50*time.Millisecond, 0)
	w3.observeRead(1) // 首字节到了：不再超时
	time.Sleep(120 * time.Millisecond)
	_, ok = w3.timedOut()
	require.False(t, ok)
	require.NoError(t, ctx.Err())
	w3.release()
	require.Error(t, ctx.Err())
	_, ok = w3.timedOut()
	require.False(t, ok, "release is not a timeout")
}

func TestSleepGeminiBackoffCtx_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleepGeminiBackoffCtx(ctx, 3), context.Canceled)
}
