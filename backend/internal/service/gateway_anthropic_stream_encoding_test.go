//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicStreamUpstreamRequestsUseIdentityEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &GatewayService{cfg: &config.Config{}}
	account := newAnthropicAPIKeyAccountForTest()
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("Accept-Encoding", "gzip")
			body := []byte(`{"model":"claude-sonnet-5-5","stream":false,"messages":[]}`)
			if stream {
				body = []byte(`{"model":"claude-sonnet-5-5","stream":true,"messages":[]}`)
			}

			generic, _, err := service.buildUpstreamRequest(context.Background(), c, account, body,
				"upstream-key", "apikey", "claude-sonnet-5-5", stream, false)
			require.NoError(t, err)
			passthrough, _, err := service.buildUpstreamRequestAnthropicAPIKeyPassthrough(
				context.Background(), c, account, body, "upstream-key")
			require.NoError(t, err)
			want := "gzip"
			if stream {
				want = "identity"
			}
			for _, req := range []*http.Request{generic, passthrough} {
				require.Equal(t, want, getHeaderRaw(req.Header, "accept-encoding"))
				count := 0
				for key, values := range req.Header {
					if http.CanonicalHeaderKey(key) == "Accept-Encoding" {
						count += len(values)
					}
				}
				require.Equal(t, 1, count)
			}
		})
	}
}
