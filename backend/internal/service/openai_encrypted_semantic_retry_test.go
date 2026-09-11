package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression shape observed on HK43: a Responses SSE response.failed envelope
// carries invalid_encrypted_content although transport HTTP is 200. The same
// explicit error over HTTP 400 already retries once in the non-passthrough path.
// Test data is entirely synthetic; the reasoning summary and message must survive.
func TestOpenAIEncryptedSemanticFailureHasHTTPRetryParity(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, wire := range []string{"http400", "sse200", "bare_sse200", "sse400"} {
			t.Run(wire+map[bool]string{false: "/normal", true: "/passthrough"}[passthrough], func(t *testing.T) {
				first := &http.Response{StatusCode: http.StatusBadRequest,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body:   io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"Encrypted content could not be verified"}}`))}
				if wire != "http400" {
					first.StatusCode = http.StatusOK
					if wire == "sse400" {
						first.StatusCode = http.StatusBadRequest
					}
					first.Header.Set("Content-Type", "text/event-stream")
					first.Body = io.NopCloser(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"Encrypted content could not be verified\"}}}\n\n"))
					if wire == "bare_sse200" {
						first.Body = io.NopCloser(strings.NewReader("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"Encrypted content could not be verified\"}}\n\n"))
					}
				}
				upstream := &httpUpstreamRecorder{responses: []*http.Response{first, {
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
				}}}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				account := &Account{ID: 23, Name: "synthetic-gpt-account", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3,
					Credentials: map[string]any{"api_key": "synthetic-test-only", "base_url": "https://example.com"},
					Extra:       map[string]any{"use_responses_api": true, "openai_passthrough": passthrough}}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				body := []byte(`{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-ciphertext","summary":[{"type":"summary_text","text":"retain this summary"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic followup"}]}]}`)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 2)
				require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.encrypted_content").Exists())
				require.Equal(t, "retain this summary", gjson.GetBytes(upstream.bodies[1], "input.0.summary.0.text").String())
				require.Equal(t, "synthetic followup", gjson.GetBytes(upstream.bodies[1], "input.1.content.0.text").String())
				require.NotContains(t, recorder.Body.String(), "invalid_encrypted_content")
			})
		}
	}
}

func TestOpenAIEncryptedSummaryRepairPreservesOpaqueState(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":""}]}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":true}]}]}`,
		`{"previous_response_id":"resp_private","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"some summary"}]}]}`,
		`{"input":[{"type":"item_reference","id":"remote"},{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"some summary"}]}]}`,
		`{"input":[{"type":"message","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"some summary"}]}]}`,
	} {
		out, changed := openAIEncryptedReasoningSummaryRetryBody([]byte(body))
		require.False(t, changed)
		require.Equal(t, body, string(out))
	}
}

func TestOpenAIEncryptedSummaryRepairBudgetAndCodeIsolation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"model":"gpt-6-astra","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"keep"}]},{"type":"function_call","call_id":"call_test","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_test","output":"done"}],"nonce":9007199254740993}`)
	initializeOpenAIEncryptedSemanticRetry(c, account, body)
	require.Nil(t, newOpenAIEncryptedSemanticRetrySignal(c, []byte(`{"error":{"code":"thinking_signature_invalid"}}`)))
	require.Nil(t, newOpenAIEncryptedSemanticRetrySignal(c, []byte(`{"request":{"error":{"code":"invalid_encrypted_content"}}}`)))
	failure := newOpenAIEncryptedSemanticRetrySignal(c, []byte(`{"response":{"error":{"code":"invalid_encrypted_content"}}}`))
	require.NotNil(t, failure)
	out, changed := consumeOpenAIEncryptedSemanticRetry(c, body, failure, account)
	require.True(t, changed)
	require.Equal(t, gjson.GetBytes(body, "input.1").Raw, gjson.GetBytes(out, "input.1").Raw)
	require.Equal(t, gjson.GetBytes(body, "input.2").Raw, gjson.GetBytes(out, "input.2").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "nonce").Raw)
	initializeOpenAIEncryptedSemanticRetry(c, account, body)
	require.Nil(t, newOpenAIEncryptedSemanticRetrySignal(c, []byte(`{"error":{"code":"invalid_encrypted_content"}}`)))
	_, changed = consumeOpenAIEncryptedSemanticRetry(c, body, failure, account)
	require.False(t, changed)
}

func TestOpenAIEncryptedSemanticRetryCannotReplayCommittedOrOpaqueState(t *testing.T) {
	const failure = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"Encrypted content could not be verified\"}}}\n\n"
	const summaryBody = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"keep"}]},{"type":"message","role":"user","content":"synthetic followup"}]}`
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, prefix string
			wantAttempts       int
		}{
			{"repeated_rejection", summaryBody, "", 2},
			{"already_answered", summaryBody, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"already delivered\"}\n\n", 1},
			{"opaque_compaction", `{"model":"gpt-6-astra","stream":true,"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, "", 1},
		} {
			t.Run(tc.name+map[bool]string{false: "/normal", true: "/passthrough"}[passthrough], func(t *testing.T) {
				upstream := &httpUpstreamRecorder{responses: []*http.Response{
					{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.prefix + failure))},
					{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(failure))},
				}}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3,
					Credentials: map[string]any{"api_key": "synthetic-test-only", "base_url": "https://example.com"},
					Extra:       map[string]any{"use_responses_api": true, "openai_passthrough": passthrough}}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				_, err := svc.Forward(context.Background(), c, account, []byte(tc.body))
				require.Error(t, err)
				require.Len(t, upstream.bodies, tc.wantAttempts)
				if tc.prefix != "" {
					require.Contains(t, recorder.Body.String(), "already delivered")
				}
				require.Contains(t, recorder.Body.String(), "invalid_encrypted_content")
			})
		}
	}
}
