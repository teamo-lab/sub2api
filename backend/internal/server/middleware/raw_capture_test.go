package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/rawcapture"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeRawSink struct {
	mu   sync.Mutex
	recs []*rawcapture.Record
	full bool
}

func (f *fakeRawSink) Enqueue(rec *rawcapture.Record) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.full {
		return false
	}
	f.recs = append(f.recs, rec)
	return true
}

func (f *fakeRawSink) Stats() rawcapture.Stats { return rawcapture.Stats{} }

func newRawCaptureRouter(rc config.RawCaptureConfig, sink rawCaptureSink, groupID int64, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ClientRequestIDWithOptions(true))
	r.Use(func(c *gin.Context) {
		gid := groupID
		c.Set("api_key", &service.APIKey{ID: 7, UserID: 9, GroupID: &gid})
		c.Next()
	})
	r.Use(rawCaptureHandler(rc, sink))
	r.POST("/v1/responses", handler)
	return r
}

func TestRawCaptureKeepsBodyForHandlerAndEnqueuesRecord(t *testing.T) {
	sink := &fakeRawSink{}
	body := `{"model":"gpt-6-astra","stream":true,"input":"hello"}`
	var seen string
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 1 << 20, GroupIDs: []int64{5}}, sink, 5, func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		seen = string(b)
		c.Status(http.StatusTeapot)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "new-api")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, body, seen)
	require.Len(t, sink.recs, 1)
	rec := sink.recs[0]
	require.Equal(t, body, string(rec.Body))
	require.Equal(t, http.StatusTeapot, rec.Status)
	require.Equal(t, "gpt-6-astra", rec.Model)
	require.True(t, rec.Stream)
	require.Equal(t, int64(5), rec.GroupID)
	require.Equal(t, int64(7), rec.APIKeyID)
	require.NotContains(t, rec.Headers, "Authorization")
	require.Equal(t, "new-api", rec.Headers["User-Agent"])
	// 落盘对象名、响应头里的 X-Oneapi-Request-Id 和 X-Client-Request-ID 是同一个 ID。
	require.NotEmpty(t, rec.ClientRequestID)
	require.Equal(t, rec.ClientRequestID, w.Header().Get("X-Oneapi-Request-Id"))
	require.Equal(t, rec.ClientRequestID, w.Header().Get("X-Client-Request-ID"))
}

func TestRawCaptureSkipsGroupsOutsideAllowlist(t *testing.T) {
	sink := &fakeRawSink{}
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 1 << 20, GroupIDs: []int64{5}}, sink, 6, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		require.Equal(t, "{}", string(b))
		c.Status(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}")))
	require.Empty(t, sink.recs)
}

func TestRawCaptureOversizedBodyStillReachesHandlerButOnlyMetadataIsKept(t *testing.T) {
	sink := &fakeRawSink{}
	body := bytes.Repeat([]byte("a"), 100)
	var seen int
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 10}, sink, 5, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		seen = len(b)
		c.Status(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
	require.Equal(t, 100, seen)
	require.Len(t, sink.recs, 1)
	require.True(t, sink.recs[0].BodyTruncated)
	require.Nil(t, sink.recs[0].Body)
}

func TestRawCaptureReadErrorIsPassedThroughToHandler(t *testing.T) {
	sink := &fakeRawSink{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 5)
		c.Next()
	})
	r.Use(rawCaptureHandler(config.RawCaptureConfig{MaxBodyBytes: 1 << 20}, sink))
	var handlerErr error
	r.POST("/v1/responses", func(c *gin.Context) {
		_, handlerErr = io.ReadAll(c.Request.Body)
		c.Status(http.StatusRequestEntityTooLarge)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("0123456789")))
	var maxErr *http.MaxBytesError
	require.ErrorAs(t, handlerErr, &maxErr)
	require.Len(t, sink.recs, 1)
	require.True(t, sink.recs[0].BodyTruncated)
	require.Equal(t, http.StatusRequestEntityTooLarge, sink.recs[0].Status)
}

func TestRawCaptureQueueFullDoesNotAffectResponse(t *testing.T) {
	sink := &fakeRawSink{full: true}
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 1 << 20}, sink, 5, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}")))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "ok", w.Body.String())
}

func TestClientRequestIDDoesNotExposeOneAPIHeaderByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ClientRequestID())
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	require.NotEmpty(t, w.Header().Get("X-Client-Request-ID"))
	require.Empty(t, w.Header().Get("X-Oneapi-Request-Id"))
}

func TestRawCaptureRecordsJSONResponse(t *testing.T) {
	sink := &fakeRawSink{}
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20}, sink, 5, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": "resp_1", "status": "completed"})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}")))
	require.Len(t, sink.recs, 1)
	rec := sink.recs[0]
	require.Equal(t, w.Body.String(), string(rec.Response))
	require.Equal(t, int64(w.Body.Len()), rec.ResponseBytes)
	require.False(t, rec.ResponseTruncated)
	require.Contains(t, rec.ResponseHeaders["Content-Type"], "application/json")
	require.Equal(t, rec.ClientRequestID, rec.ResponseHeaders["X-Oneapi-Request-Id"])
}

