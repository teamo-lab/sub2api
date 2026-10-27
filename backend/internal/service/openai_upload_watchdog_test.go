//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func uploadWatchdogTestSettings() openAIUploadWatchdogSettings {
	return openAIUploadWatchdogSettings{
		minBodyBytes: 1024,
		maxUpload:    180 * time.Second,
		window:       30 * time.Second,
		tick:         3 * time.Second,
	}
}

func newUploadWatchdogRequest(t *testing.T, size int) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.test/v1/responses", bytes.NewReader(make([]byte, size)))
	require.NoError(t, err)
	return req
}

// runMonitor 按 rate 字节/秒的速度从 startAt 开始上传 total 字节，每个 tick 喂给 observe，
// 返回第一次判定中止的时刻（相对 0 点），没中止返回 -1。
func runMonitor(total int64, startAt time.Duration, rate float64, until time.Duration) time.Duration {
	settings := uploadWatchdogTestSettings()
	m := &openAIUploadMonitor{settings: settings}
	t0 := time.Unix(1_000_000, 0)
	for at := time.Duration(0); at <= until; at += settings.tick {
		snap := openAIUploadSnapshot{total: total}
		if at >= startAt {
			snap.started = true
			snap.sent = min(total, int64(rate*(at-startAt).Seconds()))
			snap.done = snap.sent >= total
		}
		if m.observe(t0.Add(at), snap) != nil {
			return at
		}
	}
	return -1
}

func TestOpenAIUploadMonitor_AbortsUploadThatCannotFinishInTime(t *testing.T) {
	// 10-26 sv-1 → switchbase：119MB 按约 184KB/s 要传十几分钟。
	at := runMonitor(119<<20, 0, 184<<10, 10*time.Minute)
	require.Equal(t, 30*time.Second, at, "judged on the first tick a full window after the upload was first seen")
}

func TestOpenAIUploadMonitor_LetsUploadThatFinishesInTimeThrough(t *testing.T) {
	// codebot 约 1.7MB/s：100MB 约 60s 传完。
	require.Equal(t, time.Duration(-1), runMonitor(100<<20, 0, 1.7*(1<<20), 10*time.Minute))
}

func TestOpenAIUploadMonitor_WaitBeforeFirstByteDoesNotCount(t *testing.T) {
	// 等连接 5 分钟（远超 180s），之后 1.7MB/s 正常上传，不中止。
	require.Equal(t, time.Duration(-1), runMonitor(100<<20, 5*time.Minute, 1.7*(1<<20), 20*time.Minute))
	// 等完之后慢上传，按开始上传的时间计时。
	require.Equal(t, 5*time.Minute+30*time.Second, runMonitor(119<<20, 5*time.Minute, 184<<10, 20*time.Minute))
}

func TestOpenAIUploadMonitor_NoProgressInWindowAborts(t *testing.T) {
	settings := uploadWatchdogTestSettings()
	m := &openAIUploadMonitor{settings: settings}
	t0 := time.Unix(1_000_000, 0)
	snap := openAIUploadSnapshot{total: 10 << 20, started: true, sent: 1 << 20}
	require.Nil(t, m.observe(t0, snap))
	require.Nil(t, m.observe(t0.Add(29*time.Second), snap))
	stalled := m.observe(t0.Add(30*time.Second), snap)
	require.NotNil(t, stalled)
	require.Zero(t, stalled.rate)
	require.Equal(t, int64(1<<20), stalled.sentBytes)
}

func TestOpenAIUploadMonitor_UsesRecentWindowRate(t *testing.T) {
	settings := uploadWatchdogTestSettings()
	m := &openAIUploadMonitor{settings: settings}
	t0 := time.Unix(1_000_000, 0)
	const total = 200 << 20
	sent := int64(0)
	// 前 60s 很快（2MB/s），之后几乎停住：最近一个窗口的速度决定结果。
	for at := time.Duration(0); at <= 60*time.Second; at += settings.tick {
		sent = int64(2 << 20 * at.Seconds())
		require.Nil(t, m.observe(t0.Add(at), openAIUploadSnapshot{total: total, started: true, sent: sent}), "at %s", at)
	}
	var stalledAt time.Duration
	for at := 63 * time.Second; at <= 120*time.Second; at += settings.tick {
		sent += 10 << 10 * 3
		if m.observe(t0.Add(at), openAIUploadSnapshot{total: total, started: true, sent: sent}) != nil {
			stalledAt = at
			break
		}
	}
	require.NotZero(t, stalledAt, "a stall after a fast start must still be caught")
}

