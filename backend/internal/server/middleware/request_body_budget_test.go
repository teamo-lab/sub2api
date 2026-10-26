package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNewRequestBodyBudget_Modes(t *testing.T) {
	require.Nil(t, NewRequestBodyBudget(config.RequestBodyBudgetConfig{MaxInflightBytes: -1}, 10<<30), "negative disables")
	require.Nil(t, NewRequestBodyBudget(config.RequestBodyBudgetConfig{}, math.MaxInt64), "auto without a memory limit disables")
	auto := NewRequestBodyBudget(config.RequestBodyBudgetConfig{MinBodyBytes: 1 << 20}, 10<<30)
	require.NotNil(t, auto)
	require.Equal(t, int64(1<<30), auto.Max(), "auto is a tenth of the memory limit")
	fixed := NewRequestBodyBudget(config.RequestBodyBudgetConfig{MaxInflightBytes: 123}, math.MaxInt64)
	require.NotNil(t, fixed)
	require.Equal(t, int64(123), fixed.Max())
}

// budgetRouter 模拟网关路由：bodyLimit → 鉴权 → 预算 → [路由级 limit] → handler。
// 请求头 X-Auth: no 时鉴权失败；查询参数 hold 时 handler 读完请求体后等 release 关闭再返回。
type budgetRouter struct {
	engine   *gin.Engine
	release  chan struct{}
	entered  chan struct{}
	seenBody atomic.Int64
}

func newBudgetRouter(budget *RequestBodyBudget, groupLimit int64, routeLimit int64) *budgetRouter {
	gin.SetMode(gin.TestMode)
	br := &budgetRouter{engine: gin.New(), release: make(chan struct{}), entered: make(chan struct{}, 64)}
	auth := func(c *gin.Context) {
		if c.GetHeader("X-Auth") == "no" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
			return
		}
		c.Next()
	}
	handler := func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		br.seenBody.Store(int64(len(body)))
		if c.Query("hold") != "" {
			br.entered <- struct{}{}
			<-br.release
		}
		c.String(http.StatusOK, "%d", len(body))
	}
	chain := []gin.HandlerFunc{RequestBodyLimit(groupLimit), auth, RequestBodyBudgetMiddleware(budget)}
	if routeLimit > 0 {
		chain = append(chain, RequestBodyLimit(routeLimit))
	}
	chain = append(chain, handler)
	for _, path := range []string{"/v1/responses", "/v1/messages", "/v1beta/models/x:generateContent", "/antigravity/v1beta/models/x:generateContent"} {
		br.engine.POST(path, chain...)
	}
	return br
}

func (br *budgetRouter) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	br.engine.ServeHTTP(rec, req)
	return rec
}

// holdAsync 发一个会被 handler 挂住的请求，等它进到 handler 再返回。
func (br *budgetRouter) holdAsync(t *testing.T, req *http.Request) <-chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- br.do(req) }()
	<-br.entered
	return done
}

func chunkedRequest(path string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, io.NopCloser(strings.NewReader(body)))
	req.ContentLength = -1
	return req
}

func TestRequestBodyBudget_KnownLengthReservesAndRejectsWith503(t *testing.T) {
	budget := &RequestBodyBudget{max: 1000, minBody: 100}
	br := newBudgetRouter(budget, 1<<20, 0)

	held := br.holdAsync(t, httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 800))))
	require.Equal(t, int64(800), budget.Used())

	// 800 + 300 > 1000：直接 503 + Retry-After，各协议的错误格式。
	rec := br.do(httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 300))))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "10", rec.Header().Get("Retry-After"))
	var openAIErr struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &openAIErr))
	require.Equal(t, "server_overloaded", openAIErr.Error.Code)
	rec = br.do(httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(make([]byte, 300))))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), `"overloaded_error"`)
	for _, path := range []string{"/v1beta/models/x:generateContent", "/antigravity/v1beta/models/x:generateContent"} {
		rec = br.do(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, 300))))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":503`, path)
		require.Contains(t, rec.Body.String(), `"status":"`+googleapi.HTTPStatusToGoogleStatus(http.StatusServiceUnavailable)+`"`, path)
	}

	// 小于 minBody 的请求不受预算影响。
	rec = br.do(httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 50))))
	require.Equal(t, http.StatusOK, rec.Code)

	close(br.release)
	require.Equal(t, http.StatusOK, (<-held).Code)
	require.Equal(t, int64(0), budget.Used(), "reservation is released when the request ends")
	rec = br.do(httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 300))))
	require.Equal(t, http.StatusOK, rec.Code)
}

