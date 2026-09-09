package requestprofile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPartitionConservesElapsedWithoutCountingNestedWorkTwice(t *testing.T) {
	spans := []Span{{ID: 1, Name: "outer", StartUS: 10, EndUS: 90}, {ID: 2, Name: "inner", StartUS: 20, EndUS: 40}}
	got := Partition(spans, 100)
	want := []string{"unattributed", "outer", "inner", "outer", "unattributed"}
	sum := int64(0)
	for i, s := range got {
		if s.Name != want[i] {
			t.Fatalf("%+v", got)
		}
		sum += s.DurationUS
	}
	if sum != 100 {
		t.Fatal(sum)
	}
	rng := rand.New(rand.NewSource(17))
	for run := 0; run < 100; run++ {
		spans = nil
		for n := 0; n < 25; n++ {
			a := rng.Int63n(1000)
			spans = append(spans, Span{ID: n, Name: "work", StartUS: a, EndUS: a + rng.Int63n(1000)})
		}
		sum = 0
		last := int64(0)
		for _, s := range Partition(spans, 1000) {
			if s.StartUS != last || s.DurationUS <= 0 {
				t.Fatal(s)
			}
			last += s.DurationUS
			sum += s.DurationUS
		}
		if sum != 1000 {
			t.Fatal(sum)
		}
	}
}
func TestAttemptsAndConcurrentSpansRemainDistinct(t *testing.T) {
	start := time.Now()
	ctx := Attach(context.Background(), start)
	for _, account := range []int64{4, 4, 7} {
		a := NewAttempt(ctx, account)
		done := Start(a, "upstream_headers")
		done()
		Mark(a, "upstream_response", 503)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); end := Start(ctx, "prepare"); end(); end() }()
	}
	wg.Wait()
	pending := Start(ctx, "unfinished")
	s := Finish(ctx, time.Now())
	pending()
	if len(s.Spans) != 24 || s.Attempts != 3 || !s.Spans[23].Incomplete {
		t.Fatalf("%+v", s)
	}
	kinds := map[string]int{}
	for _, e := range s.Events {
		kinds[e.Kind]++
	}
	if kinds["retry"] != 1 || kinds["fallback"] != 1 {
		t.Fatal(s.Events)
	}
	for _, span := range s.Spans {
		if span.EndUS < span.StartUS || span.EndUS > s.TotalUS {
			t.Fatal(span)
		}
	}
}
func TestBoundedAndClosedRecorder(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	for i := 0; i < MaxSpans+9; i++ {
		Start(ctx, "work")()
	}
	for i := 0; i < MaxEvents+7; i++ {
		Mark(ctx, "point", 0)
	}
	s := Finish(ctx, time.Now())
	if len(s.Spans) != MaxSpans || len(s.Events) != MaxEvents || s.Dropped != 16 {
		t.Fatalf("bounds %+v", s)
	}
	Start(ctx, "late")()
	Mark(ctx, "late", 0)
	if len(From(ctx).events) != MaxEvents {
		t.Fatal("closed recorder changed")
	}
}
func TestHTTPObserverPreservesWireAndExistingTrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != "private-body" || r.Header.Get("Authorization") != "Bearer private-key" {
			t.Error("changed request")
		}
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("unchanged-response"))
	}))
	defer server.Close()
	ctx := Attach(context.Background(), time.Now())
	called := false
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { called = true }})
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"?secret=private-query", strings.NewReader("private-body"))
	req.Header.Set("Authorization", "Bearer private-key")
	observed, finish := ObserveHTTP(req, 19)
	resp, err := server.Client().Do(observed)
	finish(resp, err)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("X-Test") != "yes" || string(b) != "unchanged-response" || !called {
		t.Fatal("wire/trace changed")
	}
	s := Finish(ctx, time.Now())
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "private") || s.Attempts != 1 {
		t.Fatal(string(raw))
	}
	found := false
	for _, e := range s.Events {
		if e.Kind == "upstream_response" && e.Status == 429 {
			found = true
		}
	}
	if !found {
		t.Fatal(s.Events)
	}
	disabled, _ := http.NewRequest("GET", server.URL, nil)
	got, _ := ObserveHTTP(disabled, 1)
	if got != disabled {
		t.Fatal("disabled request cloned")
	}
}

func TestPreparationEndsBeforeTransportAndLateEventsClamp(t *testing.T) {
	start := time.Now()
	base := Attach(context.Background(), start)
	ctx, end := Preparation(base)
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/", nil)
	_, finish := ObserveHTTP(req, 1)
	if From(ctx).spans[0].EndUS < 0 {
		t.Fatal("preparation not closed at transport boundary")
	}
	finish(nil, context.Canceled)
	end()
	cutoff := time.Now()
	Mark(ctx, "client_cancelled", 499)
	s := Finish(ctx, cutoff)
	for _, e := range s.Events {
		if e.AtUS > s.TotalUS {
			t.Fatal("event beyond total", e)
		}
	}
}
func BenchmarkRequestProfileSnapshot(b *testing.B) {
	for _, n := range []int{20, MaxSpans} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				ctx := Attach(context.Background(), time.Now())
				for j := 0; j < n; j++ {
					Start(ctx, "prepare")()
				}
				Finish(ctx, time.Now())
			}
		})
	}
}

