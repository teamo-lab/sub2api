package service

import "net/http"

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
//
// 大请求体挂着上传看门狗（见 openai_upload_watchdog.go）：上传明显传不完时中止这一跳，
// 返回 *openAIUploadStalledError，由 handleOpenAIUpstreamTransportError 换号。
//
// API Key 账号开了 request_body_compression 时，请求体先压缩再发（见 upstream_body_compression.go）；
// 看门狗挂在压缩之后，按实际上线的字节计速。压缩被拒后的不压缩重发同样挂看门狗、仍算同一跳。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	noteOpenAIUpstreamAttempt(request.Context())
	return sendUpstreamWithBodyCompression(request, account, s.cfg, func(req *http.Request) (*http.Response, error) {
		return s.doOpenAIUpstreamOnce(req, proxyURL, account)
	})
}

func (s *OpenAIGatewayService) doOpenAIUpstreamOnce(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	settings, watch := resolveOpenAIUploadWatchdogSettings(s.cfg)
	request, finish := watchOpenAIUpload(request, settings, watch)
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, finish(err)
		}
	}
	response, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	return response, finish(err)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
