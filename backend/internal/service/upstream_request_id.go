package service

import (
	"net/http"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

// AccountExtraUpstreamRequestIDHeader 是账户 extra 中的键，值为直接上游声明请求标识的响应头名。
// 未指定时不记录上游请求标识。
const AccountExtraUpstreamRequestIDHeader = "upstream_request_id_header"

const (
	maxUpstreamRequestIDHeaderNameLen = 64
	// maxUsageUpstreamRequestIDLen 与 usage_logs.upstream_request_id VARCHAR(128) 对齐。
	maxUsageUpstreamRequestIDLen = 128
)

// UpstreamRequestIDHeaderName 返回账户指定的上游请求标识头名，未指定时为空串。
func UpstreamRequestIDHeaderName(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.TrimSpace(account.GetExtraString(AccountExtraUpstreamRequestIDHeader))
}

// UpstreamRequestIDFromHeaders 从直接上游的响应头解析请求标识。
// 只读账户指定的头；账户未指定头名时恒为空串。
func UpstreamRequestIDFromHeaders(account *Account, h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	name := UpstreamRequestIDHeaderName(account)
	if name == "" {
		return ""
	}
	return strings.TrimSpace(h.Get(name))
}

// opsUpstreamRequestID 给 ops 上游错误事件（含换号失败的每一跳）取直接上游的请求标识：
// 优先账户指定的头；没配或拿不到时退回 x-request-id，再依次尝试 fallbackHeaders。
func opsUpstreamRequestID(account *Account, h http.Header, fallbackHeaders ...string) string {
	if len(h) == 0 {
		return ""
	}
	if id := UpstreamRequestIDFromHeaders(account, h); id != "" {
		return id
	}
	if id := strings.TrimSpace(h.Get("x-request-id")); id != "" {
		return id
	}
	for _, name := range fallbackHeaders {
		if id := strings.TrimSpace(h.Get(name)); id != "" {
			return id
		}
	}
	return ""
}

// opsUpstreamCFRay 在拿不到账户指定的业务请求标识时返回响应的 CF-Ray。
// Cloudflare 边缘直接返回的错误（403 1010、524 等）只带 CF-Ray，没有业务 ID；
// 退回读到的 x-request-id 也可能是链路里别处的标识，所以此时同样保留 CF-Ray。
func opsUpstreamCFRay(account *Account, h http.Header) string {
	if len(h) == 0 || UpstreamRequestIDFromHeaders(account, h) != "" {
		return ""
	}
	return strings.TrimSpace(h.Get("cf-ray"))
}

// recordOpenAIMidStreamError 记录已经向客户端写出内容之后的上游断流（HTTP/2 GOAWAY、
// 连接 reset、没收到终止事件就 EOF）。这时已不能换号，请求以 502 结束；上游响应头早已收到，
// 照样按账号指定的头记下上游 ID（拿不到业务 ID 时另存 cf-ray），让这一跳和换号失败、
// 最终成功一样能对上上游。
func recordOpenAIMidStreamError(c *gin.Context, account *Account, passthrough bool, headers http.Header, streamErr error) {
	if c == nil {
		return
	}
	message := "OpenAI stream disconnected after output started"
	if streamErr != nil {
		if text := strings.TrimSpace(streamErr.Error()); text != "" {
			message += ": " + text
		}
	}
	message = sanitizeUpstreamErrorMessage(message)
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	event := OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           PlatformOpenAI,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  opsUpstreamRequestID(account, headers),
		UpstreamCFRay:      opsUpstreamCFRay(account, headers),
		Passthrough:        passthrough,
		Kind:               "stream_error",
		Message:            message,
	}
	if account != nil {
		event.Platform = account.Platform
		event.AccountID = account.ID
		event.AccountName = account.Name
	}
	appendOpsUpstreamError(c, event)
}

// usageUpstreamRequestIDPtr 生成落库到 usage_logs.upstream_request_id 的值。
// WS 轮次没有 HTTP 响应头，保持 nil；超长时截断到列宽而不是让整条用量行失败。
func usageUpstreamRequestIDPtr(account *Account, h http.Header, wsMode bool) *string {
	if wsMode {
		return nil
	}
	id := UpstreamRequestIDFromHeaders(account, h)
	if id == "" {
		return nil
	}
	if len(id) > maxUsageUpstreamRequestIDLen {
		id = id[:maxUsageUpstreamRequestIDLen]
		for len(id) > 0 && !utf8.ValidString(id) {
			id = id[:len(id)-1]
		}
	}
	if id == "" {
		return nil
	}
	return &id
}

// upstreamRequestIDHeaderRequiredMessage 说明怎么为账户选上游请求标识头。
const upstreamRequestIDHeaderRequiredMessage = "extra.upstream_request_id_header is required for OpenAI API key accounts on this deployment " +
	"(gateway.require_upstream_request_id_header). Set it to the response header in which the direct upstream returns its own request ID " +
	"(check once with curl -sD - against the upstream): X-Oneapi-Request-Id for new-api forks such as zero.cat; " +
	"X-Request-Id for stock new-api v1.6.x; X-Client-Request-Id for another Sub2API station (its X-Request-Id is a chain ID, not its own); " +
	"x-request-id for api.openai.com"

// RequireUpstreamRequestIDHeaderExtra 在 gateway.require_upstream_request_id_header 打开时调用：
// OpenAI API Key 账号的 extra 必须带非空的上游请求标识头名，否则换号失败的每一跳都查不到上游 ID。
// 其它平台和类型不受影响。
func RequireUpstreamRequestIDHeaderExtra(platform, accountType string, extra map[string]any) error {
	if platform != PlatformOpenAI || accountType != AccountTypeAPIKey {
		return nil
	}
	if name, _ := extra[AccountExtraUpstreamRequestIDHeader].(string); strings.TrimSpace(name) != "" {
		return nil
	}
	return infraerrors.BadRequest("UPSTREAM_REQUEST_ID_HEADER_REQUIRED", upstreamRequestIDHeaderRequiredMessage)
}

// ValidateUpstreamRequestIDHeaderExtra 校验并规范化 extra 中的上游请求标识头名：
// 必须是合法的 HTTP 头字段名且不超过 64 字节；空白值视为未指定并从 extra 中移除。
func ValidateUpstreamRequestIDHeaderExtra(extra map[string]any) error {
	if extra == nil {
		return nil
	}
	raw, ok := extra[AccountExtraUpstreamRequestIDHeader]
	if !ok || raw == nil {
		return nil
	}
	name, ok := raw.(string)
	if !ok {
		return infraerrors.BadRequest("INVALID_UPSTREAM_REQUEST_ID_HEADER",
			"upstream_request_id_header must be a string")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		delete(extra, AccountExtraUpstreamRequestIDHeader)
		return nil
	}
	if len(name) > maxUpstreamRequestIDHeaderNameLen || !httpguts.ValidHeaderFieldName(name) {
		return infraerrors.BadRequest("INVALID_UPSTREAM_REQUEST_ID_HEADER",
			"upstream_request_id_header must be a valid HTTP header name of at most 64 bytes")
	}
	extra[AccountExtraUpstreamRequestIDHeader] = name
	return nil
}
