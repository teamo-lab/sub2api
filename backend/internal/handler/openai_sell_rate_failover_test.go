//go:build unit

package handler

import (
	"bytes"
	"context"
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
)

// sellRateFailoverUpstream fails the first (more expensive) account with a 5xx and
// lets the second account answer, so the request is served after one failover hop.
type sellRateFailoverUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	calls    []int64
	failedID int64
}

func (u *sellRateFailoverUpstream) Do(_ *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, id)
	u.mu.Unlock()
	if id == u.failedID {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"upstream_error","message":"synthetic bad gateway"}}`)),
		}, nil
	}
	body := `{"id":"resp_sell_rate","object":"response","created_at":1700000000,"model":"gpt-5.1","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
		`"usage":{"input_tokens":1000,"output_tokens":200}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

// failover 换号后按最终成功服务的账号售价计价；失败跳不产生用量记录（不计费）。
func TestOpenAIResponsesFailoverBillsFinalAccountSellRate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	expensiveSell, cheapSell := 4.2, 1.1
	upstream := &sellRateFailoverUpstream{failedID: 22}
	usage := &lateFailureUsage{}
	creds := map[string]any{"api_key": "synthetic-only", "base_url": "https://example.invalid"}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: 22, Name: "first-hop", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 1, SellRateMultiplier: &expensiveSell, Credentials: creds},
		{ID: 23, Name: "final", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 3, Priority: 10, SellRateMultiplier: &cheapSell, Credentials: creds},
	}}
	cache := &lateFailureCache{helperConcurrencyCacheStub: helperConcurrencyCacheStub{userSeq: []bool{true}}}
	concurrency := service.NewConcurrencyService(cache)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream, service.AccountTypeAPIKey)
	h.gatewayService = service.NewOpenAIGatewayService(accounts, usage, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, nil, upstream, service.NewDeferredService(accounts, nil, time.Hour), nil, nil, nil, nil, nil, nil, nil)
	h.concurrencyHelper = NewConcurrencyHelper(concurrency, SSEPingFormatNone, 0)

	groupID := int64(5)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 2.0, SubscriptionType: service.SubscriptionTypeStandard}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.1","stream":false,"input":"hello"}`)).WithContext(ctx)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 951, UserID: 952, GroupID: &groupID, Group: group, User: &service.User{ID: 952}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 952, Concurrency: 1})

	h.Responses(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []int64{22, 23}, upstream.calls, "首跳失败后必须换号")
	require.Len(t, usage.rows, 1, "失败跳不计费，只有最终成功账号产生一条用量")
	row := usage.rows[0]
	require.Equal(t, int64(23), row.AccountID)
	require.InDelta(t, cheapSell, row.RateMultiplier, 1e-12, "按最终账号售价计价，而不是首跳账号或分组价")
	require.Greater(t, row.TotalCost, 0.0)
	require.InDelta(t, row.TotalCost*cheapSell, row.ActualCost, 1e-10)
}
