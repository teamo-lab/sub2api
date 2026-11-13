//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 1x1 PNG
var testPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

func allowLoopbackImageFetch(t *testing.T) {
	t.Helper()
	prev := imageURLInlineIPAllowed
	imageURLInlineIPAllowed = func(net.IP) bool { return true }
	t.Cleanup(func() { imageURLInlineIPAllowed = prev })
}

func newImageServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		switch r.URL.Path {
		case "/a.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG)
		case "/no-type":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(testPNG)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>nope</html>"))
		case "/redirect":
			http.Redirect(w, r, "/a.png", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testGinContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func TestInlineClaudeImageURLs_ReplacesURLSourcesIncludingToolResults(t *testing.T) {
	allowLoopbackImageFetch(t)
	var hits int32
	srv := newImageServer(t, &hits)

	body := []byte(`{"model":"gemini-4-argon","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"看图"},` +
		`{"type":"image","source":{"type":"url","url":"` + srv.URL + `/a.png?sig=secret"}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"image","source":{"type":"url","url":"` + srv.URL + `/no-type"}}]}]}]}`)

	out, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
	require.NoError(t, err)

	first := gjson.GetBytes(out, "messages.0.content.1.source")
	require.Equal(t, "base64", first.Get("type").String())
	require.Equal(t, "image/png", first.Get("media_type").String())
	require.Equal(t, base64.StdEncoding.EncodeToString(testPNG), first.Get("data").String())
	require.False(t, first.Get("url").Exists())

	untouched := gjson.GetBytes(out, "messages.0.content.2.source")
	require.Equal(t, "AAAA", untouched.Get("data").String())

	nested := gjson.GetBytes(out, "messages.2.content.0.content.0.source")
	require.Equal(t, "base64", nested.Get("type").String())
	require.Equal(t, "image/png", nested.Get("media_type").String(), "octet-stream should be sniffed as png")
	require.Equal(t, int32(2), atomic.LoadInt32(&hits))
}

func TestInlineClaudeImageURLs_CachesAcrossRetriesOnSameContext(t *testing.T) {
	allowLoopbackImageFetch(t)
	var hits int32
	srv := newImageServer(t, &hits)
	body := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","url":"` + srv.URL + `/a.png"}},` +
		`{"type":"image","source":{"type":"url","url":"` + srv.URL + `/a.png"}}]}]}`)
	c := testGinContext()

	_, err := inlineClaudeImageURLs(context.Background(), c, body)
	require.NoError(t, err)
	_, err = inlineClaudeImageURLs(context.Background(), c, body)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&hits), "duplicate URLs and failover retries should reuse the download")
}

func TestInlineClaudeImageURLs_FollowsRedirect(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + srv.URL + `/redirect"}}]}]}`)
	out, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
	require.NoError(t, err)
	require.Equal(t, "base64", gjson.GetBytes(out, "messages.0.content.0.source.type").String())
}

func TestInlineClaudeImageURLs_Errors(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	cases := map[string]string{
		"not found":  srv.URL + "/missing",
		"not image":  srv.URL + "/html",
		"bad scheme": "ftp://example.com/a.png",
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + u + `"}}]}]}`)
			_, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
			require.Error(t, err)
			require.True(t, IsImageURLInlineError(err))
		})
	}
}

func TestInlineClaudeImageURLs_BlocksPrivateDestinations(t *testing.T) {
	srv := newImageServer(t, nil) // 默认策略不允许回环地址
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + srv.URL + `/a.png"}}]}]}`)
	_, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not allowed")
}

func TestInlineClaudeImageURLs_RedactsQueryInError(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + srv.URL + `/missing?token=secret"}}]}]}`)
	_, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestInlineClaudeImageURLs_NoURLImagesReturnsSameBody(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"see https://example.com"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}],"tools":[{"name":"f","input_schema":{"properties":{"url":{"type":"string"}}}}]}`)
	out, err := inlineClaudeImageURLs(context.Background(), testGinContext(), body)
	require.NoError(t, err)
	require.Equal(t, string(body), string(out))
}

func TestIsPublicImageFetchIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.19.0.100", "192.168.1.1", "169.254.0.23", "100.64.1.1", "0.0.0.0", "::1", "fd00::1"} {
		require.Falsef(t, isPublicImageFetchIP(net.ParseIP(ip)), ip)
	}
	for _, ip := range []string{"8.8.8.8", "47.246.1.1", "2400:3200::1"} {
		require.Truef(t, isPublicImageFetchIP(net.ParseIP(ip)), ip)
	}
}

func TestConvertClaudeToGemini_RejectsUninlinedURLImage(t *testing.T) {
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`)
	_, err := convertClaudeMessagesToGeminiGenerateContent(body)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "inlined"))
}

func geminiImageCompatFixture(t *testing.T) (*GeminiMessagesCompatService, *geminiCompatHTTPUpstreamStub, *Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstreamBody := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"a geometry exercise"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1100,"candidatesTokenCount":3}}}` + "\n\n" + "data: [DONE]\n\n"
	stub := &geminiCompatHTTPUpstreamStub{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &GeminiMessagesCompatService{tokenProvider: &GeminiTokenProvider{}, httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{ID: 1118, Platform: PlatformGemini, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "ya29.test-token", "project_id": "project-1"}}
	return svc, stub, account
}

// 复现 11-13 的线上问题：OpenAI 格式带 http 图片 URL，经 Gemini 组转发，上游必须收到 inlineData。
func TestGeminiForwardAsChatCompletions_InlinesHTTPImageURL(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	svc, stub, account := geminiImageCompatFixture(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[{"type":"text","text":"描述这张图片"},{"type":"image_url","image_url":{"url":"` + srv.URL + `/a.png"}}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	sent, err := io.ReadAll(stub.lastReq.Body)
	require.NoError(t, err)
	inline := gjson.GetBytes(sent, "request.contents.0.parts.#(inlineData).inlineData")
	require.True(t, inline.Exists(), "upstream request must carry the image: %s", sent)
	require.Equal(t, "image/png", inline.Get("mimeType").String())
	require.Equal(t, base64.StdEncoding.EncodeToString(testPNG), inline.Get("data").String())
}

func TestGeminiForwardAsChatCompletions_UnreachableImageURLReturns400(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	svc, stub, account := geminiImageCompatFixture(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[{"type":"text","text":"描述这张图片"},{"type":"image_url","image_url":{"url":"` + srv.URL + `/missing"}}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "failed to fetch image url")
	require.Equal(t, 0, stub.calls, "must not call upstream without the image")
}
