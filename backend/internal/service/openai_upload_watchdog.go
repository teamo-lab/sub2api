package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 上游请求体上传看门狗。
//
// 10-26 sv-1：100MB 以上的请求体只能落到 switchbase，单条 h2 流的上传只有约 200KB/s，一条要传十几分钟，
// 有的挂了一个小时。上游响应头超时从请求体发完才开始计时，管不到慢上传；客户端还在等，也不会断开。
// 这些请求一直攥着几份请求体拷贝，十几条就把进程撑到 OOM。
//
// 看门狗按最近一段时间的上传速度估算总上传时间，明显传不完就中止这一跳，按「这个号接不住这么大的
// 请求体」处理：换号、不冷却、不计账号健康；都接不住时回 413。

// ErrOpenAIUpstreamUploadStalled 表示看门狗因为请求体上传太慢中止了这一跳。
var ErrOpenAIUpstreamUploadStalled = errors.New("upstream request body upload too slow")

// openAIUploadStalledFailoverBody 是看门狗中止后换号用的错误体。code 用专门的 request_body_too_large，
// 不用 transport 失败的 upstream_error：按 upstream_error 配置的错误恢复规则会在同一个号上重试，
// 又把大请求体慢慢传一遍。
var openAIUploadStalledFailoverBody = []byte(`{"error":{"type":"invalid_request_error","code":"request_body_too_large","message":"Request payload is too large"}}`)

// openAIUploadStalledError 带上中止时的上传进度，供日志和 ops 记录。
// 它不包装 context.Canceled：调用方会把 context.Canceled 当成客户端断开，不换号。
type openAIUploadStalledError struct {
	sentBytes  int64
	totalBytes int64
	elapsed    time.Duration
	rate       float64
}

func (e *openAIUploadStalledError) Error() string {
	return fmt.Sprintf("%s: sent %d of %d bytes in %s (recent %.0f B/s)",
		ErrOpenAIUpstreamUploadStalled.Error(), e.sentBytes, e.totalBytes, e.elapsed.Round(time.Second), e.rate)
}

func (e *openAIUploadStalledError) Unwrap() error { return ErrOpenAIUpstreamUploadStalled }

type openAIUploadWatchdogSettings struct {
	minBodyBytes int64
	maxUpload    time.Duration
	window       time.Duration
	tick         time.Duration
}

func resolveOpenAIUploadWatchdogSettings(cfg *config.Config) (openAIUploadWatchdogSettings, bool) {
	if cfg == nil {
		return openAIUploadWatchdogSettings{}, false
	}
	w := cfg.Gateway.UpstreamUploadWatchdog
	if !w.Enabled || w.MinBodyBytes <= 0 || w.MaxUploadSeconds <= 0 || w.WindowSeconds <= 0 {
		return openAIUploadWatchdogSettings{}, false
	}
	window := time.Duration(w.WindowSeconds) * time.Second
	tick := window / 10
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	return openAIUploadWatchdogSettings{
		minBodyBytes: w.MinBodyBytes,
		maxUpload:    time.Duration(w.MaxUploadSeconds) * time.Second,
		window:       window,
		tick:         tick,
	}, true
}

