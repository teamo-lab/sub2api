package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const clientRequestIDHeader = "X-Client-Request-ID"

// oneAPIRequestIDHeader 是 new-api 识别上游请求标识的响应头。new-api 不把它转给自己的客户端，
// 而是存进 logs.upstream_request_id。
const oneAPIRequestIDHeader = "X-Oneapi-Request-Id"

// ClientRequestID ensures every request has a unique client_request_id in request.Context().
//
// This is used by the Ops monitoring module for end-to-end request correlation.
func ClientRequestID() gin.HandlerFunc {
	return ClientRequestIDWithOptions(false)
}

// ClientRequestIDWithOptions 与 ClientRequestID 相同；exposeOneAPI 为 true 时还会在响应头
// 回 X-Oneapi-Request-Id = client_request_id，供前置的 new-api 串联请求。
func ClientRequestIDWithOptions(exposeOneAPI bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		if v, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(v) != "" {
			var valid bool
			v, valid = normalizeCorrelationID(v)
			if !valid {
				v = uuid.New().String()
			}
			c.Header(clientRequestIDHeader, v)
			if exposeOneAPI {
				c.Header(oneAPIRequestIDHeader, v)
			}
			ctx := context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, v)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}

		id := uuid.New().String()
		c.Header(clientRequestIDHeader, id)
		if exposeOneAPI {
			c.Header(oneAPIRequestIDHeader, id)
		}
		ctx := context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, id)
		requestLogger := logger.FromContext(ctx).With(zap.String("client_request_id", strings.TrimSpace(id)))
		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
