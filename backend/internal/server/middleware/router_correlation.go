package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 前置 TeamoRouter 在转发到 Sub2API 时带上的关联头。Sub2API 只信任其“声明”语义：
// 值写进 Ops 错误日志与请求日志用于排障对单，不参与鉴权、计费或选路；
// 上游转发走白名单，这两个头不会再传给上游账号。
const (
	routerRequestIDHeader       = "X-Teamo-Router-Request-Id"
	routerUserIDHeader          = "X-Teamo-Router-User-Id"
	maxRouterCorrelationIDBytes = 64
)

// normalizeRouterCorrelationID 只接受短的 [A-Za-z0-9._:-] 标识（router 的 UUID / u-xxxx uid），
// 其余一律丢弃，避免任意调用方往错误日志里塞长串或控制字符。
func normalizeRouterCorrelationID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxRouterCorrelationIDBytes {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-' || ch == '_' || ch == '.' || ch == ':':
		default:
			return "", false
		}
	}
	return value, true
}

// applyRouterCorrelation 把 router 声明的 request_id / uid 放进 request context 和请求日志字段。
// 由 ClientRequestID 中间件在 c.Next() 之前调用，所有挂了 client_request_id 的网关路由都会带上。
func applyRouterCorrelation(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	requestID, okRequest := normalizeRouterCorrelationID(c.GetHeader(routerRequestIDHeader))
	userID, okUser := normalizeRouterCorrelationID(c.GetHeader(routerUserIDHeader))
	if !okRequest && !okUser {
		return
	}
	ctx := c.Request.Context()
	fields := make([]zap.Field, 0, 2)
	if okRequest {
		ctx = context.WithValue(ctx, ctxkey.RouterRequestID, requestID)
		fields = append(fields, zap.String("router_request_id", requestID))
	}
	if okUser {
		ctx = context.WithValue(ctx, ctxkey.RouterUserID, userID)
		fields = append(fields, zap.String("router_user_id", userID))
	}
	ctx = logger.IntoContext(ctx, logger.FromContext(ctx).With(fields...))
	c.Request = c.Request.WithContext(ctx)
}
