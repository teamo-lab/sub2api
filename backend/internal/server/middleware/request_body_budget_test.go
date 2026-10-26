package middleware

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNewRequestBodyBudget_Modes(t *testing.T) {
	require.Nil(t, NewRequestBodyBudget(config.RequestBodyBudgetConfig{MaxInflightBytes: -1}, 256<<20, 10<<30), "negative disables")
	require.Nil(t, NewRequestBodyBudget(config.RequestBodyBudgetConfig{}, 256<<20, math.MaxInt64), "auto without a memory limit disables")
	auto := NewRequestBodyBudget(config.RequestBodyBudgetConfig{MinBodyBytes: 1 << 20}, 256<<20, 10<<30)
	require.NotNil(t, auto)
	require.Equal(t, int64(1<<30), auto.Max(), "auto is a tenth of the memory limit")
	require.Equal(t, int64(256<<20), auto.bodyLimit)
	fixed := NewRequestBodyBudget(config.RequestBodyBudgetConfig{MaxInflightBytes: 123}, 0, math.MaxInt64)
	require.NotNil(t, fixed)
	require.Equal(t, int64(123), fixed.Max())
	require.Equal(t, int64(123), fixed.bodyLimit, "a missing max_body_size still leaves a finite fallback limit")
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

// 声明超过上限的请求按上限预占，且每个请求只计一次（分组级和路由级都挂了预算时）。
func TestRequestBodyBudget_ReservationIsCappedByLimitAndCountedOnce(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 100, bodyLimit: 10 << 20}
	var observed atomic.Int64
	gin.SetMode(gin.TestMode)
	r := gin.New()
	mw := RequestBodyBudgetMiddleware(budget)
	r.POST("/v1/responses", RequestBodyLimit(1<<20), mw, mw, func(c *gin.Context) {
		observed.Store(budget.Used())
		_, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusOK)
	})
	for _, tc := range []struct {
		declared int64
		want     int64
	}{
		{declared: 400 << 10, want: 400 << 10},
		{declared: 2 << 20, want: 1 << 20},
		{declared: math.MaxInt64, want: 1 << 20},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 1000)))
		req.ContentLength = tc.declared
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, tc.want, observed.Load(), "declared %d", tc.declared)
		require.Equal(t, int64(0), budget.Used())
	}
}

// RequestBodyLimitByRoute 按路由模板选上限：预算看到的就是最终上限，不会先按分组的大上限预占。
func TestRequestBodyLimitByRoute_BudgetReservesTheRouteLimit(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 100, bodyLimit: 10 << 20}
	var observed atomic.Int64
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1")
	g.Use(RequestBodyLimitByRoute(4<<20, map[string]int64{"/v1/embeddings": 512 << 10}), RequestBodyBudgetMiddleware(budget))
	handler := func(c *gin.Context) {
		observed.Store(budget.Used())
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	}
	g.POST("/embeddings", handler)
	g.POST("/responses", handler)

	for path, want := range map[string]int64{"/v1/embeddings": 512 << 10, "/v1/responses": 2 << 20} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, 2<<20))))
		require.Equal(t, want, observed.Load(), path)
		require.Equal(t, int64(0), budget.Used())
		if path == "/v1/embeddings" {
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "the route limit is enforced, not just reserved")
		} else {
			require.Equal(t, http.StatusOK, rec.Code)
		}
	}
}

// 前面没有 RequestBodyLimit 时，预算自己补一层上限：声明 MaxInt64 也只按上限预占，读请求体也被截断。
func TestRequestBodyBudget_NoUpstreamLimitFallsBackToBodyLimit(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 100, bodyLimit: 1 << 20}
	require.True(t, budget.reserve(3<<20), "some budget already in use")
	var observed atomic.Int64
	var readErr atomic.Value
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/responses", RequestBodyBudgetMiddleware(budget), func(c *gin.Context) {
		observed.Store(budget.Used())
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			readErr.Store(err)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 1000)))
	req.ContentLength = math.MaxInt64
	r.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, int64(3<<20+1<<20), observed.Load(), "reserves the fallback limit, not the declared length")
	require.Equal(t, int64(3<<20), budget.Used())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, chunkedRequest("/v1/responses", strings.Repeat("x", 2<<20)))
	var maxBytesErr *http.MaxBytesError
	err, _ := readErr.Load().(error)
	require.ErrorAs(t, err, &maxBytesErr, "the fallback limit is enforced on the body too")
	require.Equal(t, int64(3<<20), budget.Used())
	budget.release(3 << 20)
}

