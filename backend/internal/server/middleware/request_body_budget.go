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
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/memlimit"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 全局在途请求体预算：限制同时在处理的大请求体总字节数，为请求体驻留设置总量上界。
//
// 每个在途请求的请求体在网关里会被复制好几份（原文落盘、改写、每一跳的上游请求体），10-26 sv-1 实测
// 每 1 字节请求体约占 5–6 字节进程内存。上传看门狗只管单个请求挂住；一批并发大请求各自都传得动时，
// 只有这个总量上限能防 OOM。
//
// 放在 API Key 鉴权之后：未鉴权的请求碰不到预算，也不会在这里被读请求体。
//
// 每个预占都有一个有限的上界：按前面 RequestBodyLimit 记下的上限截断；前面没有上限时，这里按
// gateway.max_body_size 补一层 MaxBytesReader。原始体积按该上限截断预占，无论客户端声明多大的
// Content-Length 都不会溢出。
//
// 带 Content-Encoding 的请求体在这里读进内存并解压，按压缩体和解出来的明文各自实际分配的缓冲区预占：
// 压缩比没有上界，按估算或按解压上限一刀切都不对（Codex CLI 默认每个请求都用 zstd 压缩）。

const (
	// requestBodyBudgetMemoryDivisor：自动预算取内存上限的 1/10。按 5–6 倍放大，预算打满时请求体约用掉
	// 软上限的一半多，给进程基线及其他分配留出余量；这不是宿主机合计 RSS 的硬限制。
	requestBodyBudgetMemoryDivisor = 10
	// requestBodyBudgetRetryAfterSeconds 是拒绝时建议客户端等待的秒数。
	requestBodyBudgetRetryAfterSeconds = 10
	requestBodyBudgetMessage           = "Server is busy processing other large requests, please retry later"

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
	// raw 按请求体（压缩体）的缓冲区预占，decoded 按解压出来的明文缓冲区预占，都只增不减。
	raw     int64
	decoded int64
}

func (r *requestBodyReservation) total() int64 { return r.raw + r.decoded }

// reserveRaw 把按请求体预占的字节增加到 raw（只增不减：已知长度只预占一次，缓冲区只会变大）。
// 每次只预占增量，reserve 用减法比较，不会溢出。预算不够时不变。
func (r *requestBodyReservation) reserveRaw(raw int64) bool {
	return r.grow(&r.raw, raw)
}

// reserveDecoded 把按明文预占的字节增加到 decoded，规则同 reserveRaw。
func (r *requestBodyReservation) reserveDecoded(decoded int64) bool {
	return r.grow(&r.decoded, decoded)
}

func (r *requestBodyReservation) grow(field *int64, to int64) bool {
	if to <= *field {
		return true
	}
	if !r.budget.reserve(to - *field) {
		return false
	}
	*field = to
	return true
}

