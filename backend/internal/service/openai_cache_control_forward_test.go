package service

import (
	"bytes"
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

func TestAPIKeyForwardPreservesExplicitCacheControls(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"prompt_cache_key":"stable-key","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","input":[{"role":"user","content":[{"type":"input_text","text":"stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}},{"type":"input_text","text":"dynamic suffix"}]}]}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse)}}
	c := newOpenAIRejectedFieldTestContext(body)
	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIRejectedFieldTestAccount(), body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	sent := upstream.bodies[0]
	require.Equal(t, "explicit", gjson.GetBytes(sent, "prompt_cache_options.mode").String())
	require.Equal(t, "30m", gjson.GetBytes(sent, "prompt_cache_options.ttl").String())
	require.Equal(t, "24h", gjson.GetBytes(sent, "prompt_cache_retention").String())
	require.Equal(t, "stable-key", gjson.GetBytes(sent, "prompt_cache_key").String())
	require.Equal(t, "explicit", gjson.GetBytes(sent, "input.0.content.0.prompt_cache_breakpoint.mode").String())
}

func TestAPIKeyChatForwardPreservesCacheControls(t *testing.T) {
	for _, payload := range []string{
		`"messages":[{"role":"user","content":[{"type":"text","text":"prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]}]`,
		`"input":[{"role":"user","content":[{"type":"input_text","text":"prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]}]`,
	} {
		t.Run(payload[:8], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			body := []byte(`{"model":"gpt-5.6-sol","stream":false,"prompt_cache_key":"client-key","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h",` + payload + `}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			c.Set("api_key", &APIKey{ID: 99})
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test stop after forwarding"}}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test-key"}, Extra: map[string]any{"openai_responses_supported": true}}
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "stable-key", "gpt-5.6-sol")
			require.Error(t, err)
			require.Equal(t, "explicit", gjson.GetBytes(upstream.lastBody, "prompt_cache_options.mode").String())
			require.Equal(t, "24h", gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").String())
			require.Equal(t, "client-key", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
			require.Equal(t, "explicit", gjson.GetBytes(upstream.lastBody, "input.0.content.0.prompt_cache_breakpoint.mode").String())
		})
	}
}
