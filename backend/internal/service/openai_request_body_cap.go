package service

import (
	"context"
	"net/http"
	"strings"
)

// AccountExtraMaxRequestBodyBytes 是账号 extra 中的键：该上游能接受的最大请求体字节数。
// 未配置或 <=0 表示不限制。超过上限的请求在选号阶段就跳过该账号，不再把几十 MB
// 的请求体传上去再等它拒绝。
//
// 例：zero.cat（new-api 改版）超过 32 MiB 回 400 "Invalid task protocol request"，
// 超过约 50 MiB 由 openresty 回 413、经 Cloudflare 常被包成 HTML 502，超过 100 MiB
// 由 Cloudflare 回 413。三种都不是账号故障，换一个号就能成功。
const AccountExtraMaxRequestBodyBytes = "max_request_body_bytes"

// newAPIBodyParseLimitMinBytes 是把 new-api 的 "Invalid task protocol request"
// 当作请求体超限的最小入站字节数。new-api 读请求体有 MAX_REQUEST_BODY_MB 上限
// （默认 32），超限后解析失败回的就是这句；但请求体本身不是合法 JSON 时也回同一句，
// 所以只在请求体确实大到接近上限时才这样认定。
const newAPIBodyParseLimitMinBytes = 16 << 20

const openAIAccountRequestBodyTooLargeReason = "request_body_too_large"

type openAIInboundRequestBodyBytesContextKey struct{}

// WithOpenAIInboundRequestBodyBytes 记录本次入站请求体的字节数，供选号时按账号
// 请求体上限过滤，以及识别 new-api 的请求体超限报错。
func WithOpenAIInboundRequestBodyBytes(ctx context.Context, n int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, openAIInboundRequestBodyBytesContextKey{}, int64(n))
}

func openAIInboundRequestBodyBytesFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	n, _ := ctx.Value(openAIInboundRequestBodyBytesContextKey{}).(int64)
	return n
}

// OpenAIMaxRequestBodyBytes 返回账号配置的请求体上限，未配置时为 0。
func (a *Account) OpenAIMaxRequestBodyBytes() int64 {
	if a == nil || a.Extra == nil {
		return 0
	}
	v, ok := a.Extra[AccountExtraMaxRequestBodyBytes]
	if !ok {
		return 0
	}
	n := int64(parseExtraFloat64(v))
	if n <= 0 {
		return 0
	}
	return n
}

// openAIAccountRejectsRequestBodySize 判断本次请求体是否超过账号的请求体上限。
func openAIAccountRejectsRequestBodySize(ctx context.Context, account *Account) bool {
	limit := account.OpenAIMaxRequestBodyBytes()
	if limit <= 0 {
		return false
	}
	return openAIInboundRequestBodyBytesFromContext(ctx) > limit
}

// isNewAPIBodyParseLimitRejection 识别 new-api 上游因请求体超过 MAX_REQUEST_BODY_MB
// 而回的 400 "Invalid task protocol request"。requestBytes 取转发体与入站体中较大者。
func isNewAPIBodyParseLimitRejection(statusCode int, upstreamMsg string, requestBytes int64) bool {
	if statusCode != http.StatusBadRequest || requestBytes < newAPIBodyParseLimitMinBytes {
		return false
	}
	return strings.Contains(strings.ToLower(upstreamMsg), "invalid task protocol request")
}

// markOpenAIRequestBodyTooLarge 把 failover 错误标成「这个号的请求体上限不够」：
// 换下一个号、不在同号重试，全部用尽时给客户端固定的 413 文案。
func markOpenAIRequestBodyTooLarge(failoverErr *UpstreamFailoverError) {
	if failoverErr == nil {
		return
	}
	failoverErr.RetryableOnSameAccount = false
	failoverErr.RequestScopedTransient = false
	failoverErr.Scope = GatewayFailureScopeAccount
	failoverErr.Reason = openAIRequestBodyTooLargeReason
	failoverErr.NextAccountAction = NextAccountRetry
	failoverErr.ClientStatusCode = http.StatusRequestEntityTooLarge
	failoverErr.ClientMessage = OpenAIRequestBodyTooLargeClientMessage
}
