//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// Gemini 三个入口的大请求体存活内存：上游刚读完请求体时、流式响应写到一半时，
// 除调用方持有的那一份请求体之外还额外存活多少堆。HK 生产上每条在途 Gemini 流约 10–15MB，
// 对应编程 Agent 2–4MB 的上下文被复制多份、整条流期间都没释放。
//
//	GEMINI_LARGE_BODY_RESIDENCY=1 go test -tags=unit ./internal/service -run TestGeminiLargeBodyResidency -v

const geminiLargeBodyTarget = 3 << 20

type geminiResidencyUpstream struct {
	upstreamBytes atomic.Int64
	probe         func(stage string)
}

type geminiProbeReader struct {
	fn   func()
	done bool
}

func (r *geminiProbeReader) Read([]byte) (int, error) {
	if !r.done {
		r.done = true
		if r.fn != nil {
			r.fn()
		}
	}
	return 0, io.EOF
}

func (u *geminiResidencyUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	n, _ := io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	u.upstreamBytes.Store(n)
	if u.probe != nil {
		u.probe("upstream_sent")
	}
	var head bytes.Buffer
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&head, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"tok%d \"}]}}]}\n\n", i)
	}
	tail := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":100000,\"candidatesTokenCount\":21,\"totalTokenCount\":100021}}\n\n"
	body := io.MultiReader(&head, &geminiProbeReader{fn: func() {
		if u.probe != nil {
			u.probe("streaming")
		}
	}}, strings.NewReader(tail))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(body)}, nil
}

func (u *geminiResidencyUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// 编程 Agent 形态：长对话、每轮带文件内容的工具调用和工具结果。
func geminiAgentTurnText(i int) string {
	return strings.Repeat(fmt.Sprintf("func handler%04d(w http.ResponseWriter, r *http.Request) { // read config, validate input, call service layer and write JSON\n", i), 12)
}

func buildGeminiNativeLargeBody(tb testing.TB, target int) []byte {
	tb.Helper()
	contents := make([]any, 0, 256)
	size := 0
	for i := 0; size < target; i++ {
		text := geminiAgentTurnText(i)
		contents = append(contents,
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}},
			map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "read_file", "args": map[string]any{"path": fmt.Sprintf("pkg/f%04d.go", i)}}}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "read_file", "response": map[string]any{"content": text}}}}},
		)
		size += 2*len(text) + 300
	}
	body, err := json.Marshal(map[string]any{
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "You are a coding agent."}}},
		"contents":          contents,
		"generationConfig":  map[string]any{"maxOutputTokens": 8192},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

func buildGeminiChatLargeBody(tb testing.TB, target int) []byte {
	tb.Helper()
	messages := []any{map[string]any{"role": "system", "content": "You are a coding agent."}}
	size := 0
	for i := 0; size < target; i++ {
		text := geminiAgentTurnText(i)
		id := fmt.Sprintf("call_%04d", i)
		messages = append(messages,
			map[string]any{"role": "user", "content": text},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": fmt.Sprintf(`{"path":"pkg/f%04d.go"}`, i)}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": text},
		)
		size += 2*len(text) + 300
	}
	body, err := json.Marshal(map[string]any{
		"model": "gemini-3.8-flash", "stream": true, "messages": messages,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

func buildGeminiClaudeLargeBody(tb testing.TB, target int) []byte {
	tb.Helper()
	messages := make([]any, 0, 256)
	size := 0
	for i := 0; size < target; i++ {
		text := geminiAgentTurnText(i)
		id := fmt.Sprintf("toolu_%04d", i)
		messages = append(messages,
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "read_file", "input": map[string]any{"path": fmt.Sprintf("pkg/f%04d.go", i)}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text}}},
		)
		size += 2*len(text) + 300
	}
	messages = append(messages, map[string]any{"role": "user", "content": "continue"})
	body, err := json.Marshal(map[string]any{
		"model": "gemini-3.8-flash", "max_tokens": 8192, "stream": true, "system": "You are a coding agent.", "messages": messages,
		"tools": []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return body
}

func geminiResidencyLiveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

func TestGeminiLargeBodyResidency(t *testing.T) {
	if os.Getenv("GEMINI_LARGE_BODY_RESIDENCY") == "" {
		t.Skip("set GEMINI_LARGE_BODY_RESIDENCY=1 to measure")
	}
	mbf := func(n int64) float64 { return float64(n) / (1 << 20) }
	account := &Account{ID: 9901, Platform: PlatformGemini, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "k", "base_url": "https://gemini.example.test"}}

	cases := []struct {
		name string
		body []byte
		run  func(svc *GeminiMessagesCompatService, body []byte) error
	}{
		{"native/stream", buildGeminiNativeLargeBody(t, geminiLargeBodyTarget), func(svc *GeminiMessagesCompatService, body []byte) error {
			c, _ := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse")
			_, err := svc.ForwardNative(context.Background(), c, account, "gemini-3.8-flash", "streamGenerateContent", true, body)
			return err
		}},
		{"chat_completions/stream", buildGeminiChatLargeBody(t, geminiLargeBodyTarget), func(svc *GeminiMessagesCompatService, body []byte) error {
			c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &geminiResidencyUpstream{}
			svc := &GeminiMessagesCompatService{httpUpstream: up, cfg: &config.Config{},
				rateLimitService: NewRateLimitService(&errorPolicyRepoStub{}, nil, &config.Config{}, nil, nil)}
			// 预热一次，避免把一次性初始化算进去。
			_ = tc.run(svc, tc.body)
			time.Sleep(100 * time.Millisecond)
			base := geminiResidencyLiveHeap()
			samples := map[string]int64{}
			up.probe = func(stage string) {
				samples[stage] = geminiResidencyLiveHeap() - base
				if dir := os.Getenv("GEMINI_LARGE_BODY_HEAP_DIR"); dir != "" && stage == "streaming" {
					f, err := os.Create(dir + "/" + strings.ReplaceAll(tc.name, "/", "_") + ".heap")
					if err == nil {
						_ = pprof.Lookup("heap").WriteTo(f, 0)
						_ = f.Close()
					}
				}
			}
			if err := tc.run(svc, tc.body); err != nil {
				t.Fatalf("forward: %v", err)
			}
			up.probe = nil
			after := geminiResidencyLiveHeap() - base
			n := int64(len(tc.body))
			t.Logf("%-24s body=%.2fMB upstream=%.2fMB  extra live@upstream_sent=%.2fMB (%.2fx)  @streaming=%.2fMB (%.2fx)  @return=%.2fMB",
				tc.name, mbf(n), mbf(up.upstreamBytes.Load()),
				mbf(samples["upstream_sent"]), float64(samples["upstream_sent"])/float64(n),
				mbf(samples["streaming"]), float64(samples["streaming"])/float64(n), mbf(after))
			runtime.KeepAlive(tc.body)
		})
	}
}

