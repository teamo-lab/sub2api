package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const encryptedRetryBudgetRequest = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"keep"}]},{"type":"message","role":"user","content":"synthetic followup"}]}`
const encryptedRetryBudgetFailure = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"Encrypted content could not be verified\"}}}\n\n"

type encryptedBudgetUpstream struct {
	do func(*http.Request) (*http.Response, error)
}

func TestOpenAIEncryptedRetryOwnBudgetBoundsSecondAttempt(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			calls, canceled := 0, false
			upstream := &encryptedBudgetUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
				}
				select {
				case <-r.Context().Done():
					canceled = true
					return nil, r.Context().Err()
				case <-time.After(200 * time.Millisecond):
					return nil, errors.New("test watchdog: unbounded encrypted repair")
				}
			}}
			parent := context.Background()
			svc, c, recorder, account := encryptedBudgetFixture(t, passthrough, parent, upstream)
			initializeOpenAIEncryptedSemanticRetry(c, account, []byte(encryptedRetryBudgetRequest))
			// Inject a shorter instance of the same timer; production uses 30s.
			openAIEncryptedSemanticRetryStateFor(c).budget = newOpenAIEncryptedRetryBudget(c, parent, 20*time.Millisecond)
			_, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
			require.Error(t, err)
			require.True(t, canceled)
			require.Equal(t, 2, calls)
			require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
			require.Contains(t, recorder.Body.String(), "encrypted_reasoning_recovery_timeout")
		})
	}
}

type encryptedOutputRecorder struct {
	*httptest.ResponseRecorder
	reached chan struct{}
	once    sync.Once
}

func (r *encryptedOutputRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "delivered answer") {
		r.once.Do(func() { close(r.reached) })
	}
	return n, err
}

func TestOpenAIEncryptedRetryDisarmsAfterSemanticOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder := &encryptedOutputRecorder{ResponseRecorder: httptest.NewRecorder(), reached: make(chan struct{})}
			outcome := make(chan error, 1)
			calls := 0
			upstream := &encryptedBudgetUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
				}
				reader, writer := io.Pipe()
				go func() {
					defer writer.Close()
					_, _ = io.WriteString(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"delivered answer\"}\n\n")
					select {
					case <-recorder.reached:
					case <-time.After(300 * time.Millisecond):
						outcome <- errors.New("semantic output was not delivered")
						return
					}
					// After output, both client cancellation and the short test budget
					// must leave the successful stream available for normal draining.
					cancel()
					time.Sleep(60 * time.Millisecond)
					if err := r.Context().Err(); err != nil {
						outcome <- err
						_ = writer.CloseWithError(err)
						return
					}
					_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
					outcome <- nil
				}()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
			}}
			svc, c, _, account := encryptedBudgetFixture(t, passthrough, parent, upstream)
			c, _ = gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(parent)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			initializeOpenAIEncryptedSemanticRetry(c, account, []byte(encryptedRetryBudgetRequest))
			openAIEncryptedSemanticRetryStateFor(c).budget = newOpenAIEncryptedRetryBudget(c, parent, 30*time.Millisecond)
			result, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
			require.NoError(t, err)
			require.NoError(t, <-outcome)
			require.NotNil(t, result)
			require.Equal(t, 2, calls)
			require.Contains(t, recorder.Body.String(), "response.completed")
		})
	}
}

func TestOpenAIEncryptedRetryDoesNotReplaceExistingRecoveryBudget(t *testing.T) {
	c, _, account, failure := recoveryFixture(t, "apikey", 0, 1)
	require.Equal(t, ErrorRecoverySwitch, ApplyErrorRecovery(c, account, "gpt", failure))
	outer := recoveryState(c)
	originalDeadline := outer.deadline
	budget := newOpenAIEncryptedRetryBudget(c, c.Request.Context(), openAIEncryptedSemanticRetryBudget)
	defer budget.cancel()
	defer budget.timer.Stop()
	defer budget.stopParent()
	require.Same(t, outer, recoveryState(c))
	require.Equal(t, originalDeadline, outer.deadline)
	require.Equal(t, originalDeadline, budget.deadline)
	uncapped := newOpenAIEncryptedRetryBudget(nil, context.Background(), openAIEncryptedSemanticRetryBudget)
	defer uncapped.cancel()
	defer uncapped.timer.Stop()
	defer uncapped.stopParent()
	require.InDelta(t, 30, time.Until(uncapped.deadline).Seconds(), 0.2)
}

func TestOpenAIEncryptedRetryMetadataTimeoutWritesOneTerminal(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, prefix := range []string{": keepalive\n\n", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_metadata\",\"status\":\"in_progress\"}}\n\n"} {
			t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough]+"/"+strings.Split(prefix, "\n")[0], func(t *testing.T) {
				parent := context.Background()
				var requestContext *gin.Context
				canceled := make(chan bool, 1)
				calls := 0
				upstream := &encryptedBudgetUpstream{do: func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
					}
					if strings.HasPrefix(prefix, ":") {
						// Model the gateway's own already-flushed SSE comment, which
						// commits transport headers but is not semantic model output.
						requestContext.Header("Content-Type", "text/event-stream")
						_, _ = requestContext.Writer.WriteString(": gateway keepalive\n\n")
						requestContext.Writer.Flush()
					}
					reader, writer := io.Pipe()
					go func() {
						_, _ = io.WriteString(writer, prefix)
						select {
						case <-r.Context().Done():
							canceled <- true
							_ = writer.CloseWithError(r.Context().Err())
						case <-time.After(200 * time.Millisecond):
							canceled <- false
							_ = writer.CloseWithError(errors.New("test watchdog: metadata disarmed budget"))
						}
					}()
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
				}}
				svc, c, recorder, account := encryptedBudgetFixture(t, passthrough, parent, upstream)
				requestContext = c
				initializeOpenAIEncryptedSemanticRetry(c, account, []byte(encryptedRetryBudgetRequest))
				openAIEncryptedSemanticRetryStateFor(c).budget = newOpenAIEncryptedRetryBudget(c, parent, 20*time.Millisecond)
				_, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
				require.Error(t, err)
				require.True(t, <-canceled)
				require.Equal(t, 2, calls)
				body := recorder.Body.String()
				if strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
					require.True(t, json.Valid([]byte(body)), body)
					require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
					require.Equal(t, 1, strings.Count(body, `"encrypted_reasoning_recovery_timeout"`))
				} else {
					require.Contains(t, recorder.Header().Get("Content-Type"), "text/event-stream")
					require.Equal(t, 1, strings.Count(body, "event: response.failed\n")+strings.Count(body, "event: error\n"), body)
				}
			})
		}
	}
}

func TestOpenAIEncryptedRetryBudgetDoesNotAppendToCommittedHTTPError(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			calls := 0
			upstream := &encryptedBudgetUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
				}
				// Simulate a final response racing the recovery deadline. The usual
				// HTTP handler may have committed it before the Forward defer runs.
				<-r.Context().Done()
				return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_request_error","message":"final request rejection"}}`))}, nil
			}}
			parent := context.Background()
			svc, c, recorder, account := encryptedBudgetFixture(t, passthrough, parent, upstream)
			initializeOpenAIEncryptedSemanticRetry(c, account, []byte(encryptedRetryBudgetRequest))
			openAIEncryptedSemanticRetryStateFor(c).budget = newOpenAIEncryptedRetryBudget(c, parent, 20*time.Millisecond)
			_, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
			require.Error(t, err)
			require.Equal(t, 2, calls)
			require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
			require.True(t, json.Valid(recorder.Body.Bytes()), recorder.Body.String())
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "encrypted_reasoning_recovery_timeout")
		})
	}
}