// watchOpenAIUpload 给大请求体挂上看门狗。返回的 finish 必须在上游调用返回后调用：它停掉看门狗，
// 并在这一跳是被看门狗中止时把错误换成 *openAIUploadStalledError。不需要看门狗时原样返回。
func watchOpenAIUpload(req *http.Request, settings openAIUploadWatchdogSettings, enabled bool) (*http.Request, func(error) error) {
	passthrough := func(err error) error { return err }
	if !enabled || req == nil || req.Body == nil || req.Body == http.NoBody || req.ContentLength < settings.minBodyBytes {
		return req, passthrough
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	progress := &openAIUploadProgress{total: req.ContentLength}
	watched := req.WithContext(ctx)
	watched.Body = &openAIUploadProgressReader{ReadCloser: req.Body, progress: progress}
	if req.GetBody != nil {
		getBody := req.GetBody
		// 传输层重放请求体时从头计数。
		watched.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			progress.restart()
			return &openAIUploadProgressReader{ReadCloser: body, progress: progress}, nil
		}
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	go runOpenAIUploadWatchdog(progress, settings, stop, ctx.Done(), cancel)
	return watched, func(err error) error {
		stopOnce.Do(func() { close(stop) })
		var stalled *openAIUploadStalledError
		if err != nil && errors.As(context.Cause(ctx), &stalled) {
			return stalled
		}
		return err
	}
}

// runOpenAIUploadWatchdog 一直跑到 finish 关掉 stop，或者这一跳的 context 结束（请求结束、客户端断开；
// 传输层 panic 没走到 finish 时也靠它退出）。
func runOpenAIUploadWatchdog(progress *openAIUploadProgress, settings openAIUploadWatchdogSettings, stop, done <-chan struct{}, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(settings.tick)
	defer ticker.Stop()
	type sample struct {
		at   time.Time
		sent int64
	}
	var samples []sample
	var start time.Time
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case now := <-ticker.C:
			// 读完了也不退出：传输层可能用 GetBody 重放请求体（重定向、连接复用失败重试），
			// 重放会重置进度，需要接着盯。看门狗一直活到上游调用返回、finish 关掉 stop。
			if progress.finished() {
				continue
			}
			// 从传输层第一次读请求体开始计时：等连接、建连、握手的时间不算上传。
			// 传输层重放请求体时 startedAt 会变，重新计时。
			startedAt := progress.startedAt.Load()
			if startedAt == 0 {
				continue
			}
			if begin := time.Unix(0, startedAt); !begin.Equal(start) {
				start = begin
				samples = []sample{{at: start}}
			}
			sent := progress.sent.Load()
			samples = append(samples, sample{at: now, sent: sent})
			elapsed := now.Sub(start)
			if elapsed < settings.window {
				continue
			}
			// 丢掉窗口之前的样本，只留一个刚好落在窗口起点之前的作为基准。
			for len(samples) > 2 && now.Sub(samples[1].at) >= settings.window {
				samples = samples[1:]
			}
			base := samples[0]
			rate := float64(sent-base.sent) / now.Sub(base.at).Seconds()
			remaining := progress.total - sent
			if remaining <= 0 {
				continue
			}
			if rate > 0 && elapsed+time.Duration(float64(remaining)/rate*float64(time.Second)) <= settings.maxUpload {
				continue
			}
			cancel(&openAIUploadStalledError{sentBytes: sent, totalBytes: progress.total, elapsed: elapsed, rate: rate})
			return
		}
	}
}

type openAIUploadProgress struct {
	total     int64
	sent      atomic.Int64
	startedAt atomic.Int64 // 第一次读请求体的 UnixNano，0 表示还没开始
	done      atomic.Bool
}

func (p *openAIUploadProgress) restart() {
	p.sent.Store(0)
	p.startedAt.Store(0)
	p.done.Store(false)
}

func (p *openAIUploadProgress) finished() bool { return p.done.Load() }

// openAIUploadProgressReader 记录传输层从请求体里读走了多少字节。h1 写进 socket 缓冲、h2 受流控
// 窗口约束，读走的字节数和真实发出的字节数只差一个缓冲区，足够估算上传速度。
type openAIUploadProgressReader struct {
	io.ReadCloser
	progress *openAIUploadProgress
}

func (r *openAIUploadProgressReader) Read(p []byte) (int, error) {
	r.progress.startedAt.CompareAndSwap(0, time.Now().UnixNano())
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.progress.sent.Add(int64(n))
	}
	if err == io.EOF {
		r.progress.done.Store(true)
	}
	return n, err
}

func (r *openAIUploadProgressReader) Close() error {
	r.progress.done.Store(true)
	return r.ReadCloser.Close()
}
