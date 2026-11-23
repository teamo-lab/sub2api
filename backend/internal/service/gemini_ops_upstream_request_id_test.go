//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 11-23：HK 的 NewAPI 类 Gemini 上游把请求号放在 X-Oneapi-Request-Id（账号 extra.upstream_request_id_header 配的头），
// 成功请求按这个头记进了 usage_logs，失败的每一跳却只读 x-request-id / x-goog-request-id，429 / 空回复一跳都没号。

func TestGeminiOpsUpstreamRequestID_PrefersAccountHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-Oneapi-Request-Id", "oneapi-1")
	h.Set("X-Request-Id", "local-1")
	h.Set("x-goog-request-id", "goog-1")

	require.Equal(t, "oneapi-1", geminiOpsUpstreamRequestID(geminiAccountWithReqIDHeader(), h, "x-request-id"))
	// 没配头：保持原来的行为，先默认头再 x-goog-request-id
	require.Equal(t, "local-1", geminiOpsUpstreamRequestID(geminiPoolModeAPIKeyAccount(), h, "x-request-id"))
	h.Del("X-Request-Id")
	require.Equal(t, "goog-1", geminiOpsUpstreamRequestID(geminiPoolModeAPIKeyAccount(), h, "x-request-id"))
	// 配了头但这次响应没带：照样退回
	h.Del("X-Oneapi-Request-Id")
	require.Equal(t, "goog-1", geminiOpsUpstreamRequestID(geminiAccountWithReqIDHeader(), h, "x-request-id"))
	require.Equal(t, "", geminiOpsUpstreamRequestID(geminiAccountWithReqIDHeader(), nil, "x-request-id"))
}

func geminiAccountWithReqIDHeader() *Account {
	acc := geminiPoolModeAPIKeyAccount()
	acc.Extra = map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Oneapi-Request-Id"}
	return acc
}

func newGeminiReqIDTestService(status int, body string) *GeminiMessagesCompatService {
	svc, stub := newGeminiSkippedWriteService(status, body)
	stub.response.Header.Set("X-Oneapi-Request-Id", "202611230445478552637858268d9d6Y")
	stub.response.Header.Set("X-Request-Id", "local-proxy-id")
	return svc
}

func requireOpsUpstreamRequestID(t *testing.T, c *gin.Context, want string) {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	for _, ev := range events {
		require.Equal(t, want, ev.UpstreamRequestID, "kind=%s reason=%s", ev.Kind, ev.Reason)
	}
}

func TestGeminiForwardAsChatCompletions_EmptyReplyHopRecordsAccountRequestID(t *testing.T) {
	svc := newGeminiReqIDTestService(http.StatusOK, `{"candidates":[],"usageMetadata":{"promptTokenCount":0}}`)
	c, _ := newGeminiEmptyTestContext("/v1/chat/completions")
	body := []byte(strings.Replace(geminiChatTestBody, "%s", "false", 1))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiAccountWithReqIDHeader(), body)
	require.Error(t, err)
	requireOpsUpstreamRequestID(t, c, "202611230445478552637858268d9d6Y")
}

func TestGeminiForwardMessages_RateLimitHopRecordsAccountRequestID(t *testing.T) {
	svc := newGeminiReqIDTestService(http.StatusTooManyRequests, `{"error":{"code":429,"message":"Resource exhausted. Please try again later."}}`)
	c, _ := newGeminiEmptyTestContext("/v1/messages")
	body := []byte(`{"model":"gemini-3.8-flash","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}`)

	_, err := svc.Forward(context.Background(), c, geminiAccountWithReqIDHeader(), body)
	require.Error(t, err)
	requireOpsUpstreamRequestID(t, c, "202611230445478552637858268d9d6Y")
}

func TestGeminiForwardNative_FailoverHopRecordsAccountRequestID(t *testing.T) {
	svc := newGeminiReqIDTestService(http.StatusPaymentRequired, `{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`)
	c, _ := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, geminiAccountWithReqIDHeader(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.Error(t, err)
	requireOpsUpstreamRequestID(t, c, "202611230445478552637858268d9d6Y")
}

func TestGeminiForwardNative_UnconfiguredAccountKeepsOldRequestID(t *testing.T) {
	svc := newGeminiReqIDTestService(http.StatusPaymentRequired, `{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`)
	c, _ := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
	require.Error(t, err)
	requireOpsUpstreamRequestID(t, c, "local-proxy-id")
}
