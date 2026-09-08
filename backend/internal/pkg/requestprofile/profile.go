// Package requestprofile records bounded metadata-only request timelines.
// It never changes request bodies, response flushing or retry decisions.
package requestprofile

import (
	"context"
	"sort"
	"sync"
	"time"
)

const MaxSpans = 512
const MaxEvents = 128

type Span struct {
	Turn       int    `json:"turn,omitempty"`
	ID         int    `json:"id"`
	Name       string `json:"name"`
	StartUS    int64  `json:"start_us"`
	EndUS      int64  `json:"end_us"`
	Attempt    int    `json:"attempt,omitempty"`
	AccountID  int64  `json:"account_id,omitempty"`
	Status     int    `json:"status,omitempty"`
	Incomplete bool   `json:"incomplete,omitempty"`
}
type Event struct {
	Turn      int    `json:"turn,omitempty"`
	AtUS      int64  `json:"at_us"`
	Kind      string `json:"kind"`
	Attempt   int    `json:"attempt,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	Status    int    `json:"status,omitempty"`
}
type Segment struct {
	Turn       int    `json:"turn,omitempty"`
	Name       string `json:"name"`
	StartUS    int64  `json:"start_us"`
	DurationUS int64  `json:"duration_us"`
	Attempt    int    `json:"attempt,omitempty"`
	AccountID  int64  `json:"account_id,omitempty"`
}
type Snapshot struct {
	Evidence  string    `json:"evidence"`
	Notes     []string  `json:"notes,omitempty"`
	Version   int       `json:"version"`
	TotalUS   int64     `json:"total_us"`
	Spans     []Span    `json:"spans"`
	Events    []Event   `json:"events"`
	Segments  []Segment `json:"segments"`
	Dropped   int       `json:"dropped"`
	Attempts  int       `json:"attempts"`
	GroupID   int64     `json:"group_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	Protocol  string    `json:"protocol,omitempty"`
	BodyBytes int64     `json:"body_bytes,omitempty"`
}
type Recorder struct {
	turnSpan          int
	lastAccount       int64
	chainStartAttempt int
	turn              int
	mu                sync.Mutex
	start             time.Time
	spans             []Span
	events            []Event
	dropped, attempts int
	group             int64
	model, protocol   string
	body              int64
	closed            bool
}
type contextKey struct{}
type attemptKey struct{}
type attemptRef struct {
	number  int
	account int64
}

func Attach(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, contextKey{}, &Recorder{start: start})
}
func From(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}
func (r *Recorder) now() int64 { return max(0, time.Since(r.start).Microseconds()) }
func Metadata(ctx context.Context, group int64, model, protocol string) {
	if r := From(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			return
		}
		if group > 0 {
			r.group = group
		}
		if model != "" {
			r.model = model
		}
		if protocol != "" {
			r.protocol = protocol
		}
	}
}
func BodySize(ctx context.Context, n int64) {
	if r := From(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.closed {
			r.body = n
		}
	}
}

// Start records each invocation separately, including repeated and concurrent spans.
func Start(ctx context.Context, name string) func() {
	r := From(ctx)
	if r == nil {
		return func() {}
	}
	ref, _ := ctx.Value(attemptKey{}).(attemptRef)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return func() {}
	}
	if len(r.spans) >= MaxSpans {
		r.dropped++
		r.mu.Unlock()
		return func() {}
	}
	i := len(r.spans)
	r.spans = append(r.spans, Span{ID: i + 1, Name: name, StartUS: r.now(), EndUS: -1, Attempt: ref.number, AccountID: ref.account})
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.closed {
				r.spans[i].EndUS = r.now()
			}
		})
	}
}
func Mark(ctx context.Context, kind string, status int) {
	r := From(ctx)
	if r == nil {
		return
	}
	ref, _ := ctx.Value(attemptKey{}).(attemptRef)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markLocked(kind, ref, status)
}
func (r *Recorder) markLocked(kind string, ref attemptRef, status int) {
	if r.closed {
		return
	}
	if len(r.events) >= MaxEvents {
		r.dropped++
		return
	}
	r.events = append(r.events, Event{Turn: r.turn, AtUS: r.now(), Kind: kind, Attempt: ref.number, AccountID: ref.account, Status: status})
}