// 预占不会溢出，包括预算本身接近 MaxInt64、且已有明文占用的情况。
func TestRequestBodyBudget_HugeReservationsDoNotOverflow(t *testing.T) {
	budget := &RequestBodyBudget{max: 1 << 30}
	require.True(t, budget.reserve(100))
	for _, n := range []int64{math.MaxInt64, math.MaxInt64 - 50, math.MaxInt64 - 100, 1<<30 - 99} {
		require.False(t, budget.reserve(n), "n=%d", n)
		require.Equal(t, int64(100), budget.Used())
	}
	require.True(t, budget.reserve(1<<30-100), "exactly filling the budget still works")
	require.Equal(t, int64(1<<30), budget.Used())
	require.False(t, budget.reserve(1))
	budget.release(1<<30 - 100)

	res := &requestBodyReservation{budget: budget}
	require.False(t, res.reserveRaw(math.MaxInt64))
	require.Equal(t, int64(100), budget.Used())
	budget.release(100)

	budget = &RequestBodyBudget{max: math.MaxInt64}
	require.True(t, budget.reserve(pkghttputil.MaxDecompressedBodySize))
	res = &requestBodyReservation{budget: budget, decoded: pkghttputil.MaxDecompressedBodySize}
	require.False(t, res.reserveRaw(math.MaxInt64))
	require.Equal(t, int64(pkghttputil.MaxDecompressedBodySize), budget.Used())
	require.True(t, res.reserveRaw(math.MaxInt64-pkghttputil.MaxDecompressedBodySize))
	require.Equal(t, int64(math.MaxInt64), budget.Used())
	res.releaseAll()
	require.Zero(t, budget.Used())
}

// GET/HEAD 的 handler 不读请求体：预算既不预占也不读。
func TestRequestBodyBudget_GetAndHeadAreNotTouched(t *testing.T) {
	budget := &RequestBodyBudget{max: 100, minBody: 0, bodyLimit: 100}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var read atomic.Int64
	h := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.GET("/v1/models", RequestBodyBudgetMiddleware(budget), h)
	r.HEAD("/v1/models", RequestBodyBudgetMiddleware(budget), h)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/v1/models", &countingReader{r: strings.NewReader(strings.Repeat("x", 5000)), n: &read})
		req.ContentLength = -1
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, method)
	}
	require.Equal(t, int64(0), read.Load())
	require.Equal(t, int64(0), budget.Used())
}