func TestRawCaptureStreamKeepsHeadAndTailAndStillFlushes(t *testing.T) {
	sink := &fakeRawSink{}
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 1 << 20, MaxResponseBytes: 20}, sink, 5, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		for _, chunk := range []string{"AAAAAAAAAA", "BBBBBBBBBB", "CCCCCCCCCC", "DDDDDDDDDD", "EEEEEEEEEE"} {
			_, _ = c.Writer.WriteString(chunk)
			c.Writer.Flush()
		}
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}")))
	require.True(t, w.Flushed)
	require.Equal(t, 50, w.Body.Len())
	rec := sink.recs[0]
	require.Equal(t, int64(50), rec.ResponseBytes)
	require.True(t, rec.ResponseTruncated)
	require.Equal(t, "AAAAAAAAAAEEEEEEEEEE", string(rec.Response))
}

func TestCaptureWriterTailTrimAcrossManySmallWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	cw := newCaptureWriter(c.Writer, 8, nil)
	for i := 0; i < 100; i++ {
		_, _ = cw.Write([]byte{byte('0' + i%10)})
	}
	got, total, truncated := cw.captured()
	require.Equal(t, int64(100), total)
	require.True(t, truncated)
	require.Equal(t, "01236789", string(got))
	require.Equal(t, 100, w.Body.Len())
}

func TestRawCaptureSkipsBodyWhenContentLengthExceedsLimit(t *testing.T) {
	sink := &fakeRawSink{}
	body := bytes.Repeat([]byte("x"), 100)
	var seen int
	r := newRawCaptureRouter(config.RawCaptureConfig{MaxBodyBytes: 10, MaxResponseBytes: 1 << 20}, sink, 5, func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		seen = len(b)
		c.Status(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
	require.Equal(t, 100, seen)
	require.Len(t, sink.recs, 1)
	require.True(t, sink.recs[0].BodyTruncated)
	require.Equal(t, 100, sink.recs[0].BodyBytes)
	require.Equal(t, "body_too_large", sink.recs[0].Extra["body_skipped"])
}

func TestRawCaptureInflightBudgetSkipsAndIsReleased(t *testing.T) {
	sink := &fakeRawSink{}
	rc := config.RawCaptureConfig{MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxInflightBytes: 16}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ClientRequestIDWithOptions(true))
	h := rawCaptureHandler(rc, sink)
	r.Use(h)
	r.POST("/v1/responses", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, "0123456789abcdefXYZ")
	})
	// 第一条：请求体 10 字节占额度，响应只能再复制 6 字节就到上限。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("0123456789")))
	require.Equal(t, "0123456789abcdefXYZ", w.Body.String())
	require.Len(t, sink.recs, 1)
	require.Equal(t, "0123456789", string(sink.recs[0].Body))
	require.Equal(t, "inflight_budget", sink.recs[0].Extra["response_skipped"])
	require.Equal(t, int64(19), sink.recs[0].ResponseBytes)
	// 第二条：请求体 20 字节超过额度，只记元数据；额度在上一条结束后已经全部归还。
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("01234567890123456789")))
	require.Len(t, sink.recs, 2)
	require.Equal(t, "inflight_budget", sink.recs[1].Extra["body_skipped"])
	require.Nil(t, sink.recs[1].Body)
	// 第三条：小请求体又能完整落盘，说明前两条的额度已全部归还、没有泄漏。
	// 响应是一次写出 19 字节，超过剩余额度，整块不复制（不拆分单次写入）。
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}")))
	require.Len(t, sink.recs, 3)
	require.Equal(t, "{}", string(sink.recs[2].Body))
	require.NotContains(t, sink.recs[2].Extra, "body_skipped")
	require.Equal(t, "inflight_budget", sink.recs[2].Extra["response_skipped"])
}

func TestCloseRawCaptureWithoutInstanceIsNoop(t *testing.T) {
	require.Nil(t, rawCaptureInstance)
	CloseRawCapture(context.Background())
}

func TestRawCaptureRecordsPanickingRequestAndRepanics(t *testing.T) {
	sink := &fakeRawSink{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(ClientRequestIDWithOptions(true))
	r.Use(rawCaptureHandler(config.RawCaptureConfig{MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxInflightBytes: 1 << 20}, sink))
	r.POST("/v1/responses", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		panic("boom")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-6-astra"}`)))
	require.Equal(t, http.StatusInternalServerError, w.Code, "panic must still reach gin.Recovery")
	require.Len(t, sink.recs, 1)
	rec := sink.recs[0]
	require.Equal(t, http.StatusInternalServerError, rec.Status)
	require.Equal(t, `{"model":"gpt-6-astra"}`, string(rec.Body))
	require.Equal(t, "boom", rec.Extra["panic"])
}

func TestRawCaptureChunkedBodyReservationShrinksToActualSize(t *testing.T) {
	sink := &fakeRawSink{}
	// 上限 1MiB、额度 1MiB+10：分块请求按上限预占，读完后必须缩到实际大小，
	// 否则 handler 运行期间的响应副本会因额度不足被跳过。
	rc := config.RawCaptureConfig{MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxInflightBytes: 1<<20 + 10}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ClientRequestIDWithOptions(true))
	r.Use(rawCaptureHandler(rc, sink))
	r.POST("/v1/responses", func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, strings.Repeat("r", 1000))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString("{}"))
	req.ContentLength = -1
	r.ServeHTTP(httptest.NewRecorder(), req)
	require.Len(t, sink.recs, 1)
	require.Equal(t, "{}", string(sink.recs[0].Body))
	require.NotContains(t, sink.recs[0].Extra, "response_skipped")
	require.Equal(t, 1000, len(sink.recs[0].Response))
}
