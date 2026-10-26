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
		maxUpload:    600 * time.Millisecond,
		window:       150 * time.Millisecond,
		tick:         20 * time.Millisecond,
	}
}

// slowUploadUpstream 模拟传输层：先等 delay，再按 chunk/interval 的速度读请求体，读完回 200；
// 这一跳的 context 被取消时像 net/http 一样返回 context 错误。
type slowUploadUpstream struct {
	delay    time.Duration
	chunk    int
	interval time.Duration
	calls    int
}

func (u *slowUploadUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	ctx := req.Context()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(u.delay):
	}
	buf := make([]byte, u.chunk)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(u.interval):
		}
		if _, err := req.Body.Read(buf); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("{}")))}, nil
}

func (u *slowUploadUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func newUploadWatchdogRequest(t *testing.T, size int) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://upstream.test/v1/responses", bytes.NewReader(make([]byte, size)))
	require.NoError(t, err)
	return req
}

func TestOpenAIUploadWatchdog_AbortsUploadThatCannotFinishInTime(t *testing.T) {
	// 64KiB，每 20ms 读 512B ≈ 25KiB/s：要 2.5s 才传完，超过 600ms 的上限。
	upstream := &slowUploadUpstream{chunk: 512, interval: 20 * time.Millisecond}
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), uploadWatchdogTestSettings(), true)

	started := time.Now()
	_, err := upstream.Do(req, "", 1, 1)
	err = finish(err)

	require.ErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)
	require.NotErrorIs(t, err, context.Canceled, "a stall must not look like a client disconnect")
	var stalled *openAIUploadStalledError
	require.True(t, errors.As(err, &stalled))
	require.Greater(t, stalled.sentBytes, int64(0))
	require.Less(t, stalled.sentBytes, int64(64<<10))
	require.Less(t, time.Since(started), 2*time.Second, "watchdog must abort well before the upload would finish")
}

func TestOpenAIUploadWatchdog_LetsUploadThatFinishesInTimeThrough(t *testing.T) {
	// 64KiB，每 5ms 读 4KiB ≈ 800KiB/s：约 80ms 传完。
	upstream := &slowUploadUpstream{chunk: 4 << 10, interval: 5 * time.Millisecond}
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), uploadWatchdogTestSettings(), true)

	resp, err := upstream.Do(req, "", 1, 1)
	require.NoError(t, finish(err))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, req.Context().Err(), "the response body is read under this context; finish must not cancel it")
}

func TestOpenAIUploadWatchdog_WaitBeforeFirstByteDoesNotCount(t *testing.T) {
	// 等连接 400ms（远超 150ms 窗口），之后正常速度上传，不应被中止。
	upstream := &slowUploadUpstream{delay: 400 * time.Millisecond, chunk: 4 << 10, interval: 5 * time.Millisecond}
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), uploadWatchdogTestSettings(), true)

	_, err := upstream.Do(req, "", 1, 1)
	require.NoError(t, finish(err))
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

func TestOpenAIUploadWatchdog_ClientCancelStaysClientCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(parent, http.MethodPost, "https://upstream.test/v1/responses", bytes.NewReader(make([]byte, 64<<10)))
	require.NoError(t, err)
	upstream := &slowUploadUpstream{chunk: 512, interval: 20 * time.Millisecond}
	watched, finish := watchOpenAIUpload(req, uploadWatchdogTestSettings(), true)
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err = upstream.Do(watched, "", 1, 1)
	err = finish(err)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)
}

func TestOpenAIUploadWatchdog_ReplayedBodyIsWatchedAgain(t *testing.T) {
	req, finish := watchOpenAIUpload(newUploadWatchdogRequest(t, 64<<10), uploadWatchdogTestSettings(), true)
	ctx := req.Context()
	// 第一次很快读完，等过几个 tick（看门狗不能因为读完就退出）。
	_, err := io.Copy(io.Discard, req.Body)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	// 传输层重放请求体，这次慢得传不完。
	replay, err := req.GetBody()
	require.NoError(t, err)
	buf := make([]byte, 512)
	var doErr error
	for doErr == nil {
		select {
		case <-ctx.Done():
			doErr = ctx.Err()
		case <-time.After(20 * time.Millisecond):
			if _, err := replay.Read(buf); err == io.EOF {
				doErr = errors.New("replay finished: watchdog did not fire")
			}
		}
	}
	require.ErrorIs(t, finish(doErr), ErrOpenAIUpstreamUploadStalled)
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

// 从 doOpenAIUpstream 到 handleOpenAIUpstreamTransportError：看门狗中止后得到「请求体过大」的
// 换号错误（客户端最终 413），不冷却账号。
func TestOpenAIUploadWatchdog_StallBecomesBodyTooLargeFailoverWithoutEviction(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.UpstreamUploadWatchdog = config.UpstreamUploadWatchdogConfig{Enabled: true, MinBodyBytes: 1024, MaxUploadSeconds: 1, WindowSeconds: 1}
	upstream := &slowUploadUpstream{chunk: 512, interval: 20 * time.Millisecond}
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, accountRepo: repo}
	account := &Account{ID: 23, Name: "switchbase", Platform: PlatformOpenAI}
	c, rec := newOpenAITransportErrTestContext()

	// 1MiB 每秒只读约 25KiB，1 秒窗口后就判定传不完。
	_, err := svc.doOpenAIUpstream(newUploadWatchdogRequest(t, 1<<20), "", account)
	require.ErrorIs(t, err, ErrOpenAIUpstreamUploadStalled)

	retErr := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, err, false)
	var fo *UpstreamFailoverError
	require.True(t, errors.As(retErr, &fo))
	require.True(t, fo.IsOpenAIRequestBodyTooLarge())
	require.Equal(t, NextAccountRetry, fo.NextAccountAction)
	require.Equal(t, http.StatusRequestEntityTooLarge, fo.ClientStatusCode)
	require.False(t, fo.ShouldReportAccountScheduleFailure(), "a slow upload of a huge body is not an account fault")
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(fo)
	require.False(t, eligible)
	require.Empty(t, repo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 0, rec.Body.Len())
}
