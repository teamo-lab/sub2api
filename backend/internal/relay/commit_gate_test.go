package relay

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestCommitGatePreservesBytesAndOwnsBuffer(t *testing.T) {
	g := NewCommitGate(64 * 1024)
	want := bytes.Repeat([]byte("data: hello\r\n\r\n"), 2000)
	for _, b := range want {
		p := []byte{b}
		if _, err := g.Write(p); err != nil {
			t.Fatal(err)
		}
		p[0] = '!'
	}
	if len(g.chunks) > 4 {
		t.Fatalf("per-write allocation overhead: %d chunks", len(g.chunks))
	}
	var dst bytes.Buffer
	if err := g.CommitTo(&dst); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, dst.Bytes()) {
		t.Fatal("prefix corrupted")
	}
	if !g.Committed() || !g.Closed() || g.Buffered() != 0 {
		t.Fatal("commit not final")
	}
	if err := g.CommitTo(&dst); !errors.Is(err, ErrClosed) {
		t.Fatal("second commit replayed prefix")
	}
}

func TestLimitDoesNotReleaseOrDropPrefix(t *testing.T) {
	g := NewCommitGate(4)
	_, _ = g.WriteString("abc")
	if n, err := g.WriteString("de"); n != 0 || !errors.Is(err, ErrLimit) {
		t.Fatalf("%d %v", n, err)
	}
	if g.Committed() || g.Buffered() != 3 {
		t.Fatal("limit changed commit state")
	}
	if g.Outcome(false, true) != PrecommitFailure {
		t.Fatal("failure incorrectly committed")
	}
	_ = g.Close()
	if g.chunks != nil || g.Buffered() != 0 {
		t.Fatal("discard retained prefix")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:1]) }

func TestPartialWriteNeverBecomesRetryable(t *testing.T) {
	g := NewCommitGate(64)
	_, _ = g.WriteString("data: partial\n\n")
	w := new(shortWriter)
	if err := g.CommitTo(w); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if g.Outcome(false, true) != CommittedFailure {
		t.Fatal("partial write allowed replay")
	}
	_ = g.Close()
	if !g.Committed() {
		t.Fatal("close undid commit")
	}
	if g.Outcome(false, false) != Unknown {
		t.Fatal("missing terminal treated as success")
	}
}

func TestFailureCannotBeOverriddenBySuccess(t *testing.T) {
	g := NewCommitGate(64)
	if g.Outcome(true, true) != PrecommitFailure {
		t.Fatal("later terminal erased failure")
	}
	if g.Outcome(true, false) != Succeeded {
		t.Fatal("explicit successful terminal lost")
	}
}
