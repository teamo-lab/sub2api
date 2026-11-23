//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewGeminiUpstreamRequestBodyAndReplay(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	req, err := newGeminiUpstreamRequest(context.Background(), http.MethodPost, "https://example.test/v1beta/models/m:generateContent", body)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), req.ContentLength)
	require.NotNil(t, req.GetBody)

	// 还没发完：Transport 关掉旧请求体后照样能重放。
	buf := make([]byte, 10)
	_, err = req.Body.Read(buf)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	replay, err := req.GetBody()
	require.NoError(t, err)
	got, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.Equal(t, body, got)

	// 完整发出之后、收到响应头之前（HTTP/2 REFUSED_STREAM / GOAWAY、307/308）仍然要能重放。
	again, err := req.GetBody()
	require.NoError(t, err)
	got, err = io.ReadAll(again)
	require.NoError(t, err)
	require.Equal(t, body, got)

	// Do 返回之后放掉：不再持有字节，也不能再重放。
	releaseGeminiUpstreamRequestBody(req)
	_, err = req.GetBody()
	require.ErrorIs(t, err, errGeminiUpstreamBodyReleased)
}

// 307 重定向在请求体完整发出之后靠 GetBody 重放：必须照常把同一份请求体发到新地址。
func TestNewGeminiUpstreamRequestReplaysOnRedirectAfterFullSend(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"redirect me"}]}]}`)
	var final []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // 先把请求体完整读完再重定向
		http.Redirect(w, r, "/new", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/new", func(w http.ResponseWriter, r *http.Request) {
		final, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, err := newGeminiUpstreamRequest(context.Background(), http.MethodPost, srv.URL+"/old", body)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, body, final)
	releaseGeminiUpstreamRequestBody(req)
}

func TestReleaseGeminiUpstreamRequestBodyIgnoresOtherRequests(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
	require.NoError(t, err)
	releaseGeminiUpstreamRequestBody(req)
	releaseGeminiUpstreamRequestBody(nil)
}

func TestNewGeminiUpstreamRequestEmptyBody(t *testing.T) {
	req, err := newGeminiUpstreamRequest(context.Background(), http.MethodPost, "https://example.test/x", nil)
	require.NoError(t, err)
	require.Equal(t, http.NoBody, req.Body)
	require.Zero(t, req.ContentLength)
}

// 走真实的 http.Client：服务端收到的字节和 Content-Length 都和原来一样。
func TestNewGeminiUpstreamRequestOverRealHTTP(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"` + string(make([]byte, 0)) + `hello over http"}]}]}`)
	var gotLen int64
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	req, err := newGeminiUpstreamRequest(context.Background(), http.MethodPost, srv.URL, body)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int64(len(body)), gotLen)
	require.Equal(t, body, got)
}
