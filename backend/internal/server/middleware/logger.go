package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger 请求日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()

		// 请求路径
		path := c.Request.URL.Path
		// Profile inference requests only; admin/health traffic is not a model request.
		profiled := isProfiledInferenceRequest(c.Request)
		if profiled && requestprofile.From(c.Request.Context()) == nil {
			c.Request = c.Request.WithContext(requestprofile.Attach(c.Request.Context(), startTime))
		}

		// 处理请求
		c.Next()

		// 跳过健康检查等高频探针路径的日志
		if path == "/health" || path == "/setup/status" {
			return
		}

		endTime := time.Now()
		latency := endTime.Sub(startTime)

		method := c.Request.Method
		statusCode := c.Writer.Status()
		clientIP := ip.GetClientIP(c)
		protocol := c.Request.Proto
		accountID, hasAccountID := c.Request.Context().Value(ctxkey.AccountID).(int64)
		platform, _ := c.Request.Context().Value(ctxkey.Platform).(string)
		model, _ := c.Request.Context().Value(ctxkey.Model).(string)
		reason, rejected := GetIngressRejectReason(c)
		if rejected {
			recordIngressReject(c, reason)
			allowed, droppedSummary := globalIngressRejectAccessSampler.allow(endTime)
			if droppedSummary > 0 {
				logger.FromContext(c.Request.Context()).Info("ingress rejection access logs dropped",
					zap.String("component", "http.access"),
					zap.Uint64("dropped_count", droppedSummary),
					zap.Bool(logger.OpsSystemLogSkipField, true),
				)
			}
			if !allowed {
				return
			}
		}

		fields := []zap.Field{
			zap.String("component", "http.access"),
			zap.Int("status_code", statusCode),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("client_ip", clientIP),
			zap.String("protocol", protocol),
			zap.String("method", method),
			zap.String("path", path),
		}
		if rejected {
			fields = append(fields,
				zap.String("ingress_reject_reason", string(reason)),
				zap.Bool(logger.OpsSystemLogSkipField, true),
			)
		}
		if hasAccountID && accountID > 0 {
			fields = append(fields, zap.Int64("account_id", accountID))
		}
		if platform != "" {
			fields = append(fields, zap.String("platform", platform))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}

		if profiled {
			group := int64(0)
			if key, ok := GetAPIKeyFromContext(c); ok && key.GroupID != nil {
				group = *key.GroupID
			}
			wire := "http"
			if strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
				wire = "sse"
			}
			if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
				wire = "websocket_session"
			}
			requestprofile.Metadata(c.Request.Context(), group, model, wire)
			if c.Request.Context().Err() != nil {
				requestprofile.Mark(c.Request.Context(), "client_cancelled", statusCode)
			}
			fields = append(fields, zap.Any("request_profile", requestprofile.Finish(c.Request.Context(), endTime)))
		}
		l := logger.FromContext(c.Request.Context()).With(fields...)
		l.Info("http request completed", zap.Time("completed_at", endTime))
		if profiled && !l.Core().Enabled(zap.InfoLevel) {
			enc := zapcore.NewMapObjectEncoder()
			for _, field := range fields {
				field.AddTo(enc)
			}
			enc.Fields["request_id"], _ = c.Request.Context().Value(ctxkey.RequestID).(string)
			enc.Fields["client_request_id"], _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
			logger.WriteSinkEvent("info", "http.access", "http request completed", enc.Fields)
		}

		if len(c.Errors) > 0 {
			l.Warn("http request contains gin errors", zap.String("errors", c.Errors.String()))
		}
	}
}
