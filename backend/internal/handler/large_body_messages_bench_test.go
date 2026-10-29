//go:build unit

package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// /v1/messages（Anthropic API Key 账号）的大请求体基准，口径同 large_body_memory_bench_test.go：
//
//	go test ./internal/handler -run '^$' -bench LargeMessagesBody -benchmem
//	LARGE_BODY_RESIDENCY=1 go test ./internal/handler -run TestLargeMessagesBodyResidency -v

func buildLargeMessagesBody(tb testing.TB, target int) []byte {
	tb.Helper()
	rng := rand.New(rand.NewSource(2))
	img := make([]byte, 600<<10)
	rng.Read(img)
	imageData := base64.StdEncoding.EncodeToString(img)
	text := strings.Repeat("Modeler scene graph: move cube_042 to (1.25, -0.5, 3.0), apply bevel modifier. ", 24)
	tools := make([]any, 0, 24)
	for i := 0; i < 24; i++ {
		tools = append(tools, map[string]any{
			"name":         fmt.Sprintf("modeler_op_%02d", i),
			"description":  "Execute a Modeler operator with the given arguments.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"object": map[string]any{"type": "string"}}},
		})
	}
	messages := make([]any, 0, 512)
	size := 0
	for i := 0; size < target; i++ {
		id := fmt.Sprintf("toolu_%04d", i/2)
		if i%2 == 0 {
			content := []any{map[string]any{"type": "text", "text": text}}
			if i > 0 {
				content = append([]any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("toolu_%04d", i/2-1), "content": text}}, content...)
			}
			if i%12 == 0 {
				content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imageData}})
				size += len(imageData)
			}
			messages = append(messages, map[string]any{"role": "user", "content": content})
		} else {
			messages = append(messages, map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": text},
				map[string]any{"type": "tool_use", "id": id, "name": "modeler_op_03", "input": map[string]any{"object": "cube_042"}},
			}})
		}
		size += 2*len(text) + 200
	}
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 4096, "stream": true,
		"system": "You are a Modeler 3D modelling agent.", "messages": messages, "tools": tools,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

type largeMessagesUpstream struct {
	service.HTTPUpstream
	upstreamBytes atomic.Int64
	probe         func(stage string)
}

func (u *largeMessagesUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	n, _ := io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	u.upstreamBytes.Store(n)
	if u.probe != nil {
		u.probe("upstream_sent")
	}
	var head bytes.Buffer
	head.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_large\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":100000,\"output_tokens\":1}}}\n\n")
	head.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&head, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tok%d \"}}\n\n", i)
	}
	tail := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := io.MultiReader(&head, &probeReader{fn: func() {
		if u.probe != nil {
			u.probe("streaming")
		}
	}}, strings.NewReader(tail))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(body)}, nil
}

func (u *largeMessagesUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

type largeMessagesHarness struct {
	h        *GatewayHandler
	upstream *largeMessagesUpstream
	group    *service.Group
	stop     func()
}

func newLargeMessagesHarness(tb testing.TB) *largeMessagesHarness {
	tb.Helper()
	gin.SetMode(gin.ReleaseMode)
	groupID := int64(6102)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: 9811, Name: "large-body-anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-ant-large", "base_url": "https://api.anthropic.example.test"},
		Concurrency: 0, Priority: 1, Status: service.StatusActive, Schedulable: true,
		AccountGroups: []service.AccountGroup{{AccountID: 9811, GroupID: groupID}},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxBodySize = 64 << 20
	upstream := &largeMessagesUpstream{}
	schedulerSnapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gwSvc := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg, schedulerSnapshot,
		nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream, &service.DeferredService{},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	h := &GatewayHandler{
		gatewayService:      gwSvc,
		billingCacheService: billingCacheSvc,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		cfg:                 cfg,
		maxAccountSwitches:  1,
	}
	return &largeMessagesHarness{h: h, upstream: upstream, group: group, stop: billingCacheSvc.Stop}
}

func (hs *largeMessagesHarness) serve(tb testing.TB, body []byte) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &chunkedBodyReader{data: body, chunk: 32 << 10})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	c.Request = req
	groupID := hs.group.ID
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 5511, GroupID: &groupID, Group: hs.group,
		User: &service.User{ID: 5512, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 5512, Concurrency: 0})
	hs.h.Messages(c)
	if rec.Code != http.StatusOK || hs.upstream.upstreamBytes.Load() == 0 {
		tb.Fatalf("status=%d upstream=%d body=%.300s", rec.Code, hs.upstream.upstreamBytes.Load(), rec.Body.String())
	}
}

func BenchmarkLargeMessagesBody(b *testing.B) {
	body := buildLargeMessagesBody(b, largeBodyTargetBytes)
	hs := newLargeMessagesHarness(b)
	defer hs.stop()
	hs.serve(b, body)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hs.serve(b, body)
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/float64(b.N)/float64(len(body)), "alloc_x_body")
}

func TestLargeMessagesBodyResidency(t *testing.T) {
	if os.Getenv("LARGE_BODY_RESIDENCY") == "" {
		t.Skip("set LARGE_BODY_RESIDENCY=1 to measure")
	}
	hs := newLargeMessagesHarness(t)
	defer hs.stop()
	hs.serve(t, buildLargeMessagesBody(t, 1))
	body := buildLargeMessagesBody(t, largeBodyTargetBytes)
	time.Sleep(200 * time.Millisecond)
	base := liveHeap()
	samples := map[string]int64{}
	hs.upstream.probe = func(stage string) { samples[stage] = liveHeap() - base }
	hs.serve(t, body)
	hs.upstream.probe = nil
	after := liveHeap() - base
	t.Logf("messages/apikey/stream body=%.1fMB upstream=%.1fMB  live@upstream_sent=%.1fMB (%.2fx)  live@streaming=%.1fMB (%.2fx)  live@return=%.1fMB",
		mb(len(body)), mb(int(hs.upstream.upstreamBytes.Load())),
		mb(int(samples["upstream_sent"])), float64(samples["upstream_sent"])/float64(len(body)),
		mb(int(samples["streaming"])), float64(samples["streaming"])/float64(len(body)), mb(int(after)))
	runtime.KeepAlive(body)
}
