package middleware

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/memlimit"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// requestBodyBudgetMemoryDivisor：自动预算取内存上限的 1/5。
// 10-26 sv-1 实测在途请求体每 1 字节约占 5–6 字节进程内存（几份拷贝加 GC 余量）。
const requestBodyBudgetMemoryDivisor = 5

// requestBodyBudgetRetryAfterSeconds 是拒绝时建议客户端等待的秒数。
const requestBodyBudgetRetryAfterSeconds = 10

const requestBodyBudgetMessage = "Server is busy processing other large requests, please retry later"

// errRequestBodyBudgetExceeded 是未知长度（chunked）请求体读到一半超出预算时返回的读错误。
var errRequestBodyBudgetExceeded = errors.New("request body budget exceeded")

// RequestBodyBudget 是全局在途请求体预算：限制同时在处理的大请求体总字节数。
// 零值不可用，用 NewRequestBodyBudget 构造；nil 表示不限制。
type RequestBodyBudget struct {
	max     int64
	minBody int64
	used    atomic.Int64
}

// NewRequestBodyBudget 按配置构造预算；memoryLimit 是 Go 运行时内存上限，用于自动取值。
// 返回 nil 表示不启用。
func NewRequestBodyBudget(cfg config.RequestBodyBudgetConfig, memoryLimit int64) *RequestBodyBudget {
	limit := cfg.MaxInflightBytes
	switch {
	case limit < 0:
		return nil
	case limit == 0:
		if !memlimit.IsLimited(memoryLimit) {
			return nil
		}
		limit = memoryLimit / requestBodyBudgetMemoryDivisor
	}
	if limit <= 0 {
		return nil
	}
	return &RequestBodyBudget{max: limit, minBody: max(cfg.MinBodyBytes, 0)}
}

// Max 返回预算上限字节数。
func (b *RequestBodyBudget) Max() int64 { return b.max }

// Used 返回当前已占用的字节数。
func (b *RequestBodyBudget) Used() int64 { return b.used.Load() }

func (b *RequestBodyBudget) reserve(n int64) bool {
	if n <= 0 {
		return true
	}
	if b.used.Add(n) > b.max {
		b.used.Add(-n)
		return false
	}
	return true
}

func (b *RequestBodyBudget) release(n int64) {
	if n > 0 {
		b.used.Add(-n)
	}
}

// RequestBodyLimitWithBudget 在 MaxBytesReader 限制单个请求体大小的基础上，按全局在途预算准入：
// Content-Length 已知且不小于 minBody 的请求先预占，预占不到直接回 503 + Retry-After，不读请求体；
// 未知长度的请求边读边预占，超出预算时读请求体失败。预占在请求结束时释放。budget 为 nil 时
// 行为与 RequestBodyLimit 相同。
func RequestBodyLimitWithBudget(maxBytes int64, budget *RequestBodyBudget) gin.HandlerFunc {
	if budget == nil {
		return RequestBodyLimit(maxBytes)
	}
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		length := c.Request.ContentLength
		switch {
		case c.Request.Body == nil || c.Request.Body == http.NoBody || length == 0:
			c.Next()
		case length > 0:
			if length < budget.minBody || length > maxBytes {
				// 太小的不计入；超过单请求上限的交给 MaxBytesReader 回 413。
				c.Next()
				return
			}
			if !budget.reserve(length) {
				rejectRequestBodyBudget(c, length, budget)
				return
			}
			defer budget.release(length)
			c.Next()
		default:
			body := &budgetedBody{ReadCloser: c.Request.Body, budget: budget}
			c.Request.Body = body
			defer body.releaseAll()
			c.Next()
		}
	}
}

func rejectRequestBodyBudget(c *gin.Context, length int64, budget *RequestBodyBudget) {
	logger.FromContext(c.Request.Context()).Warn("gateway.request_body_budget_rejected",
		zap.String("path", c.Request.URL.Path),
		zap.Int64("content_length", length),
		zap.Int64("budget_used", budget.Used()),
		zap.Int64("budget_max", budget.Max()),
	)
	c.Header("Retry-After", strconv.Itoa(requestBodyBudgetRetryAfterSeconds))
	// 没读请求体就回了错误，让 net/http 在响应后关掉连接，而不是为了复用连接去读完剩下的请求体。
	c.Header("Connection", "close")
	status := http.StatusServiceUnavailable
	path := c.Request.URL.Path
	switch {
	case strings.Contains(path, "/messages"):
		c.JSON(status, gin.H{
			"type":  "error",
			"error": gin.H{"type": "overloaded_error", "message": requestBodyBudgetMessage},
		})
	case strings.HasPrefix(path, "/v1beta/") || strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		c.JSON(status, gin.H{
			"error": gin.H{"code": status, "message": requestBodyBudgetMessage, "status": googleapi.HTTPStatusToGoogleStatus(status)},
		})
	default:
		c.JSON(status, gin.H{
			"error": gin.H{"message": requestBodyBudgetMessage, "type": "server_error", "param": nil, "code": "server_overloaded"},
		})
	}
	c.Abort()
}

// budgetedBody 给未知长度的请求体边读边预占预算。
type budgetedBody struct {
	io.ReadCloser
	budget   *RequestBodyBudget
	reserved int64
	exceeded bool
}

func (b *budgetedBody) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, errRequestBodyBudgetExceeded
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if !b.budget.reserve(int64(n)) {
			b.exceeded = true
			return 0, errRequestBodyBudgetExceeded
		}
		b.reserved += int64(n)
	}
	return n, err
}

func (b *budgetedBody) releaseAll() {
	b.budget.release(b.reserved)
	b.reserved = 0
}
