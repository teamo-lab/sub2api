//go:build unit

package handler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const nativeRelayAttemptAPrefix = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_attempt_A\",\"status\":\"in_progress\"}}\n\n" +
	"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_attempt_A\",\"delta\":\"PRIVATE_REASONING_A\"}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_attempt_A\",\"call_id\":\"call_attempt_A\",\"name\":\"PRIVATE_TOOL_A\",\"arguments\":\"\"}}\n\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_attempt_A\",\"delta\":\"{\\\"private_A\\\":\"}\n\n"

const nativeRelayAttemptBStream = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_attempt_B\",\"status\":\"in_progress\"}}\n\n" +
	"data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_attempt_B\",\"delta\":\"COMPLETE_REASONING_B\"}\n\n" +
	"data: {\"type\":\"response.reasoning_summary_text.done\",\"item_id\":\"rs_attempt_B\",\"text\":\"COMPLETE_REASONING_B\"}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_attempt_B\",\"delta\":\"COMPLETE_ANSWER_B\"}\n\n" +
	"data: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_attempt_B\",\"text\":\"COMPLETE_ANSWER_B\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_attempt_B\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_attempt_B\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"COMPLETE_REASONING_B\"}]},{\"type\":\"message\",\"id\":\"msg_attempt_B\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"COMPLETE_ANSWER_B\"}]}],\"usage\":{\"input_tokens\":100,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":80}}}}\n\n"

// The production HTTP request is sent over an actual local TCP connection. Only
// its destination is replaced; response parsing, account switching and usage
// recording are exercised by the real gateway service and Responses handler.
type nativeRelayHTTPFixtureUpstream struct {
	service.HTTPUpstream
	client     *http.Client
	serverURL  *url.URL
	prefixRead chan struct{}
	readErrors chan error
	mu         sync.Mutex
	accountIDs []int64
}

func (u *nativeRelayHTTPFixtureUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	local := req.Clone(req.Context())
	local.URL.Scheme, local.URL.Host = u.serverURL.Scheme, u.serverURL.Host
	local.URL.Path = fmt.Sprintf("/attempt/%d", accountID)
	local.Host = u.serverURL.Host
	resp, err := u.client.Do(local)
	if err == nil && accountID == 1 {
		resp.Body = &nativeRelayPrefixReadBody{ReadCloser: resp.Body, prefixRead: u.prefixRead, readErrors: u.readErrors}
	}
	return resp, err
}

func (u *nativeRelayHTTPFixtureUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

type nativeRelayPrefixReadBody struct {
	io.ReadCloser
	read       strings.Builder
	once       sync.Once
	prefixRead chan struct{}
	readErrors chan error
}

func (b *nativeRelayPrefixReadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	_, _ = b.read.Write(p[:n])
	if strings.Contains(b.read.String(), nativeRelayAttemptAPrefix) {
		b.once.Do(func() { close(b.prefixRead) })
	}
	if err != nil {
		select {
		case b.readErrors <- err:
		default:
		}
	}
	return n, err
}

func newNativeRelayRetryHandler(t *testing.T, upstream service.HTTPUpstream, usageRepo service.UsageLogRepository, keepaliveSeconds int) *OpenAIGatewayHandler {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.TeamoRelayEnabled = true
	cfg.Gateway.TeamoRelayGroupIDs = []int64{3131}
	cfg.Gateway.StreamKeepaliveInterval = keepaliveSeconds
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{
		{ID: 1, Name: "local-A", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Priority: 0, Credentials: map[string]any{"access_token": "local-fixture-A"}},
		{ID: 2, Name: "local-B", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: map[string]any{"access_token": "local-fixture-B"}},
	}}
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gw := service.NewOpenAIGatewayService(accounts, usageRepo, nil, nil, nil, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(nil), billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 2
	return h
}

// In particular, reset/short_chunk must fail during Body.Read, not while dialing
// or reading response headers. The fixture waits until Sub has read A's entire
// prefix before it terminates the upstream stream. The final client must still
// receive only B's complete response, with exactly one successful usage record.
func TestNativeRelayHTTPRetryAfterReasoningPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, ending := range []string{"tcp_reset", "short_chunk", "eof", "failed", "keepalive_reset", "keepalive_exhausted"} {
		t.Run(ending, func(t *testing.T) {
			withKeepalive := strings.HasPrefix(ending, "keepalive_")
			exhausted := ending == "keepalive_exhausted"
			heartbeatObserved := make(chan struct{})
			upstream := &nativeRelayHTTPFixtureUpstream{prefixRead: make(chan struct{}), readErrors: make(chan error, 4)}
			fixtureErrors := make(chan error, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/attempt/2" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("X-Request-ID", "req_attempt_B")
					if exhausted {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_attempt_B\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream temporarily unavailable\"}}}\n\n")
						return
					}
					_, _ = io.WriteString(w, nativeRelayAttemptBStream)
					return
				}
				if r.URL.Path != "/attempt/1" {
					http.Error(w, "unexpected account", http.StatusBadRequest)
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					fixtureErrors <- err
					return
				}
				defer conn.Close()
				_, _ = io.WriteString(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nX-Request-ID: req_attempt_A\r\n\r\n")
				writeChunk := func(data string) {
					_, _ = fmt.Fprintf(rw, "%x\r\n%s\r\n", len(data), data)
					_ = rw.Flush()
				}
				writeChunk(nativeRelayAttemptAPrefix)
				select {
				case <-upstream.prefixRead:
				case <-time.After(5 * time.Second):
					fixtureErrors <- fmt.Errorf("Sub did not read A's prefix before upstream termination")
					return
				}
				if withKeepalive {
					// The real client must receive the comment before A resets. This
					// exercises fallback after HTTP 200 has actually been committed.
					select {
					case <-heartbeatObserved:
					case <-time.After(5 * time.Second):
						fixtureErrors <- fmt.Errorf("client did not receive the 1s keepalive")
						return
					}
				}
				switch ending {
				case "tcp_reset", "keepalive_reset", "keepalive_exhausted":
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
				case "short_chunk":
					// Advertise a larger chunk, then terminate before it is complete.
					_, _ = io.WriteString(rw, "100\r\ndata: ")
					_ = rw.Flush()
				case "failed":
					writeChunk("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_attempt_A\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream temporarily unavailable\"}}}\n\n" +
						"data: {\"type\":\"response.output_text.delta\",\"delta\":\"PRIVATE_LATE_ANSWER_A\"}\n\n")
					fallthrough
				case "eof":
					_, _ = io.WriteString(rw, "0\r\n\r\n")
					_ = rw.Flush()
				}
			}))
			t.Cleanup(provider.Close)
			upstream.client = provider.Client()
			upstream.client.Timeout = 10 * time.Second
			upstream.serverURL, _ = url.Parse(provider.URL)
			usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
			keepaliveSeconds := 0
			if withKeepalive {
				keepaliveSeconds = 1
			}
			h := newNativeRelayRetryHandler(t, upstream, usageRepo, keepaliveSeconds)
			finished := make(chan struct{})
			router := gin.New()
			router.POST("/v1/responses", func(c *gin.Context) {
				defer close(finished)
				fixtureContext, _ := newOpenAIResponsesFailoverTestContext(t, context.Background())
				apiKey, _ := fixtureContext.Get(string(middleware2.ContextKeyAPIKey))
				user, _ := fixtureContext.Get(string(middleware2.ContextKeyUser))
				c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
				c.Set(string(middleware2.ContextKeyUser), user)
				h.Responses(c)
			})
			entry := httptest.NewServer(router)
			t.Cleanup(entry.Close)
			client := entry.Client()
			client.Timeout = 15 * time.Second
			resp, err := client.Post(entry.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.1","stream":true,"input":"local fixture"}`))
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var comment string
			if withKeepalive {
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
				line, readErr := reader.ReadString('\n')
				require.NoError(t, readErr)
				require.Equal(t, ":\n", line)
				blank, readErr := reader.ReadString('\n')
				require.NoError(t, readErr)
				require.Equal(t, "\n", blank)
				comment = line + blank
				close(heartbeatObserved)
			}
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			body = append([]byte(comment), body...)
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("Responses handler did not finish usage recording")
			}
			select {
			case err := <-fixtureErrors:
				require.NoError(t, err)
			default:
			}
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, []int64{1, 2}, upstream.calls(), "real handler must switch A to B exactly once")
			select {
			case <-upstream.prefixRead:
			default:
				t.Fatal("A prefix was not consumed by the actual upstream HTTP client")
			}
			if ending == "tcp_reset" || ending == "short_chunk" || withKeepalive {
				select {
				case readErr := <-upstream.readErrors:
					require.Error(t, readErr)
					require.NotEqual(t, io.EOF, readErr, "fixture must produce a transport body error")
				default:
					t.Fatal("expected a real HTTP response body read error")
				}
			}
			for _, privateA := range []string{"resp_attempt_A", "PRIVATE_REASONING_A", "PRIVATE_TOOL_A", "private_A", "PRIVATE_LATE_ANSWER_A", "fc_attempt_A"} {
				require.NotContains(t, string(body), privateA, "discarded A prefix must not reach final client")
			}
			eventCounts := map[string]int{}
			declaredEvent := ""
			scan := bufio.NewScanner(strings.NewReader(string(body)))
			for scan.Scan() {
				if scan.Text() == "" {
					declaredEvent = ""
					continue
				}
				if strings.HasPrefix(scan.Text(), "event: ") {
					declaredEvent = strings.TrimPrefix(scan.Text(), "event: ")
					continue
				}
				if strings.HasPrefix(scan.Text(), ":") {
					continue
				}
				if !strings.HasPrefix(scan.Text(), "data: ") {
					t.Fatalf("non-SSE data mixed into response: %q", scan.Text())
					continue
				}
				data := strings.TrimPrefix(scan.Text(), "data: ")
				require.True(t, gjson.Valid(data), "each SSE data frame must remain JSON")
				eventType := gjson.Get(data, "type").String()
				if eventType == "" {
					eventType = declaredEvent
				}
				eventCounts[eventType]++
				if eventType == "response.reasoning_summary_text.delta" {
					require.Equal(t, "COMPLETE_REASONING_B", gjson.Get(data, "delta").String())
				}
				if eventType == "response.output_text.delta" {
					require.Equal(t, "COMPLETE_ANSWER_B", gjson.Get(data, "delta").String())
				}
				if eventType == "response.completed" {
					require.Equal(t, "resp_attempt_B", gjson.Get(data, "response.id").String())
					require.EqualValues(t, 100, gjson.Get(data, "response.usage.input_tokens").Int())
					require.EqualValues(t, 7, gjson.Get(data, "response.usage.output_tokens").Int())
					require.EqualValues(t, 80, gjson.Get(data, "response.usage.input_tokens_details.cached_tokens").Int())
				}
			}
			require.NoError(t, scan.Err())
			if exhausted {
				require.Equal(t, 1, eventCounts["response.failed"]+eventCounts["error"], "exhaustion after keepalive needs one SSE failure terminal")
				for _, kind := range []string{"response.created", "response.reasoning_summary_text.delta", "response.output_text.delta", "response.completed"} {
					require.Zero(t, eventCounts[kind], "failed attempts must not leak model frames")
				}
				require.Empty(t, usageRepo.created, "neither failed attempt should create successful usage")
				return
			}
			for _, eventType := range []string{"response.created", "response.reasoning_summary_text.delta", "response.output_text.delta", "response.completed"} {
				require.Equal(t, 1, eventCounts[eventType], "each B event must appear exactly once: %s", eventType)
			}
			require.Zero(t, eventCounts["response.failed"])
			require.Zero(t, eventCounts["error"])
			require.Len(t, usageRepo.created, 1, "exactly one UsageLog.Create after successful failover")
			usage := <-usageRepo.created
			require.EqualValues(t, 2, usage.AccountID)
			require.EqualValues(t, 20, usage.InputTokens, "input log stores uncached tokens separately")
			require.EqualValues(t, 7, usage.OutputTokens)
			require.EqualValues(t, 80, usage.CacheReadTokens)
		})
	}
}
