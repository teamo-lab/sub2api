package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var ErrReadTimeout = errors.New("relay upstream stream read timeout")

type lineResult struct {
	line string
	err  error
}

// LineReader bounds read-ahead to one line and lets the request owner cancel a
// blocked read. Close must be called; the supplied HTTP body must unblock Read
// on Close (as net/http response bodies do). It does not retry or open requests.
type LineReader struct {
	body     io.ReadCloser
	lines    chan lineResult
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	closeErr error
	timer    *time.Timer
}

func NewLineReader(body io.ReadCloser, maxLineBytes int) *LineReader {
	r := &LineReader{body: body, lines: make(chan lineResult, 1), stop: make(chan struct{}), done: make(chan struct{}), timer: time.NewTimer(time.Hour)}
	r.timer.Stop()
	go func() {
		defer close(r.done)
		defer close(r.lines)
		scanner := bufio.NewScanner(body)
		if maxLineBytes < 1 {
			maxLineBytes = 1
		}
		scanner.Buffer(make([]byte, min(64*1024, maxLineBytes)), maxLineBytes)
		for scanner.Scan() {
			select {
			case r.lines <- lineResult{line: scanner.Text()}:
			case <-r.stop:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case r.lines <- lineResult{err: err}:
			case <-r.stop:
			}
		}
	}()
	return r
}

// Next's timeout bounds this read, not downstream processing time. The caller
// may also pass the remaining absolute precommit budget as this timeout.
func (r *LineReader) Next(ctx context.Context, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var timeoutCh <-chan time.Time
	if !r.timer.Stop() {
		select {
		case <-r.timer.C:
		default:
		}
	}
	if timeout > 0 {
		r.timer.Reset(timeout)
		timeoutCh = r.timer.C
		defer r.timer.Stop()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-r.stop:
		return "", ErrClosed
	case <-timeoutCh:
		return "", ErrReadTimeout
	case result, ok := <-r.lines:
		if !ok {
			return "", io.EOF
		}
		return result.line, result.err
	}
}

func (r *LineReader) Close() error {
	r.once.Do(func() { close(r.stop); r.timer.Stop(); r.closeErr = r.body.Close(); <-r.done })
	return r.closeErr
}
