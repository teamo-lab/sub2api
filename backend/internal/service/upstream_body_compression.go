package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

// 上游请求体压缩（按账号开关）。
//
// 发往上游的请求体是腾讯云出网流量的大头，真实请求体压缩比 3–9 倍。账号 extra 里
// request_body_compression = zstd | gzip 时，不小于 min_bytes 的 JSON 请求体压缩后再发，
// 带 Content-Encoding、按压缩后长度发 Content-Length，其它都不变。没开的账号一个字节都不变。
//
// 只对 API Key 账号生效：OAuth/官方上游不认压缩请求体，开关也只该打在已验证过的中转上
// （Zero 的 new-api、fluxion）。
//
// 兜底：压缩发出去拿到 400/415 且像是「请求体解析失败」时，这一跳原样不压缩再发一次；
// 不压缩那次没再被同样拒绝，就在进程内记住这个号 fallback_ttl 内不再压缩，并打 warn 日志。
//
// 入站侧的请求体预算（#103 RequestBodyBudget）和按账号的请求体上限都在选号之前按原始请求体计，
// 这里只改发往上游的那一份，不影响它们。上传看门狗挂在压缩之后，按实际上线的字节计速。

// AccountExtraRequestBodyCompression 是账号 extra 里的开关键：zstd | gzip，空或其它值为关。
const AccountExtraRequestBodyCompression = "request_body_compression"

const (
	upstreamBodyEncodingZstd     = "zstd"
	upstreamBodyEncodingGzip     = "gzip"
	upstreamBodyEncodingIdentity = "identity"

	// upstreamBodyRejectionPeekBytes 是判断 400/415 是不是解析失败时最多读的错误体字节数。
	upstreamBodyRejectionPeekBytes = 64 << 10
)

// RequestBodyCompression 返回账号配置的上游请求体压缩编码；没开或值不认识时返回空。
func (a *Account) RequestBodyCompression() string {
	if a == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(a.GetExtraString(AccountExtraRequestBodyCompression))) {
	case upstreamBodyEncodingZstd:
		return upstreamBodyEncodingZstd
	case upstreamBodyEncodingGzip:
		return upstreamBodyEncodingGzip
	default:
		return ""
	}
}

type upstreamBodyCompressionSettings struct {
	minBytes    int64
	zstdLevel   int
	gzipLevel   int
	fallbackTTL time.Duration
}

func resolveUpstreamBodyCompressionSettings(cfg *config.Config) upstreamBodyCompressionSettings {
	s := upstreamBodyCompressionSettings{
		minBytes:    config.DefaultUpstreamBodyCompressionMinBytes,
		zstdLevel:   config.DefaultUpstreamBodyCompressionZstdLevel,
		gzipLevel:   config.DefaultUpstreamBodyCompressionGzipLevel,
		fallbackTTL: time.Duration(config.DefaultUpstreamBodyCompressionFallbackTTLSeconds) * time.Second,
	}
	if cfg == nil {
		return s
	}
	c := cfg.Gateway.UpstreamBodyCompression
	if c.MinBytes > 0 {
		s.minBytes = c.MinBytes
	}
	if c.ZstdLevel > 0 {
		s.zstdLevel = c.ZstdLevel
	}
	if c.GzipLevel > 0 {
		s.gzipLevel = c.GzipLevel
	}
	if c.FallbackTTLSeconds > 0 {
		s.fallbackTTL = time.Duration(c.FallbackTTLSeconds) * time.Second
	}
	return s
}

// upstreamBodyCompressionBlocklist 记住压缩被上游拒绝过的账号，TTL 内不再压缩。只在进程内。
type upstreamBodyCompressionBlocklist struct {
	mu    sync.Mutex
	until map[int64]time.Time
	now   func() time.Time
}

func newUpstreamBodyCompressionBlocklist() *upstreamBodyCompressionBlocklist {
	return &upstreamBodyCompressionBlocklist{until: make(map[int64]time.Time), now: time.Now}
}

var defaultUpstreamBodyCompressionBlocklist = newUpstreamBodyCompressionBlocklist()

func (b *upstreamBodyCompressionBlocklist) blocked(accountID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.until[accountID]
	if !ok {
		return false
	}
	if b.now().Before(until) {
		return true
	}
	delete(b.until, accountID)
	return false
}

