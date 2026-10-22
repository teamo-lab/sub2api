package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/rawcapture"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// rawCaptureHeaders 是随原文一起保存的请求头白名单；凭证类头一律不存。
var rawCaptureHeaders = []string{
	"User-Agent", "Content-Type", "Content-Encoding", "Accept",
	"Session_id", "Session-Id", "Conversation_id", "Originator", "Version",
	"X-Client-Source", "X-Request-Id", "X-Oneapi-Request-Id", "X-Trace-Id",
	"Openai-Beta", "Anthropic-Version", "Anthropic-Beta",
}

var (
	rawCaptureOnce     sync.Once
	rawCaptureInstance *rawcapture.Capturer
)

// RawCapture 在 API Key 鉴权之后运行：读出请求体、交还给后续 handler，请求结束后把原文和
// 元数据非阻塞地放进上传队列。未开启、非 POST、分组不在名单里时直接放行。
// 读请求体出错（例如超过 MaxBytesReader 上限）时原样交还已读部分和错误，不改变下游行为。
func RawCapture(cfg *config.Config) gin.HandlerFunc {
	if cfg == nil || !cfg.Gateway.RawCapture.Enabled {
		return func(c *gin.Context) { c.Next() }
	}
	rc := cfg.Gateway.RawCapture
	capturer := sharedRawCapturer(rc)
	if capturer == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return rawCaptureHandler(rc, capturer)
}

type rawCaptureSink interface {
	Enqueue(rec *rawcapture.Record) bool
	Stats() rawcapture.Stats
}

func rawCaptureHandler(rc config.RawCaptureConfig, capturer rawCaptureSink) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.Method != http.MethodPost || c.Request.Body == nil {
			c.Next()
			return
		}
		value, _ := c.Get("api_key")
		apiKey, _ := value.(*service.APIKey)
		var groupID int64
		if apiKey != nil && apiKey.GroupID != nil {
			groupID = *apiKey.GroupID
		}
		if len(rc.GroupIDs) > 0 && !slices.Contains(rc.GroupIDs, groupID) {
			c.Next()
			return
		}

		started := time.Now()
		body, readErr := io.ReadAll(io.LimitReader(c.Request.Body, rc.MaxBodyBytes+1))
		truncated := int64(len(body)) > rc.MaxBodyBytes
		switch {
		case readErr != nil:
			// 交还已读部分和同一个错误，让 handler 按原逻辑返回 413/400。
			c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{readErr}))
		case truncated:
			// 超过落盘上限：已读部分加剩余流交还 handler，原文只记元数据。
			c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
		default:
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}

		c.Next()

		rec := &rawcapture.Record{
			StartedAt:  started,
			DurationMs: time.Since(started).Milliseconds(),
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			Status:     c.Writer.Status(),
			GroupID:    groupID,
			Headers:    captureHeaders(c.Request.Header),
		}
		if id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); id != "" {
			rec.ClientRequestID = id
		}
		if apiKey != nil {
			rec.APIKeyID = apiKey.ID
			rec.UserID = apiKey.UserID
		}
		if readErr != nil {
			rec.Extra = map[string]any{"read_error": readErr.Error()}
		}
		if truncated || readErr != nil {
			rec.BodyBytes = len(body)
			rec.BodyTruncated = true
		} else {
			rec.BodyBytes = len(body)
			rec.Body = body
			if model := gjson.GetBytes(body, "model"); model.Exists() {
				rec.Model = model.String()
			}
			rec.Stream = gjson.GetBytes(body, "stream").Bool()
		}
		if !capturer.Enqueue(rec) {
			logger.FromContext(c.Request.Context()).Warn("raw_capture.dropped",
				zap.String("client_request_id", rec.ClientRequestID),
				zap.Int("body_bytes", rec.BodyBytes),
				zap.Uint64("dropped_total", capturer.Stats().DroppedFull))
		}
	}
}

func sharedRawCapturer(rc config.RawCaptureConfig) *rawcapture.Capturer {
	rawCaptureOnce.Do(func() {
		c, err := rawcapture.New(context.Background(), rawcapture.Options{
			Endpoint:        rc.Endpoint,
			Region:          rc.Region,
			Bucket:          rc.Bucket,
			Prefix:          rc.Prefix,
			AccessKeyID:     rc.AccessKeyID,
			SecretAccessKey: rc.SecretAccessKey,
			ForcePathStyle:  rc.ForcePathStyle,
			QueueSize:       rc.QueueSize,
			MaxQueueBytes:   rc.MaxQueueBytes,
			Workers:         rc.Workers,
			UploadTimeout:   time.Duration(rc.UploadTimeoutSeconds) * time.Second,
		}, func(rec *rawcapture.Record, err error) {
			logger.L().Warn("raw_capture.upload_failed",
				zap.String("client_request_id", rec.ClientRequestID),
				zap.Int("body_bytes", rec.BodyBytes),
				zap.Error(err))
		})
		if err != nil {
			logger.L().Error("raw_capture.init_failed", zap.Error(err))
			return
		}
		rawCaptureInstance = c
		logger.L().Info("raw_capture.enabled",
			zap.String("bucket", rc.Bucket),
			zap.String("prefix", rc.Prefix),
			zap.Int64s("group_ids", rc.GroupIDs))
	})
	return rawCaptureInstance
}

func captureHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(rawCaptureHeaders))
	for _, name := range rawCaptureHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			out[name] = v
		}
	}
	return out
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