func (r *requestBodyReservation) releaseDecoded() {
	r.budget.release(r.decoded)
	r.decoded = 0
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

// RequestBodyBudgetMiddleware 在 API Key 鉴权之后按全局在途预算准入，预占在请求结束时释放：
//   - 请求体上限：取前面 RequestBodyLimit 记下的上限；没有时在这里按 gateway.max_body_size 补一层
//     MaxBytesReader。预占和读入的字节都不会超过它。
//   - Content-Length 已知、没有压缩：不小于 minBody 时预占 min(Content-Length, 上限)；预占不到直接回
//     503 + Retry-After，不读请求体。
//   - 未知长度（chunked）或带 Content-Encoding：在这里把请求体读进内存，每次扩容前按新缓冲区的完整
//     容量预占（读到的字节不到 minBody 时不计）。带 Content-Encoding 的接着用和 handler 相同的解码器
//     （httputil.NewRequestBodyDecoder，明文最多 64MiB）解压，同样扩容前按明文缓冲区容量预占；
//     「压缩体 + 明文」一过 minBody，两者都计入。预算不够时还没写响应，回 503。
//     解压成功时交给 handler 的是明文（去掉 Content-Encoding）；解不开或编码不支持时原样交还压缩体和
//     请求头，handler 按原逻辑报错。
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
		res := &requestBodyReservation{budget: budget}
		c.Set(requestBodyReservationKey, res)
		defer res.releaseAll()

		encoding := requestBodyEncoding(req)
		length := req.ContentLength
		if length == 0 {
			c.Next()
			return
		}
		if length > 0 {
			// 已知长度：先按声明长度准入，放不下就不读请求体。
			raw := min(length, limit)
			if raw >= budget.minBody && !res.reserveRaw(raw) {
				rejectRequestBodyBudget(c, length, budget)
				return
			}
			if encoding == "" {
				c.Next()
				return
			}
		}

		counted := func(n int) bool { return int64(n) >= budget.minBody }
		endRead := requestprofile.Start(req.Context(), "body_read")
		body, readErr, admitted := readBudgeted(req.Body, initialBodyCap(length), counted, res.reserveRaw)
		endRead()
		if !admitted {
			rejectRequestBodyBudget(c, int64(len(body)), budget)
			return
		}
		if readErr != nil {
			// 交还已读部分和同一个错误（如 MaxBytesError），让 handler 按原逻辑回 413/400。
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{readErr}))
			c.Next()
			return
		}
		if encoding != "" {
			endDecode := requestprofile.Start(req.Context(), "body_decompress")
			decoded, decodeErr, admitted := decodeBudgeted(encoding, body, budget.minBody, res)
			endDecode()
			if !admitted {
				rejectRequestBodyBudget(c, int64(len(body)), budget)
				return
			}
			if decodeErr == nil {
				req.Body = io.NopCloser(bytes.NewReader(decoded))
				req.ContentLength = int64(len(decoded))
				req.Header.Del("Content-Encoding")
				req.Header.Del("Content-Length")
				c.Next()
				return
			}
			// 解不开：明文缓冲区丢掉，压缩体和请求头原样交给 handler。
			res.releaseDecoded()
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		c.Next()
	}
}

// requestBodyEncoding 返回需要解压的 Content-Encoding（小写），没有或 identity 时返回空串。
func requestBodyEncoding(req *http.Request) string {
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc == "identity" {
		return ""
	}
	return enc
}

// initialBodyCap 是读请求体时缓冲区的初始容量：已知长度时按它（最多 1MiB，和 handler 的预分配一致），
// 否则从零开始。超出的部分在扩容时预占。
func initialBodyCap(length int64) int {
	if length <= 0 {
		return 0
	}
	return int(min(length, 1<<20))
}

// readBudgeted 把 r 读进内存。每次扩容前先按新缓冲区的完整容量调用 reserve（counted 为 false 时不调），
// 预算不够就停下、不分配新缓冲区，admitted 返回 false。读出错时返回已读部分和错误。
func readBudgeted(r io.Reader, initialCap int, counted func(n int) bool, reserve func(capacity int64) bool) (body []byte, readErr error, admitted bool) {
	if initialCap > 0 && counted(initialCap) && !reserve(int64(initialCap)) {
		return nil, nil, false
	}
	body = make([]byte, 0, initialCap)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			newLen := len(body) + n
			newCap := cap(body)
			if newLen > newCap {
				newCap = max(newLen, 2*newCap)
			}
			if counted(newLen) && !reserve(int64(newCap)) {
				return body, nil, false
			}
			if newCap > cap(body) {
				grown := make([]byte, len(body), newCap)
				copy(grown, body)
				body = grown
			}
			body = append(body, chunk[:n]...)
		}
		if err == io.EOF {
			return body, nil, true
		}
		if err != nil {
			return body, err, true
		}
	}
}

// decodeBudgeted 用 handler 的同一个解码器解压 raw，按明文缓冲区容量预占。「压缩体 + 明文」一过 minBody，
// 压缩体的缓冲区也一起计入（它小于 minBody 时读的时候没计）。decodeErr 非 nil 表示解不开或编码不支持。
func decodeBudgeted(encoding string, raw []byte, minBody int64, res *requestBodyReservation) (decoded []byte, decodeErr error, admitted bool) {
	dec, err := pkghttputil.NewRequestBodyDecoder(encoding, bytes.NewReader(raw))
	if err != nil {
		return nil, err, true
	}
	defer func() { _ = dec.Close() }()
	counted := func(n int) bool { return int64(len(raw)+n) >= minBody }
	reserve := func(capacity int64) bool {
		return res.reserveRaw(int64(cap(raw))) && res.reserveDecoded(capacity)
	}
	return readBudgeted(dec, 0, counted, reserve)
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
