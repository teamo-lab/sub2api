package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

// budgetTestRouter 的 /hold 会等 release 关闭才返回，用来让请求一直占着预算。
func budgetTestRouter(budget *RequestBodyBudget, release <-chan struct{}, entered chan<- struct{}) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler := func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusBadRequest, err.Error())
			return
		}
		if c.Query("hold") != "" {
			entered <- struct{}{}
			<-release
		}
		c.String(http.StatusOK, "%d", len(body))
	}
	limit := RequestBodyLimitWithBudget(1<<20, budget)
	r.POST("/v1/responses", limit, handler)
	r.POST("/v1/messages", limit, handler)
	return r
}

func TestRequestBodyLimitWithBudget_RejectsLargeBodyWhenBudgetIsHeld(t *testing.T) {
	budget := &RequestBodyBudget{max: 1000, minBody: 100}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	r := budgetTestRouter(budget, release, entered)

	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses?hold=1", bytes.NewReader(make([]byte, 800))))
		held <- rec
	}()
	<-entered
	require.Equal(t, int64(800), budget.Used())

	// 800 + 300 > 1000：直接 503，带 Retry-After，OpenAI 错误格式。
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 300))))
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

	// Anthropic 路径用 Anthropic 错误格式。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(make([]byte, 300))))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), `"overloaded_error"`)

	// 小于 minBody 的请求不受预算影响。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 50))))
	require.Equal(t, http.StatusOK, rec.Code)

	close(release)
	require.Equal(t, http.StatusOK, (<-held).Code)
	require.Equal(t, int64(0), budget.Used(), "reservation is released when the request ends")

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 300))))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "300", rec.Body.String())
}

func TestRequestBodyLimitWithBudget_ChunkedBodyIsCountedAboveMinWhileStreaming(t *testing.T) {
	budget := &RequestBodyBudget{max: 1000, minBody: 100}
	r := budgetTestRouter(budget, nil, nil)

	newChunked := func(n int) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", io.NopCloser(strings.NewReader(strings.Repeat("x", n))))
		req.ContentLength = -1
		return req
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, newChunked(500))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "500", rec.Body.String())
	require.Equal(t, int64(0), budget.Used())

	// 1500 - 100（不计的部分）> 1000：handler 读请求体时失败。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, newChunked(1500))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), errRequestBodyBudgetExceeded.Error())
	require.Equal(t, int64(0), budget.Used())

	// 预算基本占满时，小于 minBody 的 chunked 请求照常通过，超过的读失败。
	require.True(t, budget.reserve(950))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, newChunked(80))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, newChunked(200))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	budget.release(950)
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyLimitWithBudget_StackedLimitsReserveOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	budget := &RequestBodyBudget{max: 1000, minBody: 100}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	r := gin.New()
	// 和 /v1/embeddings 一样：分组级 bodyLimit 之后再挂一层路由级 textBodyLimit。
	group := r.Group("/v1", RequestBodyLimitWithBudget(1<<20, budget))
	group.POST("/embeddings", RequestBodyLimitWithBudget(512<<10, budget), func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		entered <- struct{}{}
		<-release
		c.Status(http.StatusOK)
	})
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(make([]byte, 600))))
		done <- rec.Code
	}()
	<-entered
	require.Equal(t, int64(600), budget.Used(), "a request passing two body limits is charged once")
	close(release)
	require.Equal(t, http.StatusOK, <-done)
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyLimitWithBudget_DeclaredOversizedBodyStillReservesUpToLimit(t *testing.T) {
	// 路由的单请求上限是 1MiB；声明 2MiB 的请求要按 1MiB 预占，预算不够就直接 503。
	tight := &RequestBodyBudget{max: 1<<20 - 1, minBody: 100}
	rec := httptest.NewRecorder()
	budgetTestRouter(tight, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// 预算够时交给 MaxBytesReader 拒绝，结束后释放。
	roomy := &RequestBodyBudget{max: 3 << 20, minBody: 100}
	rec = httptest.NewRecorder()
	budgetTestRouter(roomy, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
	require.Equal(t, int64(0), roomy.Used())
}

func TestRequestBodyLimitWithBudget_NilBudgetKeepsPlainLimit(t *testing.T) {
	r := budgetTestRouter(nil, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
}
