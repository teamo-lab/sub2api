package repository

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseCompletedHandler 先推 response.completed，过 tail 再结束 chunked 流。
func sseCompletedHandler(tail time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(tail):
		case <-r.Context().Done():
		}
	}
}

// readUntilCompletedThenClose 照搬网关的读法：后台 goroutine 扫描，主流程读到 completed 就 Close。
func readUntilCompletedThenClose(t *testing.T, client *http.Client, url string) time.Duration {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, abort := withUpstreamAbort(req)
	resp, err := client.Do(req)
	if err != nil {
		abort()
		t.Fatal(err)
	}
	resp.Body = wrapTrackedBody(resp, nil, abort)

	lines := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-done:
				return
			}
		}
	}()
	for line := range lines {
		if strings.Contains(line, "response.completed") {
			break
		}
	}
	close(done)
	start := time.Now()
	_ = resp.Body.Close()
	return time.Since(start)
}

func newCloseRaceServer(t *testing.T, tail time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(sseCompletedHandler(tail))
	// 卡住的 Close 会一直等到服务端关闭空闲连接；缩短到 2s 让回归在测试里可见。
	srv.Config.IdleTimeout = 2 * time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestTrackedBodyCloseAfterTerminalDoesNotStallKeepAliveConn(t *testing.T) {
	srv := newCloseRaceServer(t, 5*time.Millisecond)
	transport := &http.Transport{MaxIdleConnsPerHost: 4}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	// 不加保护时 go1.27 下约 1/4 的 Close 会卡满 IdleTimeout。
	for i := 0; i < 200; i++ {
		if d := readUntilCompletedThenClose(t, client, srv.URL); d > trackedCloseReadGrace/2 {
			t.Fatalf("request %d: Close took %v; reader/Close race poisoned the keep-alive connection", i, d)
		}
	}
}

func TestTrackedBodyCloseAbortsReaderWhenUpstreamNeverEnds(t *testing.T) {
	srv := newCloseRaceServer(t, time.Minute)
	transport := &http.Transport{MaxIdleConnsPerHost: 4}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	for i := 0; i < 3; i++ {
		d := readUntilCompletedThenClose(t, client, srv.URL)
		if d < trackedCloseReadGrace/2 || d > trackedCloseReadGrace+time.Second {
			t.Fatalf("request %d: Close took %v; want about %v (grace, then abort the connection)", i, d, trackedCloseReadGrace)
		}
	}
}

func TestTrackedBodyCloseWithoutConcurrentReaderIsImmediate(t *testing.T) {
	srv := newCloseRaceServer(t, time.Minute)
	client := &http.Client{Transport: &http.Transport{}}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	req, abort := withUpstreamAbort(req)
	resp, err := client.Do(req)
	if err != nil {
		abort()
		t.Fatal(err)
	}
	resp.Body = wrapTrackedBody(resp, nil, abort)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "response.completed") {
			break
		}
	}
	start := time.Now()
	_ = resp.Body.Close()
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Close with no in-flight Read took %v", d)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after Close should fail")
	}
}
