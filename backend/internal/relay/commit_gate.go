// Package relay contains request-local data-plane primitives. Protocol adapters,
// retry policy, account selection and accounting belong to the caller.
package relay

import (
	"bytes"
	"errors"
	"io"
)

var (
	ErrLimit  = errors.New("relay precommit buffer limit exceeded")
	ErrClosed = errors.New("relay commit gate closed")
)

// CommitGate holds an attempt's prefix until its protocol adapter identifies a
// commit point. It owns no connections, files, timers or shared state. A single
// stream owner must serialize calls; heartbeats should bypass this prefix.
type CommitGate struct {
	limit     int64
	size      int64
	chunks    [][]byte
	closed    bool
	committed bool
}

func NewCommitGate(limit int64) *CommitGate {
	if limit < 1 {
		limit = 1
	}
	return &CommitGate{limit: limit}
}

func (g *CommitGate) Buffered() int64 { return g.size }
func (g *CommitGate) Closed() bool    { return g.closed }
func (g *CommitGate) Committed() bool { return g.committed }

// Write takes a copy: callers may reuse scanner/serialization buffers. The
// entire write is rejected at the limit; exceeding it never forces a commit.
func (g *CommitGate) Write(p []byte) (int, error) {
	if g.closed {
		return 0, ErrClosed
	}
	if int64(len(p)) > g.limit-g.size {
		return 0, ErrLimit
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := len(p)
	for len(p) > 0 {
		last := len(g.chunks) - 1
		if last < 0 || len(g.chunks[last]) == cap(g.chunks[last]) {
			// Bound allocation overhead even for millions of one-byte writes.
			capacity := min(int64(16*1024), g.limit-g.size)
			g.chunks = append(g.chunks, make([]byte, 0, int(capacity)))
			last++
		}
		count := min(len(p), cap(g.chunks[last])-len(g.chunks[last]))
		g.chunks[last] = append(g.chunks[last], p[:count]...)
		g.size += int64(count)
		p = p[count:]
	}
	return n, nil
}

func (g *CommitGate) WriteString(p string) (int, error) { return g.Write([]byte(p)) }

// CommitTo is irreversible even on a short or failed write: downstream may
// have received a prefix. It must never be called again to replay that prefix.
func (g *CommitGate) CommitTo(dst io.Writer) error {
	return g.Commit(func(src io.Reader) error {
		_, err := io.Copy(dst, src)
		return err
	})
}

// Commit lets an existing protocol adapter consume the held prefix without
// copying it into another full-size buffer. Consumption is an irreversible
// commit even if the adapter returns an error after writing part of a response.
func (g *CommitGate) Commit(consume func(io.Reader) error) error {
	if g.closed {
		return ErrClosed
	}
	g.committed = true
	g.closed = true
	defer g.release()
	readers := make([]io.Reader, 0, len(g.chunks))
	for _, chunk := range g.chunks {
		readers = append(readers, bytes.NewReader(chunk))
	}
	return consume(io.MultiReader(readers...))
}

// Close discards an uncommitted prefix and releases memory. It does not undo a
// commit and does not grant permission to retry an upstream execution.
func (g *CommitGate) Close() error {
	g.closed = true
	g.release()
	return nil
}

func (g *CommitGate) release() { g.chunks = nil; g.size = 0 }
