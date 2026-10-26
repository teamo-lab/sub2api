package middleware

import (
	"bytes"
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
//   - Content-Length 已知且不小于 minBody：先预占（超过单请求上限的按上限预占——MaxBytesReader 要读满
//     上限才报错，这期间请求体照样占内存），预占不到直接回 503 + Retry-After，不读请求体。
//   - 未知长度（chunked）：中间件先把请求体读进来，前 minBody 字节不计，之后边读边预占；超出预算时
//     还没写响应，同样回 503。读完后交给后续 handler 的是内存里的副本。
//
// 预占在请求结束时释放。budget 为 nil 时行为与 RequestBodyLimit 相同。
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
			if length < budget.minBody {
				c.Next()
				return
			}
			reserved := min(length, maxBytes)
			if !budget.reserve(reserved) {
				rejectRequestBodyBudget(c, length, budget)
				return
			}
			defer budget.release(reserved)
			c.Next()
		default:
			body, reserved, readErr, admitted := readBudgetedBody(c.Request.Body, budget)
			defer budget.release(reserved)
			if !admitted {
				rejectRequestBodyBudget(c, int64(len(body)), budget)
				return
			}
			if readErr != nil {
				// 交还已读部分和同一个错误，让 handler 按原逻辑回 413/400。
				c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{readErr}))
			} else {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				c.Request.ContentLength = int64(len(body))
			}
			c.Next()
		}
	}
}

// readBudgetedBody 读完未知长度的请求体：前 minBody 字节不计入预算，之后边读边预占。
// admitted 为 false 表示预算不够，此时已预占的字节仍由调用方释放。
func readBudgetedBody(r io.Reader, budget *RequestBodyBudget) (body []byte, reserved int64, readErr error, admitted bool) {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if need := int64(buf.Len()) - budget.minBody - reserved; need > 0 {
				if !budget.reserve(need) {
					return buf.Bytes(), reserved, nil, false
				}
				reserved += need
			}
		}
		if err == io.EOF {
			return buf.Bytes(), reserved, nil, true
		}
		if err != nil {
			return buf.Bytes(), reserved, err, true
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