func TestOpenAIUploadMonitor_DoneAndReplay(t *testing.T) {
	settings := uploadWatchdogTestSettings()
	m := &openAIUploadMonitor{settings: settings}
	t0 := time.Unix(1_000_000, 0)
	// 读完了在等响应头：不判。
	for at := time.Duration(0); at <= 10*time.Minute; at += settings.tick {
		require.Nil(t, m.observe(t0.Add(at), openAIUploadSnapshot{total: 10 << 20, started: true, sent: 10 << 20, done: true}))
	}
	// 重放（attempt 变了）：从重放开始重新计时，慢了照样中止。
	base := t0.Add(10 * time.Minute)
	replay := openAIUploadSnapshot{total: 10 << 20, attempt: 1, started: true, sent: 512}
	require.Nil(t, m.observe(base, replay))
	require.Nil(t, m.observe(base.Add(27*time.Second), replay))
	require.NotNil(t, m.observe(base.Add(30*time.Second), replay))
}

func TestOpenAIUploadProgress_SnapshotIsConsistentAcrossReplay(t *testing.T) {
	p := &openAIUploadProgress{total: 100}
	p.record(0, 60, false)
	attempt := p.restart()
	require.Equal(t, openAIUploadSnapshot{total: 100, attempt: 1}, p.snapshot(), "restart resets the whole round at once")
	p.record(0, 40, true) // 上一轮的 reader 晚到的读
	require.Equal(t, openAIUploadSnapshot{total: 100, attempt: 1}, p.snapshot(), "a stale reader does not leak into the new round")
	p.record(attempt, 10, false)
	require.Equal(t, openAIUploadSnapshot{total: 100, attempt: 1, started: true, sent: 10}, p.snapshot())
}

// manualTicks 手动驱动看门狗的 tick。每次发送都要等看门狗处理完上一个 tick 回到 select；
// 看门狗已经取消这一跳时发送不出去，直接报告。
type manualTicks struct {
	ch chan time.Time
	t0 time.Time
}

func newManualTicks() *manualTicks {
	return &manualTicks{ch: make(chan time.Time), t0: time.Unix(1_000_000, 0)}
}

func (m *manualTicks) settings() openAIUploadWatchdogSettings {
	s := uploadWatchdogTestSettings()
	s.ticks = m.ch
	return s
}

// send 发一个 at 时刻的 tick；返回 false 表示看门狗已经取消了这一跳。
func (m *manualTicks) send(ctx context.Context, at time.Duration) bool {
	select {
	case m.ch <- m.t0.Add(at):
		return true
	case <-ctx.Done():
		return false
	}
}

func TestOpenAIUploadWatchdog_AbortsStalledUpload(t *testing.T) {
	ticks := newManualTicks()
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), ticks.settings(), true)
	_, err := req.Body.Read(make([]byte, 512))
	require.NoError(t, err)

	require.True(t, ticks.send(req.Context(), 0))
	require.True(t, ticks.send(req.Context(), 15*time.Second))
	require.True(t, ticks.send(req.Context(), 30*time.Second), "the stalling tick itself is received")
	<-req.Context().Done()

	err = finish(req.Context().Err())
	require.ErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)
	require.NotErrorIs(t, err, context.Canceled, "a stall must not look like a client disconnect")
	var stalled *openAIUploadStalledError
	require.True(t, errors.As(err, &stalled))
	require.Equal(t, int64(512), stalled.sentBytes)
	require.Equal(t, int64(64<<10), stalled.totalBytes)
}

func TestOpenAIUploadWatchdog_CompletedUploadIsNeverCancelled(t *testing.T) {
	ticks := newManualTicks()
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), ticks.settings(), true)
	// 等连接很久不算；之后一口气读完，再等很久响应头也不算。
	for at := time.Duration(0); at <= 10*time.Minute; at += 3 * time.Second {
		require.True(t, ticks.send(req.Context(), at), "waiting to connect must not abort (at %s)", at)
	}
	_, err := io.Copy(io.Discard, req.Body)
	require.NoError(t, err)
	for at := 10 * time.Minute; at <= 20*time.Minute; at += 3 * time.Second {
		require.True(t, ticks.send(req.Context(), at), "waiting for headers must not abort (at %s)", at)
	}
	require.NoError(t, finish(nil))
	require.NoError(t, req.Context().Err(), "the response body is read under this context; finish must not cancel it")
}

func TestOpenAIUploadWatchdog_ReplayedBodyIsWatchedAgain(t *testing.T) {
	ticks := newManualTicks()
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), ticks.settings(), true)
	ctx := req.Context()
	_, err := io.Copy(io.Discard, req.Body)
	require.NoError(t, err)
	require.True(t, ticks.send(ctx, 0))
	require.True(t, ticks.send(ctx, 3*time.Second), "the watchdog keeps running after the first read completes")

	// 传输层重放请求体，这次读了一点就停住。
	replay, err := req.GetBody()
	require.NoError(t, err)
	_, err = replay.Read(make([]byte, 512))
	require.NoError(t, err)
	require.True(t, ticks.send(ctx, 6*time.Second))
	require.True(t, ticks.send(ctx, 36*time.Second))
	<-ctx.Done()
	require.ErrorIs(t, finish(ctx.Err()), ErrOpenAIUpstreamUploadStalled)
}