// NewAttempt marks retry/fallback from the actual sequence of HTTP attempts.
// A switch event alone does not claim that all preceding time was retry backoff.
func NewAttempt(ctx context.Context, account int64) context.Context {
	r := From(ctx)
	if r == nil {
		return ctx
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ctx
	}
	previous := r.lastAccount
	r.attempts++
	ref := attemptRef{r.attempts, account}
	if r.attempts > r.chainStartAttempt+1 {
		kind := "retry"
		if previous != account {
			kind = "fallback"
		}
		r.markLocked(kind, ref, 0)
	}
	r.lastAccount = account
	r.markLocked("attempt_start", ref, 0)
	return context.WithValue(ctx, attemptKey{}, ref)
}
func Finish(ctx context.Context, at time.Time) *Snapshot {
	r := From(ctx)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	total := max(0, at.Sub(r.start).Microseconds())
	spans := append([]Span{}, r.spans...)
	for i := range spans {
		if spans[i].EndUS < 0 {
			spans[i].EndUS = total
			spans[i].Incomplete = true
		}
		spans[i].StartUS = min(total, spans[i].StartUS)
		spans[i].EndUS = max(spans[i].StartUS, min(total, spans[i].EndUS))
	}
	events := append([]Event{}, r.events...)
	for i := range events {
		events[i].AtUS = max(0, min(total, events[i].AtUS))
	}
	return &Snapshot{Evidence: "measured", Version: 1, TotalUS: total, Spans: spans, Events: events, Segments: Partition(spans, total), Dropped: r.dropped, Attempts: r.attempts, GroupID: r.group, Model: r.model, Protocol: r.protocol, BodyBytes: r.body}
}

// Partition constructs a wall-clock strip without summing overlapping spans.
// The narrowest enclosing span owns an interval; ties use the later span ID.
// Uninstrumented intervals remain explicit rather than being called upstream time.
func Partition(spans []Span, total int64) []Segment {
	total = max(0, total)
	bounds := []int64{0, total}
	for _, s := range spans {
		bounds = append(bounds, max(0, min(total, s.StartUS)), max(0, min(total, s.EndUS)))
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	out := []Segment{}
	for i := 1; i < len(bounds); i++ {
		a, b := bounds[i-1], bounds[i]
		if b <= a {
			continue
		}
		var selected *Span
		for j := range spans {
			s := &spans[j]
			if s.StartUS <= a && s.EndUS >= b && (selected == nil || s.EndUS-s.StartUS < selected.EndUS-selected.StartUS || s.EndUS-s.StartUS == selected.EndUS-selected.StartUS && s.ID > selected.ID) {
				selected = s
			}
		}
		seg := Segment{Name: "unattributed", StartUS: a, DurationUS: b - a}
		if selected != nil {
			seg.Turn = selected.Turn
			seg.Name = selected.Name
			seg.Attempt = selected.Attempt
			seg.AccountID = selected.AccountID
		}
		if n := len(out); n > 0 && out[n-1].Name == seg.Name && out[n-1].Attempt == seg.Attempt && out[n-1].AccountID == seg.AccountID && out[n-1].Turn == seg.Turn {
			out[n-1].DurationUS += seg.DurationUS
		} else {
			out = append(out, seg)
		}
	}
	return out
}

// Preparation ends at the transport boundary, with a deferred fallback for
// requests rejected before transport. The context value does not affect routing.
type preparationKey struct{}

func Preparation(ctx context.Context) (context.Context, func()) {
	if From(ctx) == nil {
		return ctx, func() {}
	}
	end := Start(ctx, "request_prepare")
	return context.WithValue(ctx, preparationKey{}, end), end
}
func endPreparation(ctx context.Context) {
	if end, ok := ctx.Value(preparationKey{}).(func()); ok {
		end()
	}
}

// BeginTurn separates ordinary WebSocket messages from transport retries.
// Repeating the same logical turn on account fallback keeps its retry chain.
func BeginTurn(ctx context.Context, turn int) {
	r := From(ctx)
	if r == nil || turn <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.turn == turn {
		if r.turnSpan >= 0 && r.turnSpan < len(r.spans) {
			r.spans[r.turnSpan].EndUS = -1
		}
		return
	}
	if r.turn > 0 {
		r.chainStartAttempt = r.attempts
		r.lastAccount = 0
	}
	r.turn = turn
	r.turnSpan = -1
	if len(r.spans) < MaxSpans {
		r.turnSpan = len(r.spans)
		r.spans = append(r.spans, Span{ID: len(r.spans) + 1, Name: "websocket_turn", Turn: turn, StartUS: r.now(), EndUS: -1})
	} else {
		r.dropped++
	}
	r.markLocked("turn_start", attemptRef{}, 0)
}
func EndTurn(ctx context.Context, turn int, failed bool) {
	r := From(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.turn != turn {
		return
	}
	if r.turnSpan >= 0 && r.turnSpan < len(r.spans) {
		r.spans[r.turnSpan].EndUS = r.now()
	}
	kind := "turn_complete"
	if failed {
		kind = "turn_attempt_error"
	}
	r.markLocked(kind, attemptRef{}, 0)
}