func (b *upstreamBodyCompressionBlocklist) block(accountID int64, ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until[accountID] = b.now().Add(ttl)
}

// upstreamBodyStats 是一跳上游请求体的大小事实，打在日志里用于测算节省。
type upstreamBodyStats struct {
	Encoding   string
	RawBytes   int64
	SentBytes  int64
	CompressMs float64
}

var (
	zstdEncoderPools sync.Map // map[int]*sync.Pool
	gzipWriterPools  sync.Map // map[int]*sync.Pool
)

func zstdEncoderPool(level int) *sync.Pool {
	if p, ok := zstdEncoderPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	p, _ := zstdEncoderPools.LoadOrStore(level, &sync.Pool{New: func() any {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			return err
		}
		return enc
	}})
	return p.(*sync.Pool)
}

func gzipWriterPool(level int) *sync.Pool {
	if p, ok := gzipWriterPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	p, _ := gzipWriterPools.LoadOrStore(level, &sync.Pool{New: func() any {
		w, err := gzip.NewWriterLevel(nil, level)
		if err != nil {
			return err
		}
		return w
	}})
	return p.(*sync.Pool)
}

// compressUpstreamBody 把 src 按 encoding 压缩进 dst，返回读到的原始字节数。
// contentLength >= 0 时写进 zstd 帧头（单段帧，解压端按实际大小分配窗口）。
func compressUpstreamBody(dst *bytes.Buffer, src io.Reader, encoding string, settings upstreamBodyCompressionSettings, contentLength int64) (int64, error) {
	switch encoding {
	case upstreamBodyEncodingZstd:
		pool := zstdEncoderPool(settings.zstdLevel)
		got := pool.Get()
		enc, ok := got.(*zstd.Encoder)
		if !ok {
			if err, isErr := got.(error); isErr {
				return 0, err
			}
			return 0, errors.New("zstd encoder unavailable")
		}
		enc.ResetContentSize(dst, contentLength)
		n, err := io.Copy(enc, src)
		closeErr := enc.Close()
		enc.Reset(nil)
		pool.Put(enc)
		if err != nil {
			return n, err
		}
		return n, closeErr
	case upstreamBodyEncodingGzip:
		pool := gzipWriterPool(settings.gzipLevel)
		got := pool.Get()
		w, ok := got.(*gzip.Writer)
		if !ok {
			if err, isErr := got.(error); isErr {
				return 0, err
			}
			return 0, errors.New("gzip writer unavailable")
		}
		w.Reset(dst)
		n, err := io.Copy(w, src)
		closeErr := w.Close()
		w.Reset(io.Discard)
		pool.Put(w)
		if err != nil {
			return n, err
		}
		return n, closeErr
	default:
		return 0, fmt.Errorf("unsupported upstream body encoding %q", encoding)
	}
}

// upstreamBodyCompressionEncoding 判断这一跳要不要压缩，要的话返回编码。
func upstreamBodyCompressionEncoding(req *http.Request, account *Account, settings upstreamBodyCompressionSettings, blocklist *upstreamBodyCompressionBlocklist) string {
	if req == nil || account == nil || account.Type != AccountTypeAPIKey {
		return ""
	}
	encoding := account.RequestBodyCompression()
	if encoding == "" {
		return ""
	}
	// 只压缩能重放、长度已知的请求体：兜底重发要从 GetBody 再拿一份原文。
	if req.Body == nil || req.Body == http.NoBody || req.GetBody == nil || req.ContentLength < settings.minBytes {
		return ""
	}
	if strings.TrimSpace(req.Header.Get("Content-Encoding")) != "" {
		return ""
	}
	if !strings.Contains(strings.ToLower(req.Header.Get("Content-Type")), "json") {
		return ""
	}
	if blocklist != nil && blocklist.blocked(account.ID) {
		return ""
	}
	return encoding
}

