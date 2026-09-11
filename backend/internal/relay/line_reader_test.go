package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLineReaderClosesRealHTTPBodyOnTimeout(t *testing.T) {
	closed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: preamble\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLineReader(resp.Body, 1024)
	defer reader.Close()
	for range 2 {
		if _, err = reader.Next(context.Background(), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = reader.Next(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrReadTimeout) {
		t.Fatal(err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("HTTP connection still occupied")
	}
	select {
	case <-reader.done:
	default:
		t.Fatal("read goroutine still alive")
	}
}

func TestLineReaderCancellationBeforeCommit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLineReader(resp.Body, 1024)
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.Next(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}
