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

	// 完整发出一次之后不再持有字节，也不能再重放。
	_, err = req.GetBody()
	require.ErrorIs(t, err, errGeminiUpstreamBodyReleased)
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
