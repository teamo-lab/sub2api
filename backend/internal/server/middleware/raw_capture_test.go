package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
	cw := newCaptureWriter(c.Writer, 8)
	for i := 0; i < 100; i++ {
		_, _ = cw.Write([]byte{byte('0' + i%10)})
	}
	got, total, truncated := cw.captured()
	require.Equal(t, int64(100), total)
	require.True(t, truncated)
	require.Equal(t, "01236789", string(got))
	require.Equal(t, 100, w.Body.Len())
}
