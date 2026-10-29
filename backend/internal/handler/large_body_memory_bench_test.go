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

// 大请求体内存基准：模拟 10-29 打满 HK 在途预算的 Blender Agent 负载（单个请求体 10–15MB，
// 几百条 input、带 base64 截图），量化一个请求从读请求体到发给上游、再到流式响应结束，
// 进程里分配和驻留了多少字节。
//
//	go test ./internal/handler -run '^$' -bench LargeResponsesBody -benchmem
//	LARGE_BODY_RESIDENCY=1 go test ./internal/handler -run TestLargeResponsesBodyResidency -v
//
// 驻留数据在 TestLargeResponsesBodyResidency 里测：上游收到请求时、流式响应写到一半时各做一次
// runtime.GC，看存活堆比请求前多了多少（benchmark 里不能 GC，会搅乱耗时）。

const largeBodyTargetBytes = 12 << 20

func buildLargeResponsesBody(tb testing.TB, target int, stream bool) []byte {
	tb.Helper()
	rng := rand.New(rand.NewSource(1))
	img := make([]byte, 600<<10)
	rng.Read(img)
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	text := strings.Repeat("Blender scene graph: move cube_042 to (1.25, -0.5, 3.0), apply bevel modifier. ", 24)

	input := make([]any, 0, 512)
	tools := make([]any, 0, 24)
	for i := 0; i < 24; i++ {
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        fmt.Sprintf("blender_op_%02d", i),
			"description": "Execute a Blender operator with the given arguments.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"object": map[string]any{"type": "string"},
					"args":   map[string]any{"type": "object"},
				},
				"required": []string{"object"},
			},
		})
	}
	size := 0
	for i := 0; size < target; i++ {
		var item map[string]any
		switch i % 4 {
		case 0:
			content := []any{map[string]any{"type": "input_text", "text": text}}
			if i%24 == 0 {
				content = append(content, map[string]any{"type": "input_image", "image_url": imageURL})
				size += len(imageURL)
			}
			item = map[string]any{"type": "message", "role": "user", "content": content}
		case 1:
			item = map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call_%04d", i), "name": "blender_op_03", "arguments": `{"object":"cube_042","args":{"x":1.25}}`}
		case 2:
			item = map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("call_%04d", i-1), "output": text}
		default:
			item = map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
		}
		input = append(input, item)
		size += len(text) + 120
	}
	body, err := json.Marshal(map[string]any{
		"model":        "gpt-6-astra",
		"stream":       stream,
		"instructions": "You are a Blender 3D modelling agent.",
		"input":        input,
		"tools":        tools,
		"store":        false,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

// largeBodyUpstream 读完上游请求体后回一段 SSE（或 JSON）。probe 非 nil 时，在收到请求和
// 响应写到一半这两个时间点各调用一次（用于测驻留）。
type largeBodyUpstream struct {
	service.HTTPUpstream
	stream        bool
	upstreamBytes atomic.Int64
	probe         func(stage string)
}

func (u *largeBodyUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	n, _ := io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	u.upstreamBytes.Store(n)
	if u.probe != nil {
		u.probe("upstream_sent")
	}
	if !u.stream {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_large","object":"response","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":100000,"output_tokens":10,"total_tokens":100010}}`)),
		}, nil
	}
	var head, tail bytes.Buffer
	head.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_large\",\"model\":\"gpt-6-astra\",\"status\":\"in_progress\"}}\n\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&head, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"tok%d \"}\n\n", i)
	}
	tail.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_large\",\"model\":\"gpt-6-astra\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100000,\"output_tokens\":20,\"total_tokens\":100020}}}\n\n")
	body := io.MultiReader(&head, &probeReader{fn: func() {
		if u.probe != nil {
			u.probe("streaming")
		}
	}}, &tail)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(body),
	}, nil
}

