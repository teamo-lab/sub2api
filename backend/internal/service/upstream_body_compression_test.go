//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func compressionTestBody(minLen int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","input":[`)
	for i := 0; b.Len() < minLen; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"role":"user","content":"line %d 你好，这是一段会重复出现的上下文"}`, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func compressionTestAccount(id int64, encoding string) *Account {
	extra := map[string]any{}
	if encoding != "" {
		extra[AccountExtraRequestBodyCompression] = encoding
	}
	return &Account{ID: id, Name: fmt.Sprintf("acct-%d", id), Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Extra: extra}
}

func compressionTestRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.example/v1/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Custom", "keep-me")
	return req
}

func compressionTestSettings() upstreamBodyCompressionSettings {
	return resolveUpstreamBodyCompressionSettings(nil)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type capturedSend struct {
	reqs      []*http.Request
	bodies    [][]byte
	responses []*http.Response
}

func (c *capturedSend) send(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	c.reqs = append(c.reqs, req)
	c.bodies = append(c.bodies, b)
	if len(c.responses) == 0 {
		return jsonResponse(http.StatusOK, `{"ok":true}`), nil
	}
	resp := c.responses[0]
	c.responses = c.responses[1:]
	return resp, nil
}

func decodeBody(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		defer dec.Close()
		out, err := io.ReadAll(dec)
		require.NoError(t, err)
		return out
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		require.NoError(t, err)
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		return out
	}
	t.Fatalf("unexpected encoding %q", encoding)
	return nil
}

func TestUpstreamBodyCompression_OffAccountIsByteForByteUnchanged(t *testing.T) {
	body := compressionTestBody(64 << 10)
	for _, account := range []*Account{
		compressionTestAccount(1, ""),
		compressionTestAccount(2, "brotli"), // 不认识的值等于关
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{AccountExtraRequestBodyCompression: "zstd"}},
	} {
		req := compressionTestRequest(t, body)
		wantHeader := req.Header.Clone()
		cap := &capturedSend{}
		resp, err := sendUpstreamWithBodyCompressionUsing(req, account, compressionTestSettings(), newUpstreamBodyCompressionBlocklist(), cap.send)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, cap.reqs, 1)
		require.Same(t, req, cap.reqs[0], "account %d: request must be passed through untouched", account.ID)
		require.Equal(t, body, cap.bodies[0])
		require.Equal(t, int64(len(body)), cap.reqs[0].ContentLength)
		require.Equal(t, wantHeader, cap.reqs[0].Header)
	}
}

func TestUpstreamBodyCompression_BelowMinSizeIsUnchanged(t *testing.T) {
	settings := compressionTestSettings()
	body := compressionTestBody(1024)
	require.Less(t, int64(len(body)), settings.minBytes)
	req := compressionTestRequest(t, body)
	cap := &capturedSend{}
	_, err := sendUpstreamWithBodyCompressionUsing(req, compressionTestAccount(1, "zstd"), settings, newUpstreamBodyCompressionBlocklist(), cap.send)
	require.NoError(t, err)
	require.Same(t, req, cap.reqs[0])
	require.Equal(t, body, cap.bodies[0])
	require.Empty(t, cap.reqs[0].Header.Get("Content-Encoding"))

	// 恰好等于阈值时压缩。
	settings.minBytes = int64(len(body))
	req = compressionTestRequest(t, body)
	cap = &capturedSend{}
	_, err = sendUpstreamWithBodyCompressionUsing(req, compressionTestAccount(1, "zstd"), settings, newUpstreamBodyCompressionBlocklist(), cap.send)
	require.NoError(t, err)
	require.Equal(t, "zstd", cap.reqs[0].Header.Get("Content-Encoding"))
}

func TestUpstreamBodyCompression_SkipsNonJSONAndAlreadyEncodedAndUnreplayableBodies(t *testing.T) {
	body := compressionTestBody(64 << 10)
	account := compressionTestAccount(1, "zstd")

	multipart := compressionTestRequest(t, body)
	multipart.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	encoded := compressionTestRequest(t, body)
	encoded.Header.Set("Content-Encoding", "gzip")

	unreplayable := compressionTestRequest(t, body)
	unreplayable.GetBody = nil

	for name, req := range map[string]*http.Request{"multipart": multipart, "encoded": encoded, "unreplayable": unreplayable} {
		cap := &capturedSend{}
		_, err := sendUpstreamWithBodyCompressionUsing(req, account, compressionTestSettings(), newUpstreamBodyCompressionBlocklist(), cap.send)
		require.NoError(t, err, name)
		require.Same(t, req, cap.reqs[0], name)
		require.Equal(t, body, cap.bodies[0], name)
	}
}

func TestUpstreamBodyCompression_HeadersAndRoundTrip(t *testing.T) {
	body := compressionTestBody(200 << 10)
	for _, encoding := range []string{"zstd", "gzip", " ZSTD "} {
		want := strings.ToLower(strings.TrimSpace(encoding))
		req := compressionTestRequest(t, body)
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
		cap := &capturedSend{}
		_, err := sendUpstreamWithBodyCompressionUsing(req, compressionTestAccount(1, encoding), compressionTestSettings(), newUpstreamBodyCompressionBlocklist(), cap.send)
		require.NoError(t, err)
		require.Len(t, cap.reqs, 1)
		sent := cap.reqs[0]
		require.NotSame(t, req, sent)
		require.Equal(t, want, sent.Header.Get("Content-Encoding"))
		require.Equal(t, int64(len(cap.bodies[0])), sent.ContentLength)
		require.Equal(t, strconv.Itoa(len(cap.bodies[0])), sent.Header.Get("Content-Length"))
		require.Less(t, len(cap.bodies[0])*3, len(body), "%s should compress repetitive JSON at least 3x", want)
		require.Equal(t, body, decodeBody(t, want, cap.bodies[0]))

		// 其它头、方法、URL 不变；原请求不被改动。
		for _, h := range []string{"Content-Type", "Authorization", "X-Custom"} {
			require.Equal(t, req.Header.Get(h), sent.Header.Get(h), h)
		}
		require.Equal(t, req.Method, sent.Method)
		require.Equal(t, req.URL.String(), sent.URL.String())
		require.Empty(t, req.Header.Get("Content-Encoding"))
		require.Equal(t, int64(len(body)), req.ContentLength)

		// GetBody 重放出来的也是压缩体。
		replay, err := sent.GetBody()
		require.NoError(t, err)
		replayed, _ := io.ReadAll(replay)
		require.Equal(t, cap.bodies[0], replayed)
	}
}

func TestUpstreamBodyCompression_ZstdFrameCarriesContentSize(t *testing.T) {
	body := compressionTestBody(64 << 10)
	var buf bytes.Buffer
	n, err := compressUpstreamBody(&buf, bytes.NewReader(body), "zstd", compressionTestSettings(), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), n)
	var hdr zstd.Header
	require.NoError(t, hdr.Decode(buf.Bytes()))
	require.True(t, hdr.HasFCS)
	require.Equal(t, uint64(len(body)), hdr.FrameContentSize)
}

func TestUpstreamBodyCompression_OverRealHTTP(t *testing.T) {
	body := compressionTestBody(300 << 10)
	var gotEncoding string
	var gotLength int64
	var gotWire int
	var gotDecoded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		gotEncoding = r.Header.Get("Content-Encoding")
		gotLength = r.ContentLength
		gotWire = len(wire)
		gotDecoded = decodeBody(t, gotEncoding, wire)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := sendUpstreamWithBodyCompressionUsing(req, compressionTestAccount(1, "zstd"), compressionTestSettings(), newUpstreamBodyCompressionBlocklist(), http.DefaultClient.Do)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "zstd", gotEncoding)
	require.Equal(t, int64(gotWire), gotLength)
	require.Less(t, gotWire, len(body))
	require.Equal(t, body, gotDecoded)
}

func TestUpstreamBodyCompression_FallbackOnParseRejection(t *testing.T) {
	cases := map[string]*http.Response{
		"new-api invalid json":    jsonResponse(http.StatusBadRequest, `{"error":{"message":"invalid JSON request body","type":"new_api_error"}}`),
		"gateway could not parse": jsonResponse(http.StatusBadRequest, `{"error":{"code":"InvalidParameter","message":"could not parse the JSON body of your request"}}`),
		"empty 400":               jsonResponse(http.StatusBadRequest, ``),
		"html 400":                jsonResponse(http.StatusBadRequest, `<html><body>400 Bad Request</body></html>`),
		"415":                     jsonResponse(http.StatusUnsupportedMediaType, `{"error":{"message":"whatever"}}`),
	}
	body := compressionTestBody(64 << 10)
	for name, rejection := range cases {
		blocklist := newUpstreamBodyCompressionBlocklist()
		account := compressionTestAccount(7, "zstd")
		cap := &capturedSend{responses: []*http.Response{rejection, jsonResponse(http.StatusOK, `{"ok":true}`)}}
		req := compressionTestRequest(t, body)
		resp, err := sendUpstreamWithBodyCompressionUsing(req, account, compressionTestSettings(), blocklist, cap.send)
		require.NoError(t, err, name)
		require.Equal(t, http.StatusOK, resp.StatusCode, name)
		require.Len(t, cap.reqs, 2, name)
		require.Equal(t, "zstd", cap.reqs[0].Header.Get("Content-Encoding"), name)
		require.Same(t, req, cap.reqs[1], name)
		require.Empty(t, cap.reqs[1].Header.Get("Content-Encoding"), name)
		require.Equal(t, body, cap.bodies[1], name)
		require.True(t, blocklist.blocked(account.ID), name)

		// 记住之后，同一个号的下一跳直接不压缩。
		cap = &capturedSend{}
		req = compressionTestRequest(t, body)
		_, err = sendUpstreamWithBodyCompressionUsing(req, account, compressionTestSettings(), blocklist, cap.send)
		require.NoError(t, err, name)
		require.Len(t, cap.reqs, 1, name)
		require.Same(t, req, cap.reqs[0], name)
		// 别的号不受影响。
		cap = &capturedSend{}
		_, err = sendUpstreamWithBodyCompressionUsing(compressionTestRequest(t, body), compressionTestAccount(8, "zstd"), compressionTestSettings(), blocklist, cap.send)
		require.NoError(t, err, name)
		require.Equal(t, "zstd", cap.reqs[0].Header.Get("Content-Encoding"), name)
	}
}

func TestUpstreamBodyCompression_NoBlocklistWhenUncompressedAlsoRejected(t *testing.T) {
	body := compressionTestBody(64 << 10)
	blocklist := newUpstreamBodyCompressionBlocklist()
	cap := &capturedSend{responses: []*http.Response{
		jsonResponse(http.StatusBadRequest, `{"error":{"message":"invalid JSON request body"}}`),
		jsonResponse(http.StatusBadRequest, `{"error":{"message":"invalid JSON request body"}}`),
	}}
	resp, err := sendUpstreamWithBodyCompressionUsing(compressionTestRequest(t, body), compressionTestAccount(9, "zstd"), compressionTestSettings(), blocklist, cap.send)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Len(t, cap.reqs, 2)
	require.False(t, blocklist.blocked(9))
}

func TestUpstreamBodyCompression_BusinessErrorIsNotRetriedAndBodyIsIntact(t *testing.T) {
	body := compressionTestBody(64 << 10)
	errBody := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`
	// 错误体超过窥视上限时也要完整还原。
	bigErrBody := `{"error":{"message":"context_length_exceeded","detail":"` + strings.Repeat("x", upstreamBodyRejectionPeekBytes+100) + `"}}`
	for _, eb := range []string{errBody, bigErrBody} {
		blocklist := newUpstreamBodyCompressionBlocklist()
		cap := &capturedSend{responses: []*http.Response{jsonResponse(http.StatusBadRequest, eb)}}
		resp, err := sendUpstreamWithBodyCompressionUsing(compressionTestRequest(t, body), compressionTestAccount(10, "zstd"), compressionTestSettings(), blocklist, cap.send)
		require.NoError(t, err)
		require.Len(t, cap.reqs, 1)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, eb, string(got))
		require.False(t, blocklist.blocked(10))
	}
}