// 未鉴权的请求在预算之前就被拒绝：既不预占也不读请求体。
func TestRequestBodyBudget_UnauthenticatedRequestNeverTouchesBudget(t *testing.T) {
	budget := &RequestBodyBudget{max: 1000, minBody: 100}
	br := newBudgetRouter(budget, 1<<20, 0)
	var read atomic.Int64
	body := &countingReader{r: strings.NewReader(strings.Repeat("x", 5000)), n: &read}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.ContentLength = -1
	req.Header.Set("X-Auth", "no")
	rec := br.do(req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, int64(0), read.Load(), "chunked body must not be read before auth")
	require.Equal(t, int64(0), budget.Used())

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 900)))
	req.Header.Set("X-Auth", "no")
	require.Equal(t, http.StatusUnauthorized, br.do(req).Code)
	require.Equal(t, int64(0), budget.Used())
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// 声明超过上限的请求按上限预占；路由级更小的上限再把预占缩下来，且每个请求只计一次。
func TestRequestBodyBudget_ReservationFollowsTheTightestLimitAndCountsOnce(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 100}
	// 分组级 1MiB → 预算 → 路由级 512KiB（同 /v1/embeddings 叠加 textBodyLimit）。
	br := newBudgetRouter(budget, 1<<20, 512<<10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 400<<10)))
	held := br.holdAsync(t, req)
	require.Equal(t, int64(400<<10), budget.Used(), "a body under every limit is charged its own size, once")
	close(br.release)
	<-held
	require.Equal(t, int64(0), budget.Used())

	// 声明 2MiB：先按分组上限 1MiB 预占，路由级 512KiB 再缩到 512KiB。
	var observed atomic.Int64
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/embeddings", RequestBodyLimit(1<<20), RequestBodyBudgetMiddleware(budget), func(c *gin.Context) {
		observed.Store(budget.Used())
		c.Next()
	}, RequestBodyLimit(512<<10), func(c *gin.Context) {
		observed.Store(observed.Load()<<32 | budget.Used())
		_, err := io.ReadAll(c.Request.Body)
		require.Error(t, err)
		c.Status(http.StatusRequestEntityTooLarge)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Equal(t, int64(1<<20), observed.Load()>>32, "declared-oversized body reserves the group limit")
	require.Equal(t, int64(512<<10), observed.Load()&(1<<32-1), "a tighter route limit shrinks the reservation")
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyBudget_ChunkedBodyIsReadAfterAuthAndRejectedWith503(t *testing.T) {
	budget := &RequestBodyBudget{max: 100 << 10, minBody: 1 << 10}
	br := newBudgetRouter(budget, 1<<20, 0)

	rec := br.do(chunkedRequest("/v1/responses", strings.Repeat("x", 50<<10)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(50<<10), br.seenBody.Load(), "handler sees the whole buffered body")
	require.Equal(t, int64(0), budget.Used())

	rec = br.do(chunkedRequest("/v1/responses", strings.Repeat("x", 300<<10)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "10", rec.Header().Get("Retry-After"))
	require.Equal(t, int64(0), budget.Used())

	// 预算基本占满时，小于 minBody 的 chunked 请求照常通过。
	require.True(t, budget.reserve(100<<10-10))
	rec = br.do(chunkedRequest("/v1/responses", strings.Repeat("x", 512)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = br.do(chunkedRequest("/v1/responses", strings.Repeat("x", 64<<10)))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	budget.release(100<<10 - 10)
	require.Equal(t, int64(0), budget.Used())
}

// chunked 请求体按缓冲区实际分配的容量预占，而不只是已读字节。
func TestRequestBodyBudget_ChunkedChargesBufferCapacity(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 0}
	res := &requestBodyReservation{budget: budget}
	body, err, admitted := readBudgetedBody(strings.NewReader(strings.Repeat("x", 300<<10)), res)
	require.NoError(t, err)
	require.True(t, admitted)
	require.Len(t, body, 300<<10)
	require.GreaterOrEqual(t, res.total(), int64(cap(body)), "reservation must cover the allocated buffer")
	res.releaseAll()
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyBudget_EncodedBodyReservesDecodedEstimate(t *testing.T) {
	budget := &RequestBodyBudget{max: 100 << 20, minBody: 100}
	br := newBudgetRouter(budget, 1<<20, 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 1000)))
	req.Header.Set("Content-Encoding", "zstd")
	held := br.holdAsync(t, req)
	require.Equal(t, int64(1000+8*1000), budget.Used(), "compressed size plus the decoded estimate")
	close(br.release)
	<-held
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyBudget_NilBudgetPassesThrough(t *testing.T) {
	br := newBudgetRouter(nil, 1<<20, 0)
	require.Equal(t, http.StatusOK, br.do(chunkedRequest("/v1/responses", "hello")).Code)
	rec := br.do(httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
}

// 压测：大量并发大请求时，同时被放进来的请求体总量不超过预算，超出的立刻 503；进程堆的增长也有上界。
func TestRequestBodyBudget_ConcurrentLargeRequestsStayWithinBudget(t *testing.T) {
	const (
		bodySize = 8 << 20
		budgetSz = 32 << 20
		clients  = 32
	)
	budget := &RequestBodyBudget{max: budgetSz, minBody: 1 << 20}
	gin.SetMode(gin.TestMode)
	release := make(chan struct{})
	entered := make(chan struct{}, clients)
	var admitted, peakUsed atomic.Int64
	r := gin.New()
	r.POST("/v1/responses", RequestBodyLimit(64<<20), RequestBodyBudgetMiddleware(budget), func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		admitted.Add(1)
		for used := budget.Used(); used > peakUsed.Load(); used = budget.Used() {
			peakUsed.Store(used)
		}
		entered <- struct{}{}
		<-release
		c.String(http.StatusOK, "%d", len(body))
	})

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var wg sync.WaitGroup
	codes := make(chan int, clients)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 请求体按需生成，不在测试里预先占内存。
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", io.LimitReader(zeroReader{}, bodySize))
			req.ContentLength = bodySize
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}
	// 等所有被拒的请求返回、被放进来的都进到 handler。
	rejected, inHandler := 0, 0
	for rejected+inHandler < clients {
		select {
		case code := <-codes:
			require.Equal(t, http.StatusServiceUnavailable, code)
			rejected++
		case <-entered:
			inHandler++
		}
	}

	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	require.Equal(t, int64(budgetSz/bodySize), admitted.Load(), "exactly budget/bodySize requests are admitted")
	require.LessOrEqual(t, peakUsed.Load(), int64(budgetSz))
	require.Less(t, int64(during.HeapAlloc)-int64(before.HeapAlloc), int64(3*budgetSz),
		"heap growth is bounded by the budget, not by the %d MiB offered", clients*bodySize>>20)

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		require.Equal(t, http.StatusOK, code)
	}
	require.Equal(t, int64(0), budget.Used())
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