func TestWebSocketMessagesDoNotBecomeFalseRetries(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	BeginTurn(ctx, 1)
	a := NewAttempt(ctx, 5)
	Mark(a, "upstream_response", 503)
	a = NewAttempt(ctx, 5)
	Mark(a, "upstream_response", 200)
	BeginTurn(ctx, 2)
	a = NewAttempt(ctx, 5)
	Mark(a, "upstream_response", 503)
	BeginTurn(ctx, 2)
	NewAttempt(ctx, 7)
	s := Finish(ctx, time.Now())
	retry, fallback := 0, 0
	for _, e := range s.Events {
		if e.Kind == "retry" {
			retry++
		}
		if e.Kind == "fallback" {
			fallback++
		}
	}
	if retry != 1 || fallback != 1 || s.Attempts != 4 {
		t.Fatal(s.Events)
	}
}

func TestWebSocketTurnSpanSurvivesRetryAndKeepsTurnsSeparate(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	BeginTurn(ctx, 1)
	EndTurn(ctx, 1, true)
	BeginTurn(ctx, 1)
	EndTurn(ctx, 1, false)
	BeginTurn(ctx, 2)
	EndTurn(ctx, 2, false)
	s := Finish(ctx, time.Now())
	if len(s.Spans) != 2 || s.Spans[0].Turn != 1 || s.Spans[1].Turn != 2 {
		t.Fatal(s.Spans)
	}
	for _, span := range s.Spans {
		if span.Incomplete {
			t.Fatal("completed turn marked incomplete")
		}
	}
}

func TestLocalAdmissionReselectionIsNotUpstreamRetry(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	LocalReselect(ctx, 1, 1, 0, 1840)
	LocalReselect(ctx, 1, 0, 2, 1840)
	NewAttempt(ctx, 2)
	s := Finish(ctx, time.Now())
	if s.Attempts != 1 {
		t.Fatal(s)
	}
	for _, e := range s.Events {
		if e.Kind == "retry" || e.Kind == "fallback" || e.Status == 429 {
			t.Fatal("local admission became upstream error", e)
		}
	}
	if s.Events[0].Origin != "local_account_admission" || s.Events[0].WaitUS != 1840000 {
		t.Fatal(s.Events)
	}
}

func awaitDisconnect(t *testing.T, ctx context.Context) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		r := From(ctx)
		r.mu.Lock()
		seen := r.disconnected != nil
		r.mu.Unlock()
		if seen {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancellation observer did not fire")
}
func TestUpstreamSuccessAfterClientDisconnectDoesNotBecomeDelivery(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := Attach(parent, time.Now())
	drainCtx := context.WithoutCancel(ctx)
	DeliverySupported(ctx)
	Mark(ctx, "first_semantic", 0)
	DownstreamWrite(ctx, 12, 12, nil)
	DownstreamFlush(ctx) // heartbeat bytes are not answer output
	cancel()
	awaitDisconnect(t, ctx)
	time.Sleep(3 * time.Millisecond)
	Mark(drainCtx, "upstream_response", 200)
	Delivery(drainCtx, true, "complete")
	s := Finish(drainCtx, time.Now())
	if s.ClientOutcome != "disconnected" || s.ClientDisconnectedUS == nil || s.DrainAfterDisconnectUS <= 0 || s.DownstreamCompleteUS != nil || s.DownstreamFirstOutputUS != nil {
		t.Fatalf("false client success %+v", s)
	}
}
func TestCompletedWriteBeforeDisconnectRemainsDistinctFromReceipt(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx := Attach(parent, time.Now())
	DeliverySupported(ctx)
	Delivery(ctx, true, "complete")
	cancel()
	awaitDisconnect(t, ctx)
	s := Finish(ctx, time.Now())
	if s.ClientOutcome != "completion_written" || s.DownstreamCompleteUS == nil || s.DownstreamFirstOutputUS == nil {
		t.Fatal(s)
	}
}
func TestSuccessiveHealthyCallsAreNotInferredRetries(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	a := NewAttempt(ctx, 1)
	Mark(a, "upstream_response", 200)
	NewAttempt(ctx, 1)
	s := Finish(ctx, time.Now())
	for _, e := range s.Events {
		if e.Kind == "retry" || e.Kind == "fallback" {
			t.Fatal(e)
		}
	}
}

func TestConcurrentHTTPCallsDoNotCreateFalseRetries(t *testing.T) {
	ctx := Attach(context.Background(), time.Now())
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/", nil)
	_, finish1 := ObserveHTTP(req, 1)
	_, finish2 := ObserveHTTP(req, 2)
	a := &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("err"))}
	b := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}
	finish1(a, nil)
	finish2(b, nil)
	a.Body.Close()
	b.Body.Close()
	_, finish3 := ObserveHTTP(req, 2)
	finish3(nil, context.DeadlineExceeded)
	s := Finish(ctx, time.Now())
	if !s.ConcurrentUpstreams {
		t.Fatal("overlap missed")
	}
	for _, e := range s.Events {
		if e.Kind == "retry" || e.Kind == "fallback" {
			t.Fatal("parallel call mislabeled", e)
		}
	}
}

func TestProfileScopeStopsUnselectedGroup(t *testing.T) {
	ctx := AttachScoped(context.Background(), time.Now(), []int64{38})
	root := ctx
	ctx = AuthorizeGroup(ctx, 2)
	if From(ctx) != nil || Finish(root, time.Now()) != nil {
		t.Fatal("unselected group recorded")
	}
	selected := AttachScoped(context.Background(), time.Now(), []int64{38})
	selected = AuthorizeGroup(selected, 38)
	Start(selected, "read")()
	s := Finish(selected, time.Now())
	if s == nil || s.GroupID != 38 || len(s.Spans) != 1 {
		t.Fatal(s)
	}
}
