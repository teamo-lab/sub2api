package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// doAnthropicUpstreamWithTLS 发一跳 Anthropic /v1/messages 请求；API Key 账号开了 request_body_compression 时
// 请求体先压缩再发（见 upstream_body_compression.go）。
func (s *GatewayService) doAnthropicUpstreamWithTLS(req *http.Request, proxyURL string, account *Account, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return sendUpstreamWithBodyCompression(req, account, s.cfg, func(r *http.Request) (*http.Response, error) {
		return s.httpUpstream.DoWithTLS(r, proxyURL, account.ID, account.Concurrency, profile)
	})
}
