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

// 全局在途请求体预算：限制同时在处理的大请求体总字节数，给进程内存一个硬性的总量上界。
//
// 每个在途请求的请求体在网关里会被复制好几份（原文落盘、改写、每一跳的上游请求体），10-26 sv-1 实测
// 每 1 字节请求体约占 5–6 字节进程内存。上传看门狗只管单个请求挂住；一批并发大请求各自都传得动时，
// 只有这个总量上限能防 OOM。
//
// 放在 API Key 鉴权之后：未鉴权的请求碰不到预算，也不会在这里被读请求体。

const (
	// requestBodyBudgetMemoryDivisor：自动预算取内存上限的 1/10。按 5–6 倍放大，预算打满时请求体约用掉
	// 软上限的一半多，剩下的留给进程基线（sv-1 平时 1–3GB）。平时在途大请求体只有 0.4–0.8GB，不会被误拒。
	requestBodyBudgetMemoryDivisor = 10
	// requestBodyBudgetRetryAfterSeconds 是拒绝时建议客户端等待的秒数。
	requestBodyBudgetRetryAfterSeconds = 10
	// requestBodyBudgetDecodeExpansion：带 Content-Encoding 的请求体，handler 会解压出最多
	// requestBodyBudgetMaxDecodedBytes（与 httputil 的解压上限一致）的明文，按压缩体的 8 倍估算明文。
	requestBodyBudgetDecodeExpansion = 8
	requestBodyBudgetMaxDecodedBytes = 64 << 20

	requestBodyBudgetMessage = "Server is busy processing other large requests, please retry later"

	requestBodyLimitKey       = "request_body_limit_bytes"
	requestBodyReservationKey = "request_body_budget_reservation"
)

// RequestBodyBudget 是全局在途请求体预算。用 NewRequestBodyBudget 构造；nil 表示不限制。
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

// requestBodyReservation 是一个请求占用的预算，放在 gin.Context 里，保证每个请求只计一次。
type requestBodyReservation struct {
	budget *RequestBodyBudget
	// raw 是按请求体预占的字节，decoded 是带 Content-Encoding 时按明文估算追加的字节。
	raw     int64
	decoded int64
	encoded bool
	// shrinkable：只有按 Content-Length 预占（还没读请求体）时，才能按更小的路由级上限缩减。
	// chunked 请求体已经读进内存，缩了就和实际占用对不上。
	shrinkable bool
}

func (r *requestBodyReservation) total() int64 { return r.raw + r.decoded }

// reserveRaw 把按请求体预占的字节调到 raw，带 Content-Encoding 时同步调整明文估算。预算不够时不变。
func (r *requestBodyReservation) reserveRaw(raw int64) bool {
	decoded := int64(0)
	if r.encoded {
		decoded = decodedEstimate(raw)
	}
	need := (raw - r.raw) + (decoded - r.decoded)
	if need > 0 && !r.budget.reserve(need) {
		return false
	}
	if need < 0 {
		r.budget.release(-need)
	}
	r.raw, r.decoded = raw, decoded
	return true
}

func (r *requestBodyReservation) shrinkTo(limit int64) {
	if !r.shrinkable || limit >= r.raw {
		return
	}
	before := r.total()
	r.raw = limit
	if r.encoded {
		r.decoded = decodedEstimate(limit)
	}
	r.budget.release(before - r.total())
}

func (r *requestBodyReservation) releaseAll() {
	r.budget.release(r.total())
	r.raw, r.decoded = 0, 0
}

// noteRequestBodyLimit 记下这一层的请求体上限（取最小值），已有预占时缩到新上限。
func noteRequestBodyLimit(c *gin.Context, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	if prev, ok := c.Get(requestBodyLimitKey); ok {
		if p, _ := prev.(int64); p > 0 && p <= maxBytes {
			return
		}
	}
	c.Set(requestBodyLimitKey, maxBytes)
	if v, ok := c.Get(requestBodyReservationKey); ok {
		if r, _ := v.(*requestBodyReservation); r != nil {
			r.shrinkTo(maxBytes)
		}
	}
}

func requestBodyLimitFromContext(c *gin.Context) int64 {
	if v, ok := c.Get(requestBodyLimitKey); ok {
		if n, _ := v.(int64); n > 0 {
			return n
		}
	}
	return 0
}

