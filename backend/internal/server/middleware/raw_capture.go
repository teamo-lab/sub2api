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

// RawCapture 在 API Key 鉴权之后运行：读出请求体交还给后续 handler，并在写给客户端的同时
// 复制一份响应字节；请求结束后把请求、响应原文和元数据非阻塞地放进上传队列。未开启、非 POST、分组不在名单里时直接放行。
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
		cw := newCaptureWriter(c.Writer, rc.MaxResponseBytes)
		c.Writer = cw
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
		rec.Response, rec.ResponseBytes, rec.ResponseTruncated = cw.captured()
		rec.ResponseHeaders = captureResponseHeaders(cw.Header())
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
				zap.Int64("response_bytes", rec.ResponseBytes),
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

var rawCaptureResponseHeaders = []string{"Content-Type", "Content-Encoding", "X-Request-Id", "X-Client-Request-ID", "X-Oneapi-Request-Id"}

func captureResponseHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(rawCaptureResponseHeaders))
	for _, name := range rawCaptureResponseHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			out[name] = v
		}
	}
	return out
}

// captureWriter 在写给客户端的同时复制一份响应字节。总量不超过 limit 时全部保留；
// 超过时保留开头 limit/2 和最后 limit/2（SSE 结尾的 usage / completed 事件在后半段）。
// 只包装 Write/WriteString，Flush、Hijack 等沿用内嵌的 gin.ResponseWriter。
type captureWriter struct {
	gin.ResponseWriter
	limit int64
	head  []byte
	tail  []byte
	total int64
}

func newCaptureWriter(w gin.ResponseWriter, limit int64) *captureWriter {
	if limit <= 0 {
		limit = 16 << 20
	}
	return &captureWriter{ResponseWriter: w, limit: limit}
}

func (w *captureWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.keep(b[:n])
	return n, err
}

func (w *captureWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.keep([]byte(s[:n]))
	return n, err
}

// Unwrap 让 http.ResponseController 能找到底层 ResponseWriter。
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) keep(b []byte) {
	if len(b) == 0 {
		return
	}
	w.total += int64(len(b))
	half := int(w.limit / 2)
	if room := half - len(w.head); room > 0 {
		take := min(room, len(b))
		w.head = append(w.head, b[:take]...)
		b = b[take:]
	}
	if len(b) == 0 {
		return
	}
	w.tail = append(w.tail, b...)
	// 摊还裁剪：超过两倍上限才整体搬一次，避免每次写都拷贝。
	if len(w.tail) > 2*half {
		w.tail = append(w.tail[:0:0], w.tail[len(w.tail)-half:]...)
	}
}

// captured 返回保留的字节、实际写出的总字节数、是否截断。
func (w *captureWriter) captured() ([]byte, int64, bool) {
	half := int(w.limit / 2)
	tail := w.tail
	if len(tail) > half {
		tail = tail[len(tail)-half:]
	}
	truncated := w.total > int64(len(w.head)+len(tail))
	out := make([]byte, 0, len(w.head)+len(tail))
	out = append(out, w.head...)
	out = append(out, tail...)
	return out, w.total, truncated
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
