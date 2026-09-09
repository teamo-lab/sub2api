//go:build unit

package handler

import (
	"bytes"
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type completedDrainBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *completedDrainBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		<-b.closed
	}
	return n, e
}
func (b *completedDrainBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type completedDrainUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *completedDrainUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls++
	data := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_completed\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &completedDrainBody{Reader: strings.NewReader(data), closed: make(chan struct{})}}, nil
}
func TestResponsesCompletedIdleRecordsUsageExactlyOnce(t *testing.T) {
	u := &completedDrainUpstream{}
	usage := &lateFailureUsage{}
	cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true}}}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.StreamDataIntervalTimeout = 1
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{{ID: 23, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1, Extra: map[string]any{"use_responses_api": true}, Credentials: map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid"}}}}
	h := newOpenAIResponsesFailoverTestHandler(t, u, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, u, service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)
	groupID := int64(3)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.6-sol","stream":true,"input":"test"}`)).WithContext(ctx)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 901, UserID: 902, GroupID: &groupID, Group: group, User: &service.User{ID: 902}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 902, Concurrency: 1})
	h.Responses(c)
	require.Equal(t, 1, u.calls)
	require.Len(t, usage.rows, 1)
	require.Equal(t, int64(23), usage.rows[0].AccountID)
	require.Equal(t, 17, usage.rows[0].InputTokens)
	require.Equal(t, 3, usage.rows[0].OutputTokens)
	require.Equal(t, 1, strings.Count(w.Body.String(), `"type":"response.completed"`))
	require.NotContains(t, w.Body.String(), "stream_timeout")
	require.Equal(t, cache.accountAcquireCalls, cache.accountReleaseCalls)
}