func (u *encryptedBudgetUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.do(r)
}
func (u *encryptedBudgetUpstream) DoWithTLS(r *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.do(r)
}

func encryptedBudgetFixture(t *testing.T, passthrough bool, parent context.Context, upstream HTTPUpstream) (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder, *Account) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3,
		Credentials: map[string]any{"api_key": "synthetic-test-only", "base_url": "https://example.com"},
		Extra:       map[string]any{"use_responses_api": true, "openai_passthrough": passthrough}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(parent)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return svc, c, recorder, account
}

func TestOpenAIEncryptedRetryHonorsCancellationWhileWaitingForHeaders(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, cancelMode := range []string{"deadline", "client_cancel"} {
			t.Run(cancelMode+map[bool]string{false: "/normal", true: "/passthrough"}[passthrough], func(t *testing.T) {
				var parent context.Context
				var cancel context.CancelFunc
				if cancelMode == "deadline" {
					parent, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
				} else {
					parent, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				calls, observedCancellation := 0, false
				upstream := &encryptedBudgetUpstream{do: func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(encryptedRetryBudgetFailure))}, nil
					}
					if cancelMode == "client_cancel" {
						time.AfterFunc(10*time.Millisecond, cancel)
					}
					select {
					case <-r.Context().Done():
						observedCancellation = true
						return nil, r.Context().Err()
					case <-time.After(200 * time.Millisecond):
						return nil, errors.New("test watchdog: retry lost pre-output cancellation")
					}
				}}
				svc, c, _, account := encryptedBudgetFixture(t, passthrough, parent, upstream)
				_, err := svc.Forward(parent, c, account, []byte(encryptedRetryBudgetRequest))
				require.Error(t, err)
				require.Equal(t, 2, calls)
				require.True(t, observedCancellation, "the retry must keep the stricter deadline and pre-output client cancellation")
			})
		}
	}
}
