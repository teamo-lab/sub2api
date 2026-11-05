//go:build unit

package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const testGeminiRequestSessionID = "6f1c2a64-6d1e-5b8a-9c3e-1f2a3b4c5d6e"

func newGeminiRequestSessionContext(method, path, body, sessionID string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	if sessionID != "" {
		c.Request.Header.Set(GeminiRequestSessionHeader, sessionID)
	}
	return c
}

func TestGeminiRequestSessionID_Sanitizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.Equal(t, "", GeminiRequestSessionID(nil))
	require.Equal(t, testGeminiRequestSessionID,
		GeminiRequestSessionID(newGeminiRequestSessionContext(http.MethodPost, "/", "", "  "+testGeminiRequestSessionID+"  ")))
	require.Equal(t, "", GeminiRequestSessionID(newGeminiRequestSessionContext(http.MethodPost, "/", "", "")))
	require.Equal(t, "", GeminiRequestSessionID(newGeminiRequestSessionContext(http.MethodPost, "/", "", "abc\x01def")))
	require.Equal(t, "", GeminiRequestSessionID(newGeminiRequestSessionContext(http.MethodPost, "/", "", strings.Repeat("a", 256))))
}

func TestGeminiRequestSessionHash_StableAndOpaque(t *testing.T) {
	require.Equal(t, "", GeminiRequestSessionHash(""))
	h := GeminiRequestSessionHash(testGeminiRequestSessionID)
	require.Equal(t, h, GeminiRequestSessionHash(testGeminiRequestSessionID))
	require.True(t, strings.HasPrefix(h, geminiRequestSessionHashPrefix))
	require.Len(t, h, len(geminiRequestSessionHashPrefix)+32)
	require.NotContains(t, h, testGeminiRequestSessionID)
	require.NotEqual(t, h, GeminiRequestSessionHash(testGeminiRequestSessionID+"x"))
}

func TestGeminiForwardNative_APIKeyForwardsRequestSessionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, httpStub := newGeminiSkippedWriteService(http.StatusBadRequest, geminiSkippedTestUpstreamBody())
	c := newGeminiRequestSessionContext(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", "{}", testGeminiRequestSessionID)

	_, _ = svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(),
		"gemini-4-argon", "generateContent", false, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))

	require.NotNil(t, httpStub.lastReq)
	require.Equal(t, testGeminiRequestSessionID, httpStub.lastReq.Header.Get(GeminiRequestSessionHeader))
	require.Equal(t, "test-key", httpStub.lastReq.Header.Get("x-goog-api-key"))
}

func TestGeminiForwardNative_APIKeyWithoutSessionHeaderSendsNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, httpStub := newGeminiSkippedWriteService(http.StatusBadRequest, geminiSkippedTestUpstreamBody())
	c := newGeminiRequestSessionContext(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", "{}", "")

	_, _ = svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(),
		"gemini-4-argon", "generateContent", false, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))

	require.NotNil(t, httpStub.lastReq)
	_, present := httpStub.lastReq.Header[http.CanonicalHeaderKey(GeminiRequestSessionHeader)]
	require.False(t, present)
}

func TestGeminiForwardAsChatCompletions_APIKeyForwardsRequestSessionHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, httpStub := newGeminiSkippedWriteService(http.StatusBadRequest, geminiSkippedTestUpstreamBody())
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set(GeminiRequestSessionHeader, testGeminiRequestSessionID)

	_, _ = svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)

	require.NotNil(t, httpStub.lastReq)
	require.Contains(t, httpStub.lastReq.URL.String(), "/v1beta/models/gemini-4-argon:generateContent")
	require.Equal(t, testGeminiRequestSessionID, httpStub.lastReq.Header.Get(GeminiRequestSessionHeader))
}

func TestGeminiForwardAsChatCompletions_InvalidSessionHeaderNotForwarded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, httpStub := newGeminiSkippedWriteService(http.StatusBadRequest, geminiSkippedTestUpstreamBody())
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set(GeminiRequestSessionHeader, strings.Repeat("a", 300))

	_, _ = svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)

	require.NotNil(t, httpStub.lastReq)
	require.Empty(t, httpStub.lastReq.Header.Get(GeminiRequestSessionHeader))
}