func requestBodyEncoded(req *http.Request) bool {
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	return enc != "" && enc != "identity"
}

// decodedEstimate 是压缩请求体解压后明文的估算字节数。
func decodedEstimate(compressed int64) int64 {
	if compressed <= 0 {
		return 0
	}
	return min(compressed*requestBodyBudgetDecodeExpansion, requestBodyBudgetMaxDecodedBytes)
}

// RequestBodyBudgetMiddleware 在 API Key 鉴权之后按全局在途预算准入，预占在请求结束时释放：
//   - Content-Length 已知：不小于 minBody 时预占 min(Content-Length, 当前请求体上限)，带 Content-Encoding
//     的再加上解压明文的估算；预占不到直接回 503 + Retry-After，不读请求体。之后更小的路由级上限会把
//     预占缩下来。
//   - 未知长度（chunked）：在这里把请求体读进内存，前 minBody 字节不计，之后按缓冲区实际分配的容量
//     预占；预算不够时还没写响应，同样回 503。交给后续 handler 的是内存里的副本。
//
// 同一个请求经过多次（分组级和路由级）只计一次。budget 为 nil 时直接放行。
func RequestBodyBudgetMiddleware(budget *RequestBodyBudget) gin.HandlerFunc {
	return func(c *gin.Context) {
		if budget == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		if _, applied := c.Get(requestBodyReservationKey); applied {
			c.Next()
			return
		}
		res := &requestBodyReservation{budget: budget, encoded: requestBodyEncoded(c.Request)}
		c.Set(requestBodyReservationKey, res)
		defer res.releaseAll()

		limit := requestBodyLimitFromContext(c)
		length := c.Request.ContentLength
		switch {
		case length == 0:
		case length > 0:
			if length < budget.minBody {
				break
			}
			raw := length
			if limit > 0 {
				raw = min(raw, limit)
			}
			res.shrinkable = true
			if !res.reserveRaw(raw) {
				rejectRequestBodyBudget(c, length, budget)
				return
			}
		default:
			body, readErr, admitted := readBudgetedBody(c.Request.Body, res)
			if !admitted {
				rejectRequestBodyBudget(c, int64(len(body)), budget)
				return
			}
			if readErr != nil {
				// 交还已读部分和同一个错误（如 MaxBytesError），让 handler 按原逻辑回 413/400。
				c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{readErr}))
			} else {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				c.Request.ContentLength = int64(len(body))
			}
		}
		c.Next()
	}
}

// readBudgetedBody 读完未知长度的请求体，按缓冲区实际分配的容量（不止已读字节）预占，
// 前 minBody 字节不计；带 Content-Encoding 时连同明文估算一起预占。admitted 为 false 表示预算不够。
func readBudgetedBody(r io.Reader, res *requestBodyReservation) (body []byte, readErr error, admitted bool) {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if !res.reserveRaw(max(int64(buf.Cap())-res.budget.minBody, res.raw)) {
				return buf.Bytes(), nil, false
			}
		}
		if err == io.EOF {
			return buf.Bytes(), nil, true
		}
		if err != nil {
			return buf.Bytes(), err, true
		}
	}
}

func rejectRequestBodyBudget(c *gin.Context, length int64, budget *RequestBodyBudget) {
	logger.FromContext(c.Request.Context()).Warn("gateway.request_body_budget_rejected",
		zap.String("path", c.Request.URL.Path),
		zap.Int64("body_bytes", length),
		zap.Int64("budget_used", budget.Used()),
		zap.Int64("budget_max", budget.Max()),
	)
	c.Header("Retry-After", strconv.Itoa(requestBodyBudgetRetryAfterSeconds))
	// 没读完请求体就回了错误，让 net/http 在响应后关掉连接，而不是为了复用连接去读完剩下的请求体。
	c.Header("Connection", "close")
	status := http.StatusServiceUnavailable
	path := c.Request.URL.Path
	switch {
	case strings.Contains(path, "/messages"):
		c.JSON(status, gin.H{
			"type":  "error",
			"error": gin.H{"type": "overloaded_error", "message": requestBodyBudgetMessage},
		})
	case strings.Contains(path, "/v1beta/"):
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