// realHTTPResidencyUpstream 走真实的 http.Client 和 httptest 服务端：生产上 Transport 在读完响应之前
// 一直引用发出去的请求（以及它的 Body / GetBody），桩上游看不到这一层。
type realHTTPResidencyUpstream struct {
	client *http.Client
	target string
}

func (u *realHTTPResidencyUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host = "http", u.target
	req.Host = u.target
	return u.client.Do(req)
}

func (u *realHTTPResidencyUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestGeminiLargeBodyResidencyRealHTTP(t *testing.T) {
	if os.Getenv("GEMINI_LARGE_BODY_RESIDENCY") == "" {
		t.Skip("set GEMINI_LARGE_BODY_RESIDENCY=1 to measure")
	}
	mbf := func(n int64) float64 { return float64(n) / (1 << 20) }
	var probe atomic.Value // func(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"tok%d \"}]}}]}\n\n", i)
			fl.Flush()
		}
		time.Sleep(150 * time.Millisecond) // 客户端此时已收到前几帧、正在流式转发
		if f, ok := probe.Load().(func(string)); ok && f != nil {
			f("streaming")
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":100000,\"candidatesTokenCount\":6,\"totalTokenCount\":100006}}\n\n")
		fl.Flush()
	}))
	defer srv.Close()
	account := &Account{ID: 9902, Platform: PlatformGemini, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "k", "base_url": "https://gemini.example.test"}}
	up := &realHTTPResidencyUpstream{client: &http.Client{}, target: strings.TrimPrefix(srv.URL, "http://")}
	svc := &GeminiMessagesCompatService{httpUpstream: up, cfg: &config.Config{},
		rateLimitService: NewRateLimitService(&errorPolicyRepoStub{}, nil, &config.Config{}, nil, nil)}
	cases := []struct {
		name string
		body []byte
		run  func(body []byte) error
	}{
		{"native/stream", buildGeminiNativeLargeBody(t, geminiLargeBodyTarget), func(body []byte) error {
			c, _ := newGeminiEmptyTestContext("/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse")
			_, err := svc.ForwardNative(context.Background(), c, account, "gemini-3.8-flash", "streamGenerateContent", true, body)
			return err
		}},
		{"chat_completions/stream", buildGeminiChatLargeBody(t, geminiLargeBodyTarget), func(body []byte) error {
			c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
			return err
		}},
		{"messages/stream", buildGeminiClaudeLargeBody(t, geminiLargeBodyTarget), func(body []byte) error {
			c, _ := newGeminiEmptyTestContext("/v1/messages")
			_, err := svc.Forward(context.Background(), c, account, body)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = tc.run(tc.body)
			time.Sleep(100 * time.Millisecond)
			base := geminiResidencyLiveHeap()
			var streaming int64
			probe.Store(func(string) { streaming = geminiResidencyLiveHeap() - base })
			if err := tc.run(tc.body); err != nil {
				t.Fatalf("forward: %v", err)
			}
			probe.Store(func(string) {})
			n := int64(len(tc.body))
			t.Logf("REAL %-22s body=%.2fMB  extra live@streaming=%.2fMB (%.2fx)", tc.name, mbf(n), mbf(streaming), float64(streaming)/float64(n))
			runtime.KeepAlive(tc.body)
		})
	}
}