// HTTP/2 的 503 不带 Connection: close（那会让整条多路复用连接 GOAWAY），HTTP/1.1 带。
func TestRequestBodyBudget_ConnectionCloseOnlyForHTTP1(t *testing.T) {
	budget := &RequestBodyBudget{max: 100, minBody: 0, bodyLimit: 1 << 20}
	br := newBudgetRouter(budget, 1<<20, 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 200)))
	rec := br.do(req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "close", rec.Header().Get("Connection"))

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 200)))
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/2.0", 2, 0
	rec = br.do(req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Empty(t, rec.Header().Get("Connection"))
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

// chunked 请求体：不到 minBody 不计；一过 minBody 就按缓冲区实际分配的完整容量预占。
func TestRequestBodyBudget_ChunkedChargesFullBufferCapacityOnceOverThreshold(t *testing.T) {
	budget := &RequestBodyBudget{max: 10 << 20, minBody: 64 << 10}

	res := &requestBodyReservation{budget: budget}
	body, err, admitted := readBudgetedBody(strings.NewReader(strings.Repeat("x", 60<<10)), res)
	require.NoError(t, err)
	require.True(t, admitted)
	require.Len(t, body, 60<<10)
	require.Equal(t, int64(0), res.total(), "a body that never reaches min_body_bytes is not counted")

	for _, size := range []int{64 << 10, 65 << 10, 300 << 10} {
		res = &requestBodyReservation{budget: budget}
		body, err, admitted = readBudgetedBody(strings.NewReader(strings.Repeat("x", size)), res)
		require.NoError(t, err)
		require.True(t, admitted)
		require.Len(t, body, size)
		require.GreaterOrEqual(t, res.total(), int64(cap(body)),
			"size %d: once over min_body_bytes the whole allocated buffer is charged, with no min discount", size)
		res.releaseAll()
	}
	require.Equal(t, int64(0), budget.Used())
}

// A rejected growth must not allocate the new buffer first: concurrent rejected
// uploads must not each allocate a body larger than the remaining budget.
func TestRequestBodyBudget_ChunkedReservesBeforeGrowing(t *testing.T) {
	budget := &RequestBodyBudget{max: 64 << 10, minBody: 1}
	res := &requestBodyReservation{budget: budget}
	defer res.releaseAll()
	body, err, admitted := readBudgetedBody(io.LimitReader(zeroReader{}, 1<<20), res)
	require.NoError(t, err)
	require.False(t, admitted)
	require.LessOrEqual(t, int64(cap(body)), res.total(), "rejected growth must leave only the previously reserved buffer")
	require.LessOrEqual(t, res.total(), budget.Max())
}

// Real high-ratio gzip bodies exercise the same decoding boundary as gateway
// handlers. Even a tiny compressed body must hold its decoded reservation until
// the handler finishes, and excess clients must be rejected before reading.
func TestRequestBodyBudget_ConcurrentHighRatioCompressedBodies(t *testing.T) {
	const decodedSize, clients = 32 << 20, 4
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, err := io.Copy(zw, io.LimitReader(zeroReader{}, decodedSize))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.Less(t, compressed.Len()*9, 1<<20, "the old 8x estimate exempted this body entirely")

	for _, chunked := range []bool{false, true} {
		t.Run(strconv.FormatBool(chunked), func(t *testing.T) {
			budget := &RequestBodyBudget{max: 64<<20 + 256<<10, minBody: 1 << 20}
			gin.SetMode(gin.TestMode)
			r := gin.New()
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			entered := make(chan int, clients)
			results := make(chan *httptest.ResponseRecorder, clients)
			var read atomic.Int64
			r.POST("/v1/responses", RequestBodyLimit(64<<20), RequestBodyBudgetMiddleware(budget), func(c *gin.Context) {
				body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
				if err != nil {
					c.String(http.StatusBadRequest, "%v", err)
					return
				}
				entered <- len(body)
				<-release
				runtime.KeepAlive(body)
				c.Status(http.StatusOK)
			})
			for range clients {
				go func() {
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", &countingReader{r: bytes.NewReader(compressed.Bytes()), n: &read})
					req.ContentLength = int64(compressed.Len())
					if chunked {
						req.ContentLength = -1
					}
					req.Header.Set("Content-Encoding", "gzip")
					rec := httptest.NewRecorder()
					r.ServeHTTP(rec, req)
					results <- rec
				}()
			}
			admitted, rejected := 0, 0
			for admitted+rejected < clients {
				select {
				case size := <-entered:
					require.Equal(t, decodedSize, size)
					admitted++
				case rec := <-results:
					require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
					require.Equal(t, "10", rec.Header().Get("Retry-After"))
					rejected++
				}
			}
			require.Equal(t, 1, admitted, "only one worst-case decoded body fits")
			require.Equal(t, int64(compressed.Len()), read.Load(), "rejected clients must not read a body")
			require.GreaterOrEqual(t, budget.Used(), int64(64<<20+compressed.Len()))
			require.LessOrEqual(t, budget.Used(), budget.Max())
			unblock()
			for range admitted {
				require.Equal(t, http.StatusOK, (<-results).Code)
			}
			require.Zero(t, budget.Used())
		})
	}
}

// 多个刚过阈值的 chunked 请求并发：每个都按完整容量计，总占用不超过预算，超出的回 503。
func TestRequestBodyBudget_ConcurrentChunkedNearThresholdIsFullyCharged(t *testing.T) {
	const minBody = 64 << 10
	budget := &RequestBodyBudget{max: 4 * 128 << 10, minBody: minBody}
	br := newBudgetRouter(budget, 1<<20, 0)
	var wg sync.WaitGroup
	results := make(chan int, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- br.do(chunkedRequest("/v1/responses?hold=1", strings.Repeat("x", minBody+1))).Code
		}()
	}
	admitted, rejected := 0, 0
	for admitted+rejected < 16 {
		select {
		case <-br.entered:
			admitted++
			require.LessOrEqual(t, budget.Used(), budget.Max())
		case code := <-results:
			require.Equal(t, http.StatusServiceUnavailable, code)
			rejected++
		}
	}
	// 刚过 64KiB 的缓冲区容量是 128KiB 级别：按完整容量计，预算只容得下 4 个左右，而不是按
	// 「容量减 64KiB」时的 8 个。
	require.LessOrEqual(t, admitted, 4)
	require.Positive(t, rejected)
	close(br.release)
	wg.Wait()
	require.Equal(t, int64(0), budget.Used())
}

