// Package rawcapture 把网关请求和响应原文异步写到 S3 兼容对象存储（腾讯云 COS 等）。
//
// 设计约束：请求路径上只做一次非阻塞入队；队列按条数和字节数双重限额，满了直接丢弃并计数，
// 绝不拖慢或阻塞请求。压缩、序列化和上传都在后台 worker 里完成。对象过期由桶的生命周期规则负责。
package rawcapture

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Options 是 Capturer 的运行参数，由 config.RawCaptureConfig 映射而来。
type Options struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	QueueSize       int
	MaxQueueBytes   int64
	Workers         int
	UploadTimeout   time.Duration
}

// Record 是一次请求的原文和元数据。Body 在入队后归 Capturer 所有，调用方不得再修改。
type Record struct {
	ClientRequestID string            `json:"client_request_id"`
	StartedAt       time.Time         `json:"started_at"`
	DurationMs      int64             `json:"duration_ms"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Status          int               `json:"status"`
	APIKeyID        int64             `json:"api_key_id,omitempty"`
	UserID          int64             `json:"user_id,omitempty"`
	GroupID         int64             `json:"group_id,omitempty"`
	Model           string            `json:"model,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	BodyBytes       int               `json:"body_bytes"`
	BodyTruncated   bool              `json:"body_truncated,omitempty"`
	Body            []byte            `json:"-"`
	// ResponseHeaders 是白名单内的响应头；Response 是写给客户端的原始字节（SSE 即事件流文本）。
	// 超过上限时只保留开头和结尾各一半，ResponseTruncated=true，ResponseBytes 是实际总字节数。
	ResponseHeaders   map[string]string `json:"response_headers,omitempty"`
	ResponseBytes     int64             `json:"response_bytes"`
	ResponseTruncated bool              `json:"response_truncated,omitempty"`
	Response          []byte            `json:"-"`
	Extra             map[string]any    `json:"extra,omitempty"`
}

// Stats 是累计计数，用于日志和排障。
type Stats struct {
	Enqueued      uint64
	DroppedFull   uint64
	Uploaded      uint64
	UploadFailed  uint64
	QueuedBytes   int64
	UploadedBytes uint64
}

type putter interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Capturer 持有后台队列和上传 worker。零值不可用，用 New 构造。
type Capturer struct {
	opts        Options
	client      putter
	queue       chan *Record
	queuedBytes atomic.Int64

	enqueued      atomic.Uint64
	droppedFull   atomic.Uint64
	uploaded      atomic.Uint64
	uploadFailed  atomic.Uint64
	uploadedBytes atomic.Uint64

	onError func(rec *Record, err error)
	// mu 保护 closed 和 queue 的关闭：Enqueue 持读锁做非阻塞发送，Close 持写锁关闭队列。
	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// New 构造 Capturer 并启动 worker。onError 可为 nil，会在 worker goroutine 里调用。
func New(ctx context.Context, opts Options, onError func(rec *Record, err error)) (*Capturer, error) {
	opts = normalizeOptions(opts)
	if opts.Bucket == "" {
		return nil, fmt.Errorf("rawcapture: bucket is required")
	}
	client, err := newS3Client(ctx, opts)
	if err != nil {
		return nil, err
	}
	return newWithClient(opts, client, onError), nil
}

func newWithClient(opts Options, client putter, onError func(rec *Record, err error)) *Capturer {
	opts = normalizeOptions(opts)
	runCtx, cancel := context.WithCancel(context.Background())
	c := &Capturer{
		opts:    opts,
		client:  client,
		queue:   make(chan *Record, opts.QueueSize),
		onError: onError,
		cancel:  cancel,
	}
	for i := 0; i < opts.Workers; i++ {
		c.wg.Add(1)
		go c.worker(runCtx)
	}
	return c
}

func normalizeOptions(o Options) Options {
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.MaxQueueBytes <= 0 {
		o.MaxQueueBytes = 512 << 20
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.UploadTimeout <= 0 {
		o.UploadTimeout = 60 * time.Second
	}
	o.Prefix = strings.Trim(strings.TrimSpace(o.Prefix), "/")
	return o
}

// Enqueue 非阻塞入队。队列条数或字节数超限时丢弃并返回 false。
func (c *Capturer) Enqueue(rec *Record) bool {
	if c == nil || rec == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return false
	}
	size := rec.payloadBytes()
	if c.queuedBytes.Add(size) > c.opts.MaxQueueBytes {
		c.queuedBytes.Add(-size)
		c.droppedFull.Add(1)
		return false
	}
	select {
	case c.queue <- rec:
		c.enqueued.Add(1)
		return true
	default:
		c.queuedBytes.Add(-size)
		c.droppedFull.Add(1)
		return false
	}
}

// Stats 返回当前计数快照。
func (c *Capturer) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Enqueued:      c.enqueued.Load(),
		DroppedFull:   c.droppedFull.Load(),
		Uploaded:      c.uploaded.Load(),
		UploadFailed:  c.uploadFailed.Load(),
		QueuedBytes:   c.queuedBytes.Load(),
		UploadedBytes: c.uploadedBytes.Load(),
	}
}

