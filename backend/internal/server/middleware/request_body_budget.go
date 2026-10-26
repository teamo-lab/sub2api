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
//
// 每个预占都有一个有限的上界：按前面 RequestBodyLimit 记下的上限截断；前面没有上限时，这里按
// gateway.max_body_size 补一层 MaxBytesReader。所以无论客户端声明多大的 Content-Length，预占都不会
// 超过单个请求体上限，也不会溢出。

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
	// bodyLimit 是请求在预算之前没经过 RequestBodyLimit 时补上的请求体上限。
	bodyLimit int64
	// used 始终在 [0, max] 之间：只有放得下的预占才会提交，释放的都是之前预占的。
	used atomic.Int64
}

// NewRequestBodyBudget 按配置构造预算；maxBodySize 是网关的单个请求体上限（gateway.max_body_size），
// memoryLimit 是 Go 运行时内存上限，用于自动取值。返回 nil 表示不启用。
func NewRequestBodyBudget(cfg config.RequestBodyBudgetConfig, maxBodySize, memoryLimit int64) *RequestBodyBudget {
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
	if maxBodySize <= 0 {
		maxBodySize = limit
	}
	return &RequestBodyBudget{max: limit, minBody: max(cfg.MinBodyBytes, 0), bodyLimit: maxBodySize}
}

// Max 返回预算上限字节数。
func (b *RequestBodyBudget) Max() int64 { return b.max }

// Used 返回当前已占用的字节数。
func (b *RequestBodyBudget) Used() int64 { return b.used.Load() }

// reserve 只在 used+n 不超过上限时提交（CAS），失败的预占不会让其他请求看到临时的超额。
// 用 n > max-used 比较：used 在 [0, max] 之间，减法不会溢出，n 再大也只是放不下。
func (b *RequestBodyBudget) reserve(n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		used := b.used.Load()
		if n > b.max-used {
			return false
		}
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
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
}

func (r *requestBodyReservation) total() int64 { return r.raw + r.decoded }

// effective 是 raw 字节请求体在内存里的估算占用：带 Content-Encoding 时加上明文估算。
// minBody 阈值按它比较，压缩后很小、解压后很大的请求体也会被计入。
func (r *requestBodyReservation) effective(raw int64) int64 {
	if r.encoded {
		return raw + decodedEstimate(raw)
	}
	return raw
}

// reserveRaw 把按请求体预占的字节增加到 raw（只增不减：已知长度只预占一次，chunked 的缓冲区只会变大），
// 带 Content-Encoding 时同步追加明文估算。预算不够时不变。
func (r *requestBodyReservation) reserveRaw(raw int64) bool {
	if raw <= r.raw {
		return true
	}
	decoded := int64(0)
	if r.encoded {
		decoded = decodedEstimate(raw)
	}
	needRaw, needDecoded := raw-r.raw, decoded-r.decoded
	// needDecoded 不超过明文估算的上限；先把 needRaw 和预算上限比，两者相加就不会溢出。
	if needRaw > r.budget.max || !r.budget.reserve(needRaw+needDecoded) {
		return false
	}
	r.raw, r.decoded = raw, decoded
	return true
}

func (r *requestBodyReservation) releaseAll() {
	r.budget.release(r.total())
	r.raw, r.decoded = 0, 0
}

// noteRequestBodyLimit 记下这一层的请求体上限（多层时取最小值），供预算按它截断预占。
// 网关路由每个请求只经过一层上限，而且都在预算之前（见 RequestBodyLimitByRoute）。
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

// decodedEstimate 是压缩请求体解压后明文的估算字节数，封顶 requestBodyBudgetMaxDecodedBytes。
// 先比较再乘，compressed 再大也不会溢出。
func decodedEstimate(compressed int64) int64 {
	if compressed <= 0 {
		return 0
	}
	if compressed >= requestBodyBudgetMaxDecodedBytes/requestBodyBudgetDecodeExpansion {
		return requestBodyBudgetMaxDecodedBytes
	}
	return compressed * requestBodyBudgetDecodeExpansion
}

// RequestBodyBudgetMiddleware 在 API Key 鉴权之后按全局在途预算准入，预占在请求结束时释放：
//   - 请求体上限：取前面 RequestBodyLimit 记下的上限；没有时在这里按 gateway.max_body_size 补一层
//     MaxBytesReader。预占和读入的字节都不会超过它。
//   - Content-Length 已知：估算占用不小于 minBody 时预占 min(Content-Length, 上限)，带 Content-Encoding
//     的再加上解压明文的估算；预占不到直接回 503 + Retry-After，不读请求体。
//   - 未知长度（chunked）：在这里把请求体读进内存。读到的字节不到 minBody 时不计；一过 minBody 就按
//     缓冲区实际分配的完整容量预占（和已知长度一过阈值就按完整长度计一致）；预算不够时还没写响应，
//     同样回 503。交给后续 handler 的是内存里的副本。
//   - GET/HEAD 的 handler 不读请求体，不经过预算。
//
// 同一个请求经过多次（分组级和路由级）只计一次。budget 为 nil 时直接放行。
func RequestBodyBudgetMiddleware(budget *RequestBodyBudget) gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		if budget == nil || req == nil || req.Body == nil || req.Body == http.NoBody ||
			req.Method == http.MethodGet || req.Method == http.MethodHead {
			c.Next()
			return
		}
		if _, applied := c.Get(requestBodyReservationKey); applied {
			c.Next()
			return
		}
		limit := requestBodyLimitFromContext(c)
		if limit <= 0 {
			limit = budget.bodyLimit
			req.Body = http.MaxBytesReader(c.Writer, req.Body, limit)
			noteRequestBodyLimit(c, limit)
		}
		res := &requestBodyReservation{budget: budget, encoded: requestBodyEncoded(req)}
		c.Set(requestBodyReservationKey, res)
		defer res.releaseAll()

		switch length := req.ContentLength; {
		case length == 0:
		case length > 0:
			raw := min(length, limit)
			if res.effective(raw) >= budget.minBody && !res.reserveRaw(raw) {
				rejectRequestBodyBudget(c, length, budget)
				return
			}
		default:
			body, readErr, admitted := readBudgetedBody(req.Body, res)
			if !admitted {
				rejectRequestBodyBudget(c, int64(len(body)), budget)
				return
			}
			if readErr != nil {
				// 交还已读部分和同一个错误（如 MaxBytesError），让 handler 按原逻辑回 413/400。
				req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{readErr}))
			} else {
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
		}
		c.Next()
	}
}

// readBudgetedBody 读完未知长度的请求体。已读字节不到 minBody 时不计；过了之后按缓冲区实际分配的
// 完整容量（不止已读字节）预占，带 Content-Encoding 时连同明文估算一起预占。admitted 为 false 表示预算不够。
func readBudgetedBody(r io.Reader, res *requestBodyReservation) (body []byte, readErr error, admitted bool) {
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if res.effective(int64(buf.Len())) >= res.budget.minBody && !res.reserveRaw(int64(buf.Cap())) {
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
	// HTTP/1.x 没读完请求体就回了错误：让 net/http 在响应后关掉连接，而不是为了复用连接去读完剩下的
	// 请求体。HTTP/2 只会重置这一条流；在那里设 Connection: close 会让整条多路复用连接 GOAWAY。
	if c.Request.ProtoMajor < 2 {
		c.Header("Connection", "close")
	}
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
