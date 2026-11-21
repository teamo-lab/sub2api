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
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const ccHandlerTerminalStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_handler\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\n"

type ccTerminalHTTPUpstream struct {
	raw            string
	accountIDs     []int64
	beforeResponse func()
}

func (u *ccTerminalHTTPUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, id)
	body, _ := io.ReadAll(req.Body)
	if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "max_tokens").Int() != 6144 {
		return nil, fmt.Errorf("test request changed upstream parameters")
	}
	if u.beforeResponse != nil {
		u.beforeResponse()
	}
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	header.Set("x-request-id", fmt.Sprintf("supplier-%d", id))
	return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(u.raw))}, nil
}
func (u *ccTerminalHTTPUpstream) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, n)
}

func newCCTerminalHandler(t *testing.T, u *ccTerminalHTTPUpstream) (*GatewayHandler, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(9200)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	accounts := []*service.Account{}
	for _, id := range []int64{9201, 9202} {
		accounts = append(accounts, &service.Account{ID: id, Name: fmt.Sprintf("test-%d", id), Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: map[string]any{"api_key": "synthetic-test-key", "base_url": "https://api.anthropic.example.test"}, AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: groupID}}})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: accounts}, nil, nil, nil, nil)
	gw := service.NewGatewayService(nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg, snapshot, nil, service.NewBillingService(cfg, nil), nil, nil, nil, u, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := &GatewayHandler{gatewayService: gw, billingCacheService: billing, concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0), maxAccountSwitches: 1, cfg: cfg}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
	key := &service.APIKey{ID: 9203, UserID: 9204, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 9204, Concurrency: 0, Balance: 100}}
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 0})
	return h, c, rec
}
func TestGatewayChatTerminalFailuresUseBoundedFailover(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		stream    bool
	}{
		{"buffered_start_then_eof", ccHandlerTerminalStart, false},
		{"buffered_sse_error", ccHandlerTerminalStart + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", false},
		{"stream_error_before_output", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := &ccTerminalHTTPUpstream{raw: tt.raw}
			h, c, rec := newCCTerminalHandler(t, u)
			c.Request.Body = io.NopCloser(bytes.NewBufferString(fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":6144,"stream":%t,"messages":[{"role":"user","content":"hello"}]}`, tt.stream)))
			h.ChatCompletions(c)
			require.Len(t, u.accountIDs, 2, "one initial attempt and one configured account switch")
			require.NotEqual(t, u.accountIDs[0], u.accountIDs[1])
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			require.True(t, gjson.ValidBytes(rec.Body.Bytes()))
			require.Equal(t, "server_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.NotContains(t, rec.Body.String(), "choices")
		})
	}
}
func TestGatewayChatPartialStreamFailsWithoutSplicingOrSuccessfulFinish(t *testing.T) {
	raw := ccHandlerTerminalStart + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\n"
	u := &ccTerminalHTTPUpstream{raw: raw}
	h, c, rec := newCCTerminalHandler(t, u)
	c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"claude-sonnet-5-5","max_tokens":6144,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	h.ChatCompletions(c)
	require.Len(t, u.accountIDs, 1, "partial output must never be retried or spliced")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "partial answer")
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"error":`))
	require.Contains(t, rec.Body.String(), `"code":"upstream_stream_error"`)
	require.NotContains(t, rec.Body.String(), "[DONE]")
	require.NotContains(t, rec.Body.String(), `"finish_reason":"stop"`)
	failure, ok := service.GetOpsStreamError(c)
	require.True(t, ok)
	require.True(t, failure.CountTowardsSLA)
	require.Equal(t, http.StatusBadGateway, failure.IntendedStatus)
}
func TestGatewayChatCanceledDuringUpstreamDoesNotRetry(t *testing.T) {
	u := &ccTerminalHTTPUpstream{raw: ccHandlerTerminalStart}
	h, c, rec := newCCTerminalHandler(t, u)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	u.beforeResponse = cancel
	c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"claude-sonnet-5-5","max_tokens":6144,"messages":[{"role":"user","content":"hello"}]}`))
	h.ChatCompletions(c)
	require.Len(t, u.accountIDs, 1)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestGatewayChatStreamFailureTerminalIsNotDuplicated(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	_, err := c.Writer.WriteString(": heartbeat\n\n")
	require.NoError(t, err)
	h := &GatewayHandler{}
	failure := &service.UpstreamFailoverError{StatusCode: 502}
	h.handleCCFailoverExhausted(c, failure, true)
	h.handleCCFailoverExhausted(c, failure, true)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"error":`))
	require.NotContains(t, rec.Body.String(), "[DONE]")
}
func TestGatewayChatStreamFailurePreservesExistingJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.JSON(502, gin.H{"error": gin.H{"message": "original failure"}})
	before := rec.Body.String()
	(&GatewayHandler{}).handleCCFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: 502}, true)
	require.Equal(t, before, rec.Body.String())
}