// 预占用 CAS：放不下的大预占失败时不会让 used 临时超额，放得下的小预占不会因此被误拒。
// 先占 80/100；一批 goroutine 不停地预占 30（永远放不下），同时只有一个 goroutine 预占 10 再释放
// （80+10 永远放得下）。旧的「先加再回滚」写法会让小预占看到 110/120 而失败。
func TestRequestBodyBudget_FailedReserveNeverBlocksOneThatFits(t *testing.T) {
	const contenders, attemptsEach = 4, 20_000
	budget := &RequestBodyBudget{max: 100}
	require.True(t, budget.reserve(80))
	var contending sync.WaitGroup
	var overMax, peak atomic.Int64
	for range contenders {
		contending.Add(1)
		go func() {
			defer contending.Done()
			for range attemptsEach {
				if budget.reserve(30) {
					overMax.Add(1)
					budget.release(30)
				}
			}
		}()
	}
	contendersDone := make(chan struct{})
	go func() {
		contending.Wait()
		close(contendersDone)
	}()
	// 观察者和小预占都跑到大预占的 goroutine 做完固定次数为止：结束条件不依赖时钟。
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		for {
			select {
			case <-contendersDone:
				return
			default:
			}
			if used := budget.Used(); used > peak.Load() {
				peak.Store(used)
			}
		}
	}()
	spuriousRejects, attempts := 0, 0
	for done := false; !done; {
		select {
		case <-contendersDone:
			done = true
		default:
		}
		attempts++
		if budget.reserve(10) {
			budget.release(10)
		} else {
			spuriousRejects++
		}
	}
	<-observerDone
	require.Positive(t, attempts)
	require.Zero(t, overMax.Load(), "80+30 never fits")
	require.Zero(t, spuriousRejects, "a reservation that fits must not fail because a larger one is being rolled back")
	require.LessOrEqual(t, peak.Load(), budget.Max(), "used must never exceed max, even transiently")
	budget.release(80)
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyBudget_EncodedBodyReservesDecodeLimit(t *testing.T) {
	for _, encoding := range []string{"gzip", "x-gzip", "zstd", "deflate", " GZip ", "identity", ""} {
		t.Run(encoding, func(t *testing.T) {
			budget := &RequestBodyBudget{max: 100 << 20, minBody: 100}
			br := newBudgetRouter(budget, 1<<20, 0)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 1000)))
			req.Header.Set("Content-Encoding", encoding)
			held := br.holdAsync(t, req)
			want := int64(1000)
			if encoding != "identity" && encoding != "" {
				want += pkghttputil.MaxDecompressedBodySize
			}
			require.Equal(t, want, budget.Used())
			close(br.release)
			<-held
			require.Zero(t, budget.Used())
		})
	}
}

// 压缩后小于 minBody、解压后可能远大于它的请求体也要计入。
func TestRequestBodyBudget_SmallCompressedBodyIsCountedByDecodeLimit(t *testing.T) {
	budget := &RequestBodyBudget{max: 100 << 20, minBody: 1 << 20}
	br := newBudgetRouter(budget, 64<<20, 0)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 512<<10)))
	req.Header.Set("Content-Encoding", "gzip")
	held := br.holdAsync(t, req)
	require.Equal(t, int64(512<<10+pkghttputil.MaxDecompressedBodySize), budget.Used())
	close(br.release)
	<-held

	plain := newBudgetRouter(budget, 64<<20, 0)
	close(plain.release)
	rec := plain.do(httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 512<<10))))
	require.Equal(t, http.StatusOK, rec.Code)
	<-plain.entered
	require.Equal(t, int64(0), budget.Used(), "an uncompressed body under min_body_bytes is not counted")
}

func TestRequestBodyBudget_NilBudgetPassesThrough(t *testing.T) {
	br := newBudgetRouter(nil, 1<<20, 0)
	require.Equal(t, http.StatusOK, br.do(chunkedRequest("/v1/responses", "hello")).Code)
	rec := br.do(httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
}

// 压测：大量并发大请求时，同时被放进来的请求体总量不超过预算，超出的立刻 503。
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

	// 同时被放进来、读完请求体的只有 budget/bodySize 个，其余的没读请求体就被拒了：
	// 驻留的请求体总量不超过预算，而不是 clients*bodySize。
	require.Equal(t, int64(budgetSz/bodySize), admitted.Load(), "exactly budget/bodySize requests are admitted")
	require.LessOrEqual(t, peakUsed.Load(), int64(budgetSz))

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
