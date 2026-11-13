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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
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

func TestConvertClaudeToGemini_URLImageBecomesFileData(t *testing.T) {
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","url":"https://img.alicdn.com/imgextra/i1/O1CN01_!!6000-0-tps-1024-406.jpg"}},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/a.png?x=1"}}]}]}`)
	out, err := convertClaudeMessagesToGeminiGenerateContent(body)
	require.NoError(t, err)
	first := gjson.GetBytes(out, "contents.0.parts.0.fileData")
	require.Equal(t, "https://img.alicdn.com/imgextra/i1/O1CN01_!!6000-0-tps-1024-406.jpg", first.Get("fileUri").String())
	require.Equal(t, "image/jpeg", first.Get("mimeType").String())
	require.Equal(t, "image/png", gjson.GetBytes(out, "contents.0.parts.1.fileData.mimeType").String())
}

func TestGuessImageMIMEFromURL(t *testing.T) {
	require.Equal(t, "image/png", guessImageMIMEFromURL("https://a.com/x.PNG?sig=1"))
	require.Equal(t, "image/webp", guessImageMIMEFromURL("https://a.com/x.webp"))
	require.Equal(t, "image/jpeg", guessImageMIMEFromURL("https://a.com/x"))
}

func TestIsGeminiImageURLFetchError(t *testing.T) {
	require.True(t, isGeminiImageURLFetchError([]byte(`{"error":{"message":"Cannot fetch content from the provided URL. Status: URL_ERROR-ERROR_OTHER"}}`)))
	require.True(t, isGeminiImageURLFetchError([]byte(`Status: URL_UNREACHABLE-UNREACHABLE_ROBOTS_ERROR`)))
	require.False(t, isGeminiImageURLFetchError([]byte(`{"error":{"message":"Invalid thought signature"}}`)))
}

func TestAccountRequiresInlineImageURLs(t *testing.T) {
	require.True(t, accountRequiresInlineImageURLs(&Account{Platform: PlatformAnthropic, Type: AccountTypeServiceAccount}))
	require.True(t, accountRequiresInlineImageURLs(&Account{Platform: PlatformAnthropic, Type: AccountTypeBedrock}))
	require.False(t, accountRequiresInlineImageURLs(&Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}))
	require.False(t, accountRequiresInlineImageURLs(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}))
	require.False(t, accountRequiresInlineImageURLs(nil))
}

const geminiImageOKSSE = `data: {"response":{"candidates":[{"content":{"parts":[{"text":"a geometry exercise"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1100,"candidatesTokenCount":3}}}` + "\n\n" + "data: [DONE]\n\n"

const geminiCannotFetchBody = `{"error":{"code":400,"message":"Cannot fetch content from the provided URL. Please ensure the URL is valid and accessible. Status: URL_ERROR-ERROR_OTHER","status":"INVALID_ARGUMENT"}}`

// geminiSeqUpstreamStub 按顺序返回预设响应，并记录每次请求体。
type geminiSeqUpstreamStub struct {
	responses []func() *http.Response
	bodies    [][]byte
}

func (s *geminiSeqUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	s.bodies = append(s.bodies, b)
	i := len(s.bodies) - 1
	if i >= len(s.responses) {
		i = len(s.responses) - 1
	}
	return s.responses[i](), nil
}

func (s *geminiSeqUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

func sseResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(geminiImageOKSSE))}
}

func cannotFetchResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(geminiCannotFetchBody))}
}

func geminiImageFixture(t *testing.T, responses ...func() *http.Response) (*GeminiMessagesCompatService, *geminiSeqUpstreamStub, *Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	stub := &geminiSeqUpstreamStub{responses: responses}
	svc := &GeminiMessagesCompatService{tokenProvider: &GeminiTokenProvider{}, httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{ID: 1118, Platform: PlatformGemini, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "ya29.test-token", "project_id": "project-1"}}
	return svc, stub, account
}

func chatImageRequest(t *testing.T, imageURL string) (*gin.Context, *httptest.ResponseRecorder, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[{"type":"text","text":"描述这张图片"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	return c, rec, body
}

// 复现 11-13 的线上问题：OpenAI 格式带 http 图片 URL 经 Gemini 组转发，上游要收到这张图；
// 默认把 URL 放进 fileData 交给上游去拉，我们不下载。
func TestGeminiForwardAsChatCompletions_PassesImageURLAsFileData(t *testing.T) {
	allowLoopbackImageFetch(t)
	var hits int32
	srv := newImageServer(t, &hits)
	svc, stub, account := geminiImageFixture(t, sseResponse)
	c, rec, body := chatImageRequest(t, srv.URL+"/a.png")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, stub.bodies, 1)
	fileData := gjson.GetBytes(stub.bodies[0], "request.contents.0.parts.#(fileData).fileData")
	require.Equal(t, srv.URL+"/a.png", fileData.Get("fileUri").String(), "upstream request must carry the image url: %s", stub.bodies[0])
	require.Equal(t, "image/png", fileData.Get("mimeType").String())
	require.Equal(t, int32(0), atomic.LoadInt32(&hits), "should not download when upstream can fetch")
}

// 上游拉不到链接（400 Cannot fetch content）时，下载成 base64 重试一次。
func TestGeminiForwardAsChatCompletions_FallsBackToInlineWhenUpstreamCannotFetch(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	svc, stub, account := geminiImageFixture(t, cannotFetchResponse, sseResponse)
	c, rec, body := chatImageRequest(t, srv.URL+"/a.png")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, stub.bodies, 2)
	require.True(t, gjson.GetBytes(stub.bodies[0], "request.contents.0.parts.#(fileData)").Exists())
	inline := gjson.GetBytes(stub.bodies[1], "request.contents.0.parts.#(inlineData).inlineData")
	require.Equal(t, "image/png", inline.Get("mimeType").String())
	require.Equal(t, base64.StdEncoding.EncodeToString(testPNG), inline.Get("data").String())
}

func TestGeminiForwardAsChatCompletions_FallbackDownloadFailureReturns400(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	svc, stub, account := geminiImageFixture(t, cannotFetchResponse, sseResponse)
	c, rec, body := chatImageRequest(t, srv.URL+"/missing")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "failed to fetch image url")
	require.Len(t, stub.bodies, 1, "must not retry upstream without the image")
}

func TestGeminiForwardMessages_FallsBackToInlineWhenUpstreamCannotFetch(t *testing.T) {
	allowLoopbackImageFetch(t)
	srv := newImageServer(t, nil)
	svc, stub, account := geminiImageFixture(t, cannotFetchResponse, sseResponse)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-4-argon","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"描述"},{"type":"image","source":{"type":"url","url":"` + srv.URL + `/a.png"}}]}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, stub.bodies, 2)
	require.Equal(t, srv.URL+"/a.png", gjson.GetBytes(stub.bodies[0], "request.contents.0.parts.#(fileData).fileData.fileUri").String())
	require.Equal(t, base64.StdEncoding.EncodeToString(testPNG), gjson.GetBytes(stub.bodies[1], "request.contents.0.parts.#(inlineData).inlineData.data").String())
}
