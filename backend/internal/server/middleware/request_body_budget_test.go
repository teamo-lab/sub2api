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
	require.Equal(t, int64(2<<30), auto.Max(), "auto is a fifth of the memory limit")
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

func TestRequestBodyLimitWithBudget_ChunkedBodyIsCountedAndCapped(t *testing.T) {
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
	require.Equal(t, int64(0), budget.Used())

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, newChunked(1500))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), errRequestBodyBudgetExceeded.Error())
	require.Equal(t, int64(0), budget.Used())
}

func TestRequestBodyLimitWithBudget_NilBudgetKeepsPlainLimit(t *testing.T) {
	r := budgetTestRouter(nil, nil, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(make([]byte, 2<<20))))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request body too large")
}