// buildCompressedUpstreamRequest 返回压缩后的请求副本；压缩失败或压不小时返回 nil，调用方原样发。
func buildCompressedUpstreamRequest(req *http.Request, encoding string, settings upstreamBodyCompressionSettings) (*http.Request, upstreamBodyStats, error) {
	stats := upstreamBodyStats{Encoding: upstreamBodyEncodingIdentity, RawBytes: req.ContentLength, SentBytes: req.ContentLength}
	src, err := req.GetBody()
	if err != nil {
		return nil, stats, err
	}
	defer func() { _ = src.Close() }()

	started := time.Now()
	var buf bytes.Buffer
	buf.Grow(int(req.ContentLength / 4))
	raw, err := compressUpstreamBody(&buf, src, encoding, settings, req.ContentLength)
	if err != nil {
		return nil, stats, err
	}
	if raw != req.ContentLength {
		return nil, stats, fmt.Errorf("request body length %d does not match Content-Length %d", raw, req.ContentLength)
	}
	stats.CompressMs = float64(time.Since(started).Microseconds()) / 1000
	if int64(buf.Len()) >= raw {
		return nil, stats, nil
	}

	compressed := buf.Bytes()
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(compressed))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(compressed)), nil }
	out.ContentLength = int64(len(compressed))
	out.Header.Set("Content-Encoding", encoding)
	if out.Header.Get("Content-Length") != "" {
		out.Header.Set("Content-Length", strconv.Itoa(len(compressed)))
	}
	stats.Encoding = encoding
	stats.SentBytes = out.ContentLength
	return out, stats, nil
}

// upstreamBodyParseRejectionMarkers 是上游把（压缩过的）请求体当坏 JSON 拒掉时错误体里的特征文本。
var upstreamBodyParseRejectionMarkers = []string{
	"invalid json",             // new-api: "invalid JSON request body"
	"could not parse the json", // Volcano / Anthropic 风格
	"failed to parse the request body",
	"cannot parse json",
	"malformed json",
	"invalid character",              // Go encoding/json 读到压缩字节
	"looking for beginning of value", // 同上
	"unexpected end of json",
	"content-encoding",
	"content encoding",
	"unsupported encoding",
	"unsupported media type",
}