func TestUpstreamBodyCompression_TransportErrorIsNotRetried(t *testing.T) {
	body := compressionTestBody(64 << 10)
	calls := 0
	_, err := sendUpstreamWithBodyCompressionUsing(compressionTestRequest(t, body), compressionTestAccount(11, "zstd"), compressionTestSettings(), newUpstreamBodyCompressionBlocklist(), func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 1, calls)
}

func TestUpstreamBodyCompression_ParseRejectionClassifier(t *testing.T) {
	yes := []struct {
		status int
		body   string
	}{
		{400, `{"error":{"message":"invalid JSON request body"}}`},
		{400, `{"error":{"message":"invalid character '(' looking for beginning of value"}}`},
		{400, `{"error":{"message":"Could not parse the JSON body of your request"}}`},
		{400, `{"error":"unsupported Content-Encoding: zstd"}`},
		{400, ``},
		{400, `Bad Request`},
		{415, `{"error":{"message":"anything"}}`},
	}
	for _, c := range yes {
		require.True(t, isUpstreamBodyParseRejection(c.status, []byte(c.body), true), "%d %s", c.status, c.body)
	}
	no := []struct {
		status int
		body   string
	}{
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be <= 8192"}}`},
		{400, `{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window"}}`},
		{401, `invalid json`},
		{500, ``},
	}
	// 截断的错误体不因为「不是完整 JSON」就算解析失败，但特征文本仍然算。
	require.False(t, isUpstreamBodyParseRejection(400, []byte(`{"error":{"message":"context too long`), false))
	require.True(t, isUpstreamBodyParseRejection(400, []byte(`{"error":{"message":"invalid JSON request body`), false))
	for _, c := range no {
		require.False(t, isUpstreamBodyParseRejection(c.status, []byte(c.body), true), "%d %s", c.status, c.body)
	}
}