// Close 停止接收新记录，等待队列里已有的记录上传完或 ctx 到期。
func (c *Capturer) Close(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		c.cancel()
		<-done
	}
}

func (c *Capturer) worker(ctx context.Context) {
	defer c.wg.Done()
	for rec := range c.queue {
		c.upload(ctx, rec)
		c.queuedBytes.Add(-rec.payloadBytes())
	}
}

func (c *Capturer) upload(ctx context.Context, rec *Record) {
	payload, err := encodeRecord(rec)
	if err == nil {
		upCtx, cancel := context.WithTimeout(ctx, c.opts.UploadTimeout)
		_, err = c.client.PutObject(upCtx, &s3.PutObjectInput{
			Bucket:          aws.String(c.opts.Bucket),
			Key:             aws.String(ObjectKey(c.opts.Prefix, rec)),
			Body:            bytes.NewReader(payload),
			ContentLength:   aws.Int64(int64(len(payload))),
			ContentType:     aws.String("application/json"),
			ContentEncoding: aws.String("gzip"),
		})
		cancel()
	}
	if err != nil {
		c.uploadFailed.Add(1)
		if c.onError != nil {
			c.onError(rec, err)
		}
		return
	}
	c.uploaded.Add(1)
	c.uploadedBytes.Add(uint64(len(payload)))
}

func (r *Record) payloadBytes() int64 { return int64(len(r.Body) + len(r.Response)) }

// ObjectKey 返回记录的对象路径：<prefix>/<client_request_id>.json.gz。
// 不按时间分目录，拿到 ID（例如从 new-api 的 logs.upstream_request_id）就能直接定位对象。
// 没有 ID 的记录落到 <prefix>/unknown/<UTC 日期>/<纳秒时间戳>.json.gz。
func ObjectKey(prefix string, rec *Record) string {
	key := sanitizeKeySegment(rec.ClientRequestID) + ".json.gz"
	if key == ".json.gz" {
		key = fmt.Sprintf("unknown/%s/%d.json.gz", rec.StartedAt.UTC().Format("2006-01-02"), rec.StartedAt.UnixNano())
	}
	if prefix != "" {
		key = prefix + "/" + key
	}
	return key
}

func sanitizeKeySegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

type envelope struct {
	*Record
	BodyEncoding     string          `json:"body_encoding"`
	BodyJSON         json.RawMessage `json:"body,omitempty"`
	BodyBase64       string          `json:"body_base64,omitempty"`
	ResponseEncoding string          `json:"response_encoding"`
	ResponseJSON     json.RawMessage `json:"response,omitempty"`
	ResponseText     string          `json:"response_text,omitempty"`
	ResponseBase64   string          `json:"response_base64,omitempty"`
}

// encodeRecord 把记录写成 gzip 压缩的 JSON。合法 JSON 的请求体原样嵌入，否则 base64。
func encodeRecord(rec *Record) ([]byte, error) {
	env := envelope{Record: rec}
	if len(rec.Body) > 0 && json.Valid(rec.Body) {
		env.BodyEncoding = "json"
		env.BodyJSON = rec.Body
	} else if len(rec.Body) > 0 {
		env.BodyEncoding = "base64"
		env.BodyBase64 = base64.StdEncoding.EncodeToString(rec.Body)
	} else {
		env.BodyEncoding = "none"
	}
	// 未截断的 JSON 响应原样嵌入；SSE 等文本存成字符串；其余（如压缩字节）base64。
	switch {
	case len(rec.Response) == 0:
		env.ResponseEncoding = "none"
	case !rec.ResponseTruncated && json.Valid(rec.Response):
		env.ResponseEncoding = "json"
		env.ResponseJSON = rec.Response
	case utf8.Valid(rec.Response):
		env.ResponseEncoding = "text"
		env.ResponseText = string(rec.Response)
	default:
		env.ResponseEncoding = "base64"
		env.ResponseBase64 = base64.StdEncoding.EncodeToString(rec.Response)
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(gz).Encode(env); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newS3Client(ctx context.Context, o Options) (*s3.Client, error) {
	region := o.Region
	if region == "" {
		region = "auto"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretAccessKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("rawcapture: load aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.ForcePathStyle
		so.APIOptions = append(so.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	}), nil
}