// isUpstreamBodyParseRejection 判断压缩请求收到的 400/415 是不是「请求体没解出来」。
// 415 一律算；400 时错误体为空、不是 JSON（代理页面、纯文本），或带解析失败的特征文本才算。
// 正常的业务 400（参数校验、超窗口等）是带 error 的 JSON，不会触发重发。
// complete=false 表示 body 只是错误体的前一段：截断的 JSON 不能算「不是 JSON」，只看特征文本。
func isUpstreamBodyParseRejection(status int, body []byte, complete bool) bool {
	switch status {
	case http.StatusUnsupportedMediaType:
		return true
	case http.StatusBadRequest:
	default:
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if complete && (len(trimmed) == 0 || !json.Valid(trimmed)) {
		return true
	}
	lower := strings.ToLower(string(trimmed))
	for _, marker := range upstreamBodyParseRejectionMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

type upstreamSendFunc func(*http.Request) (*http.Response, error)

// sendUpstreamWithBodyCompression 按账号开关压缩请求体后调用 send；没开、不够大或不符合条件时原样调用。
// 压缩发送被当作坏请求体拒绝时，这一跳不压缩重发一次（见文件头）。
func sendUpstreamWithBodyCompression(req *http.Request, account *Account, cfg *config.Config, send upstreamSendFunc) (*http.Response, error) {
	return sendUpstreamWithBodyCompressionUsing(req, account, resolveUpstreamBodyCompressionSettings(cfg), defaultUpstreamBodyCompressionBlocklist, send)
}

func sendUpstreamWithBodyCompressionUsing(
	req *http.Request,
	account *Account,
	settings upstreamBodyCompressionSettings,
	blocklist *upstreamBodyCompressionBlocklist,
	send upstreamSendFunc,
) (*http.Response, error) {
	encoding := upstreamBodyCompressionEncoding(req, account, settings, blocklist)
	if encoding == "" {
		return send(req)
	}
	ctx := req.Context()
	compressedReq, stats, err := buildCompressedUpstreamRequest(req, encoding, settings)
	if err != nil {
		logger.FromContext(ctx).Warn("gateway.upstream_body_compression_failed",
			zap.String("component", "service.upstream_body_compression"),
			zap.Int64("account_id", account.ID),
			zap.String("encoding", encoding),
			zap.Int64("raw_bytes", req.ContentLength),
			zap.Error(err),
		)
		return send(req)
	}
	if compressedReq == nil {
		logUpstreamBodyCompression(ctx, account, req, stats, 0)
		return send(req)
	}

	resp, err := send(compressedReq)
	if err != nil || resp == nil || (resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnsupportedMediaType) {
		logUpstreamBodyCompression(ctx, account, req, stats, statusOf(resp))
		return resp, err
	}

	peek, complete, restored := peekUpstreamResponseBody(resp, upstreamBodyRejectionPeekBytes)
	resp.Body = restored
	if !isUpstreamBodyParseRejection(resp.StatusCode, peek, complete) {
		logUpstreamBodyCompression(ctx, account, req, stats, resp.StatusCode)
		return resp, nil
	}

	// 压缩请求体被当成坏请求体拒绝：这一跳不压缩重发一次。原请求的 Body 没被读过。
	rejectedStatus := resp.StatusCode
	_ = resp.Body.Close()
	retryResp, retryErr := send(req)
	retryStatus := statusOf(retryResp)
	// 不压缩那次没被同样拒绝，才说明是压缩的问题：TTL 内这个号不再压缩。
	disabled := false
	if retryErr == nil && retryStatus != http.StatusBadRequest && retryStatus != http.StatusUnsupportedMediaType {
		blocklist.block(account.ID, settings.fallbackTTL)
		disabled = true
	}
	logger.FromContext(ctx).Warn("gateway.upstream_body_compression_rejected",
		zap.String("component", "service.upstream_body_compression"),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.String("upstream_path", upstreamURLPath(req)),
		zap.String("encoding", encoding),
		zap.Int64("upstream_body_raw_bytes", stats.RawBytes),
		zap.Int64("rejected_sent_bytes", stats.SentBytes),
		zap.Int("rejected_status", rejectedStatus),
		zap.String("rejected_body", truncateForLog(peek, 512)),
		zap.Int("retry_status", retryStatus),
		zap.Bool("compression_disabled", disabled),
		zap.Duration("disabled_for", settings.fallbackTTL),
		zap.Error(retryErr),
	)
	return retryResp, retryErr
}

func logUpstreamBodyCompression(ctx context.Context, account *Account, req *http.Request, stats upstreamBodyStats, status int) {
	ratio := 0.0
	if stats.RawBytes > 0 {
		ratio = float64(stats.SentBytes) / float64(stats.RawBytes)
	}
	logger.FromContext(ctx).Info("gateway.upstream_body_compression",
		zap.String("component", "service.upstream_body_compression"),
		zap.Int64("account_id", account.ID),
		zap.String("platform", account.Platform),
		zap.String("upstream_path", upstreamURLPath(req)),
		zap.String("upstream_body_encoding", stats.Encoding),
		zap.Int64("upstream_body_raw_bytes", stats.RawBytes),
		zap.Int64("upstream_body_sent_bytes", stats.SentBytes),
		zap.Float64("upstream_body_sent_ratio", ratio),
		zap.Float64("compress_ms", stats.CompressMs),
		zap.Int("status", status),
	)
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func upstreamURLPath(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Path
}

// peekUpstreamResponseBody 读出响应体前 limit 字节，返回这段内容、是否已读到结尾，以及一个能完整读出
// 原响应体的替身。
func peekUpstreamResponseBody(resp *http.Response, limit int64) ([]byte, bool, io.ReadCloser) {
	if resp == nil || resp.Body == nil {
		return nil, true, http.NoBody
	}
	// 多读一个字节，区分「正好 limit 字节」和「还有后续」。
	peek, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	complete := err == nil && int64(len(peek)) <= limit
	var rest io.Reader = resp.Body
	if err != nil {
		rest = &errReader{err: err}
	}
	restored := &peekedReadCloser{Reader: io.MultiReader(bytes.NewReader(peek), rest), closer: resp.Body}
	if int64(len(peek)) > limit {
		peek = peek[:limit]
	}
	return peek, complete, restored
}

type peekedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (p *peekedReadCloser) Close() error { return p.closer.Close() }

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