func (u *largeBodyUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

type chunkedBodyReader struct {
	data  []byte
	chunk int
}

func (r *chunkedBodyReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.chunk, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// probeReader 第一次被读时调用 fn，然后返回 EOF。
type probeReader struct {
	fn   func()
	done bool
}

func (r *probeReader) Read([]byte) (int, error) {
	if !r.done {
		r.done = true
		r.fn()
	}
	return 0, io.EOF
}

type largeBodyCase struct {
	name    string
	account service.Account
	stream  bool
}

func largeBodyCases() []largeBodyCase {
	apiKey := service.Account{
		ID: 9801, Name: "large-body-apikey", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 0,
		Credentials: map[string]any{"api_key": "sk-large", "base_url": "https://api.example.test"},
	}
	passthrough := apiKey
	passthrough.ID = 9802
	passthrough.Extra = map[string]any{"openai_passthrough": true}
	return []largeBodyCase{
		{name: "apikey/stream", account: apiKey, stream: true},
		{name: "apikey/nonstream", account: apiKey, stream: false},
		{name: "passthrough/stream", account: passthrough, stream: true},
	}
}

type largeBodyHarness struct {
	h        *OpenAIGatewayHandler
	upstream *largeBodyUpstream
	groupID  int64
	stop     func()
}

func newLargeBodyHarness(tb testing.TB, account service.Account, stream bool) *largeBodyHarness {
	tb.Helper()
	gin.SetMode(gin.ReleaseMode)
	groupID := int64(6101)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.MaxBodySize = 64 << 20
	cfg.Gateway.MaxAccountSwitches = 1

	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	upstream := &largeBodyUpstream{stream: stream}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewOpenAIGatewayHandler(
		gatewaySvc, service.NewConcurrencyService(nil), billingCacheSvc,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	return &largeBodyHarness{h: h, upstream: upstream, groupID: groupID, stop: billingCacheSvc.Stop}
}

func (hs *largeBodyHarness) serve(tb testing.TB, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// 按 32KB 分段返回、不实现 WriterTo：和 net/http 服务端的请求体一样，读的一方只能边读边扩容。
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", &chunkedBodyReader{data: body, chunk: 32 << 10})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	c.Request = req
	groupID := hs.groupID
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 5501, GroupID: &groupID,
		User:  &service.User{ID: 5502, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 5502, Concurrency: 0})
	hs.h.Responses(c)
	if rec.Code != http.StatusOK {
		tb.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := hs.upstream.upstreamBytes.Load(); got == 0 {
		tb.Fatal("upstream received empty body")
	}
	return rec
}

func BenchmarkLargeResponsesBody(b *testing.B) {
	for _, tc := range largeBodyCases() {
		b.Run(tc.name, func(b *testing.B) {
			body := buildLargeResponsesBody(b, largeBodyTargetBytes, tc.stream)
			hs := newLargeBodyHarness(b, tc.account, tc.stream)
			defer hs.stop()
			hs.serve(b, body) // 预热：首个请求初始化的缓存不计入
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
			perOp := float64(after.TotalAlloc-before.TotalAlloc) / float64(b.N)
			b.ReportMetric(perOp/float64(len(body)), "alloc_x_body")
		})
	}
}

// liveHeap 做两次完整 GC（sync.Pool 的 victim 缓存要第二次才清）后返回存活堆字节数。
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// TestLargeResponsesBodyResidency 打印一个 12MB 请求在两个时间点的额外存活堆：上游刚读完请求体时
// （此时请求体的各个副本都可能还在），以及流式响应写到一半时（理想情况下请求体已经可以回收）。
func TestLargeResponsesBodyResidency(t *testing.T) {
	if os.Getenv("LARGE_BODY_RESIDENCY") == "" {
		t.Skip("set LARGE_BODY_RESIDENCY=1 to measure")
	}
	for _, tc := range largeBodyCases() {
		t.Run(tc.name, func(t *testing.T) {
			hs := newLargeBodyHarness(t, tc.account, tc.stream)
			defer hs.stop()
			// 用小请求体预热，基线里不带任何大请求的残留；客户端请求体本身（测试持有的 body）不算网关的驻留。
			hs.serve(t, buildLargeResponsesBody(t, 1, tc.stream))
			body := buildLargeResponsesBody(t, largeBodyTargetBytes, tc.stream)
			time.Sleep(200 * time.Millisecond)
			base := liveHeap()
			samples := map[string]int64{}
			hs.upstream.probe = func(stage string) { samples[stage] = liveHeap() - base }
			hs.serve(t, body)
			hs.upstream.probe = nil
			after := liveHeap() - base
			time.Sleep(500 * time.Millisecond)
			settled := liveHeap() - base
			t.Logf("%-20s body=%.1fMB upstream=%.1fMB  live@upstream_sent=%.1fMB (%.2fx)  live@streaming=%.1fMB (%.2fx)  live@return=%.1fMB live@+500ms=%.1fMB",
				tc.name, mb(len(body)), mb(int(hs.upstream.upstreamBytes.Load())),
				mb(int(samples["upstream_sent"])), float64(samples["upstream_sent"])/float64(len(body)),
				mb(int(samples["streaming"])), float64(samples["streaming"])/float64(len(body)),
				mb(int(after)), mb(int(settled)))
			runtime.KeepAlive(body)
		})
	}
}

func mb(n int) float64 { return float64(n) / (1 << 20) }

// TestLargeResponsesBodyConcurrentPeak 让 concurrency 个 12MB 请求同时在途（上游读完请求体后各挂 hold），
// 每 2ms 采样一次 HeapInuse 和 Sys，报告峰值相对「在途请求体总字节」的倍数，对应生产上
// 「在途请求体 1.1GB 时 RSS 6.7GB」的放大。用默认 GOGC、不设 GOMEMLIMIT。
func TestLargeResponsesBodyConcurrentPeak(t *testing.T) {
	if os.Getenv("LARGE_BODY_RESIDENCY") == "" {
		t.Skip("set LARGE_BODY_RESIDENCY=1 to measure")
	}
	const concurrency, rounds = 40, 3
	const hold = 300 * time.Millisecond
	tc := largeBodyCases()[0]
	hs := newLargeBodyHarness(t, tc.account, tc.stream)
	defer hs.stop()
	body := buildLargeResponsesBody(t, largeBodyTargetBytes, tc.stream)
	hs.serve(t, body)
	hs.upstream.probe = func(stage string) {
		if stage == "upstream_sent" {
			time.Sleep(hold)
		}
	}
	base := liveHeap()
	var gcBefore runtime.MemStats
	runtime.ReadMemStats(&gcBefore)

	var peakInuse, peakSys uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)
				peakInuse = max(peakInuse, ms.HeapInuse)
				peakSys = max(peakSys, ms.HeapSys-ms.HeapReleased)
			}
		}
	}()
	started := time.Now()
	done := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for r := 0; r < rounds; r++ {
				hs.serve(t, body)
			}
		}()
	}
	for i := 0; i < concurrency; i++ {
		<-done
	}
	elapsed := time.Since(started)
	close(stop)
	<-sampled
	var gcAfter runtime.MemStats
	runtime.ReadMemStats(&gcAfter)
	inflight := float64(concurrency * len(body))
	t.Logf("concurrency=%d body=%.1fMB inflight=%.0fMB  peak_heap_inuse=%.0fMB (%.2fx inflight)  peak_heap_mapped=%.0fMB (%.2fx)  gc_cycles=%d  gc_cpu_fraction=%.3f  wall=%s",
		concurrency, mb(len(body)), inflight/(1<<20),
		float64(peakInuse-uint64(base))/(1<<20), float64(peakInuse-uint64(base))/inflight,
		float64(peakSys)/(1<<20), float64(peakSys)/inflight,
		gcAfter.NumGC-gcBefore.NumGC, gcAfter.GCCPUFraction, elapsed.Round(time.Millisecond))
	runtime.KeepAlive(body)
}