func TestUpstreamBodyCompression_BlocklistExpires(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newUpstreamBodyCompressionBlocklist()
	b.now = func() time.Time { return now }
	b.block(1, time.Minute)
	require.True(t, b.blocked(1))
	now = now.Add(59 * time.Second)
	require.True(t, b.blocked(1))
	now = now.Add(2 * time.Second)
	require.False(t, b.blocked(1))
}

func TestUpstreamBodyCompression_ResolveSettings(t *testing.T) {
	s := resolveUpstreamBodyCompressionSettings(nil)
	require.Equal(t, int64(16<<10), s.minBytes)
	require.Equal(t, 3, s.zstdLevel)
	require.Equal(t, time.Hour, s.fallbackTTL)

	cfg := &config.Config{}
	cfg.Gateway.UpstreamBodyCompression = config.UpstreamBodyCompressionConfig{MinBytes: 1024, ZstdLevel: 9, GzipLevel: 1, FallbackTTLSeconds: 60}
	s = resolveUpstreamBodyCompressionSettings(cfg)
	require.Equal(t, int64(1024), s.minBytes)
	require.Equal(t, 9, s.zstdLevel)
	require.Equal(t, 1, s.gzipLevel)
	require.Equal(t, time.Minute, s.fallbackTTL)
}

