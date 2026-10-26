package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
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
	// ticks 只给测试注入手动驱动的时钟；nil 时用 time.NewTicker(tick)。
	ticks <-chan time.Time
}

func (s openAIUploadWatchdogSettings) ticker() (<-chan time.Time, func()) {
	if s.ticks != nil {
		return s.ticks, func() {}
	}
	t := time.NewTicker(s.tick)
	return t.C, t.Stop
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
		// 传输层重放请求体时从头计数、重新计时。
		watched.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			return &openAIUploadProgressReader{ReadCloser: body, progress: progress, attempt: progress.restart()}, nil
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
// 传输层 panic 没走到 finish 时也靠它退出）。读完请求体也不退出：传输层可能用 GetBody 重放请求体
// （重定向、连接复用失败重试），重放要接着盯。
func runOpenAIUploadWatchdog(progress *openAIUploadProgress, settings openAIUploadWatchdogSettings, stop, done <-chan struct{}, cancel context.CancelCauseFunc) {
	ticks, stopTicker := settings.ticker()
	defer stopTicker()
	monitor := &openAIUploadMonitor{settings: settings}
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case now := <-ticks:
			if stalled := monitor.observe(now, progress.snapshot()); stalled != nil {
				cancel(stalled)
				return
			}
		}
	}
}

type openAIUploadSample struct {
	at   time.Time
	sent int64
}

// openAIUploadMonitor 是看门狗每个 tick 的判断，不碰真实时钟，便于按给定的时间序列测试。
type openAIUploadMonitor struct {
	settings openAIUploadWatchdogSettings
	attempt  int
	start    time.Time
	samples  []openAIUploadSample
}

// observe 返回非 nil 表示按最近一个窗口的速度，这次上传在 maxUpload 内传不完（或这个窗口一个字节都没传）。
func (m *openAIUploadMonitor) observe(now time.Time, p openAIUploadSnapshot) *openAIUploadStalledError {
	if p.attempt != m.attempt {
		// 传输层重放了请求体：重新计时。
		m.attempt, m.start, m.samples = p.attempt, time.Time{}, nil
	}
	// 还没开始读（等连接、建连、握手的时间不算上传），或者已经读完在等响应头：不判。
	if !p.started || p.done {
		return nil
	}
	if m.start.IsZero() {
		// 从第一次看到开始读请求体的 tick 起计时，和真实开始时间最多差一个 tick（窗口的 1/10）。
		m.start = now
		m.samples = []openAIUploadSample{{at: now, sent: p.sent}}
		return nil
	}
	m.samples = append(m.samples, openAIUploadSample{at: now, sent: p.sent})
	elapsed := now.Sub(m.start)
	if elapsed < m.settings.window {
		return nil
	}
	// 丢掉窗口之前的样本，只留一个刚好落在窗口起点之前的作为基准。
	for len(m.samples) > 2 && now.Sub(m.samples[1].at) >= m.settings.window {
		m.samples = m.samples[1:]
	}
	base := m.samples[0]
	remaining := p.total - p.sent
	if remaining <= 0 {
		return nil
	}
	rate := float64(p.sent-base.sent) / now.Sub(base.at).Seconds()
	if rate > 0 && elapsed+time.Duration(float64(remaining)/rate*float64(time.Second)) <= m.settings.maxUpload {
		return nil
	}
	return &openAIUploadStalledError{sentBytes: p.sent, totalBytes: p.total, elapsed: elapsed, rate: rate}
}

// openAIUploadProgress 记录传输层读请求体的进度。重放会开始新的一轮（attempt 加一）；
// 字段一起加锁读写，看门狗拿到的快照不会一半是上一轮、一半是这一轮。
type openAIUploadProgress struct {
	total int64

	mu      sync.Mutex
	attempt int
	started bool
	sent    int64
	done    bool
}

type openAIUploadSnapshot struct {
	total   int64
	attempt int
	started bool
	sent    int64
	done    bool
}

func (p *openAIUploadProgress) snapshot() openAIUploadSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return openAIUploadSnapshot{total: p.total, attempt: p.attempt, started: p.started, sent: p.sent, done: p.done}
}

// restart 开始新的一轮读请求体，返回这一轮的编号。
func (p *openAIUploadProgress) restart() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempt++
	p.started, p.sent, p.done = false, 0, false
	return p.attempt
}

// record 记下第 attempt 轮读走的字节；上一轮的 reader 晚到的读不算进这一轮。
func (p *openAIUploadProgress) record(attempt int, n int, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if attempt != p.attempt {
		return
	}
	p.started = true
	p.sent += int64(n)
	p.done = p.done || done
}

// openAIUploadProgressReader 记录传输层从请求体里读走了多少字节。h1 写进 socket 缓冲、h2 受流控
// 窗口约束，读走的字节数和真实发出的字节数只差一个缓冲区，足够估算上传速度。
type openAIUploadProgressReader struct {
	io.ReadCloser
	progress *openAIUploadProgress
	attempt  int
}

func (r *openAIUploadProgressReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.progress.record(r.attempt, n, err == io.EOF)
	return n, err
}

func (r *openAIUploadProgressReader) Close() error {
	r.progress.record(r.attempt, 0, true)
	return r.ReadCloser.Close()
}
