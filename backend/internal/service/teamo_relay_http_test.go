package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type teamoRelayHTTPTransport struct{ client *http.Client }

func (u *teamoRelayHTTPTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}
func (u *teamoRelayHTTPTransport) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}

func TestTeamoRelayForwardAutoCapabilityRecoversBeforeOutput(t *testing.T) {
	var chatRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream endpoint: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if chatRequests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"error\":{\"code\":\"server_error\",\"message\":\"temporarily unavailable\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"chat_ok\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway:  config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}},
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
	}, httpUpstream: &teamoRelayHTTPTransport{client: upstream.Client()}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test-key", "base_url": upstream.URL}, Extra: map[string]any{"openai_responses_mode": "auto", "openai_responses_supported": false}}
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	enableTeamoRelayTestGroup(c)
	_, err := svc.Forward(context.Background(), c, account, body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Empty(t, rec.Body.String(), "failed attempt leaked adapter prefix")
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.EqualValues(t, 2, chatRequests.Load())
	require.Contains(t, rec.Body.String(), "hello")
	require.Contains(t, rec.Body.String(), "response.completed")
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.CacheReadInputTokens)
	require.Equal(t, "auto", account.Extra["openai_responses_mode"])
}

func TestTeamoRelayAbsoluteReadBudgetSurvivesPreamble(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
	r := svc.newTeamoRelayScanner(c, resp, func() bool { return false }, time.Now().Add(-950*time.Millisecond), "")
	defer r.Close()
	started := time.Now()
	for r.Scan() {
	}
	require.ErrorContains(t, r.Err(), "timeout")
	require.Less(t, time.Since(started), time.Second)
}

type relayCancelWriter struct {
	gin.ResponseWriter
	once   sync.Once
	seen   chan struct{}
	cancel context.CancelFunc
}

func (w *relayCancelWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if bytes.Contains(p, []byte("hello")) {
		w.once.Do(func() { w.cancel(); close(w.seen) })
	}
	return n, err
}
func (w *relayCancelWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }

func TestTeamoRelayCommittedCancellationStillDrainsUsage(t *testing.T) {
	seen := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	c.Writer = &relayCancelWriter{ResponseWriter: c.Writer, cancel: cancel, seen: seen}
	enableTeamoRelayTestGroup(c)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}, StreamDataIntervalTimeout: 1}}}
	result, err := svc.streamRawChatCompletions(c, resp, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "model", "model", "model", nil, nil, time.Now(), 20)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.CacheReadInputTokens)
}