// 真实发送路径：OpenAI doOpenAIUpstream 和 Anthropic doAnthropicUpstreamWithTLS 都接上了压缩。
func TestUpstreamBodyCompression_WiredIntoOpenAIAndAnthropicSendPaths(t *testing.T) {
	body := compressionTestBody(64 << 10)

	openaiUpstream := &httpUpstreamRecorder{resp: jsonResponse(http.StatusOK, `{"ok":true}`)}
	openaiSvc := &OpenAIGatewayService{httpUpstream: openaiUpstream, cfg: &config.Config{}}
	acct := compressionTestAccount(21, "zstd")
	resp, err := openaiSvc.doOpenAIUpstream(compressionTestRequest(t, body), "", acct)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "zstd", openaiUpstream.lastReq.Header.Get("Content-Encoding"))
	require.Equal(t, body, decodeBody(t, "zstd", openaiUpstream.lastBody))

	anthropicUpstream := &httpUpstreamRecorder{resp: jsonResponse(http.StatusOK, `{"ok":true}`)}
	gatewaySvc := &GatewayService{httpUpstream: anthropicUpstream, cfg: &config.Config{}}
	anthropicAcct := compressionTestAccount(22, "gzip")
	anthropicAcct.Platform = PlatformAnthropic
	resp, err = gatewaySvc.doAnthropicUpstreamWithTLS(compressionTestRequest(t, body), "", anthropicAcct, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "gzip", anthropicUpstream.lastReq.Header.Get("Content-Encoding"))
	require.Equal(t, body, decodeBody(t, "gzip", anthropicUpstream.lastBody))

	// 没开的号：上游收到的就是原请求。
	plain := &httpUpstreamRecorder{resp: jsonResponse(http.StatusOK, `{"ok":true}`)}
	openaiSvc.httpUpstream = plain
	req := compressionTestRequest(t, body)
	_, err = openaiSvc.doOpenAIUpstream(req, "", compressionTestAccount(23, ""))
	require.NoError(t, err)
	require.Same(t, req, plain.lastReq)
	require.Equal(t, body, plain.lastBody)
}