func TestOpenAIUploadWatchdog_ClientCancelStaysClientCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(parent, http.MethodPost, "https://upstream.test/v1/responses", bytes.NewReader(make([]byte, 64<<10)))
	require.NoError(t, err)
	ticks := newManualTicks()
	watched, finish := watchOpenAIUpload(req, ticks.settings(), true)
	_, err = watched.Body.Read(make([]byte, 512))
	require.NoError(t, err)
	cancel()
	<-watched.Context().Done()

	err = finish(watched.Context().Err())
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)
}

func TestOpenAIUploadWatchdog_ExitsWhenHopContextEndsWithoutFinish(t *testing.T) {
	ticks := newManualTicks()
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		runOpenAIUploadWatchdog(&openAIUploadProgress{total: 1 << 20}, ticks.settings(), make(chan struct{}), done, func(error) {})
		close(exited)
	}()
	close(done)
	// finish 没调（例如传输层 panic）时，这一跳的 context 结束也要让看门狗退出，不泄漏 goroutine。
	<-exited
}

func TestOpenAIUploadWatchdog_SmallBodyOrDisabledIsUntouched(t *testing.T) {
	small := newUploadWatchdogRequest(t, 512)
	got, finish := watchOpenAIUpload(small, uploadWatchdogTestSettings(), true)
	require.Same(t, small, got)
	sentinel := errors.New("boom")
	require.Same(t, sentinel, finish(sentinel))

	big := newUploadWatchdogRequest(t, 64<<10)
	got, _ = watchOpenAIUpload(big, uploadWatchdogTestSettings(), false)
	require.Same(t, big, got)
}

func TestResolveOpenAIUploadWatchdogSettings(t *testing.T) {
	_, ok := resolveOpenAIUploadWatchdogSettings(nil)
	require.False(t, ok)
	cfg := &config.Config{}
	cfg.Gateway.UpstreamUploadWatchdog = config.UpstreamUploadWatchdogConfig{Enabled: true, MinBodyBytes: 8 << 20, MaxUploadSeconds: 180, WindowSeconds: 30}
	settings, ok := resolveOpenAIUploadWatchdogSettings(cfg)
	require.True(t, ok)
	require.Equal(t, 180*time.Second, settings.maxUpload)
	require.Equal(t, 30*time.Second, settings.window)
	require.Equal(t, 3*time.Second, settings.tick)
	cfg.Gateway.UpstreamUploadWatchdog.Enabled = false
	_, ok = resolveOpenAIUploadWatchdogSettings(cfg)
	require.False(t, ok)
}

// stallingUploadUpstream 模拟传输层：读一块请求体后就传不动了，直到这一跳的 context 被取消。
type stallingUploadUpstream struct{}

func (stallingUploadUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if _, err := req.Body.Read(make([]byte, 512)); err != nil {
		return nil, err
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func (u stallingUploadUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// 从 doOpenAIUpstream 到 handleOpenAIUpstreamTransportError：看门狗中止后得到「请求体过大」的
// 换号错误（客户端最终 413），不冷却账号。
func TestOpenAIUploadWatchdog_StallBecomesBodyTooLargeFailoverWithoutEviction(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamUploadWatchdog = config.UpstreamUploadWatchdogConfig{Enabled: true, MinBodyBytes: 1024, MaxUploadSeconds: 1, WindowSeconds: 1}
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: stallingUploadUpstream{}, accountRepo: repo}
	account := &Account{ID: 23, Name: "switchbase", Platform: PlatformOpenAI}
	c, rec := newOpenAITransportErrTestContext()

	// 上传停住：真实时钟下一个 1 秒窗口后判定传不完，上游调用才返回。
	_, err := svc.doOpenAIUpstream(newUploadWatchdogRequest(t, 1<<20), "", account)
	require.ErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)

	retErr := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, err, false)
	var fo *UpstreamFailoverError
	require.True(t, errors.As(retErr, &fo))
	require.True(t, fo.IsOpenAIRequestBodyTooLarge())
	require.Equal(t, NextAccountRetry, fo.NextAccountAction)
	require.Equal(t, http.StatusRequestEntityTooLarge, fo.ClientStatusCode)
	require.False(t, fo.ShouldReportAccountScheduleFailure(), "a slow upload of a huge body is not an account fault")
	require.Equal(t, "request_body_too_large", recoveryFailureErrorCode(fo),
		"must not reuse the transport upstream_error code, or recovery rules retry the same slow account")
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(fo)
	require.False(t, eligible)
	require.Empty(t, repo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 0, rec.Body.Len())
}
