// Package requestprofile records bounded metadata-only request timelines.
// It never changes request bodies, response flushing or retry decisions.
package requestprofile

import (
	"context"
	"sort"
	"strings"
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
	Origin            string `json:"origin,omitempty"`
	SourceAccountID   int64  `json:"source_account_id,omitempty"`
	RejectedAccountID int64  `json:"rejected_account_id,omitempty"`
	WaitUS            int64  `json:"wait_us,omitempty"`
	Turn              int    `json:"turn,omitempty"`
	AtUS              int64  `json:"at_us"`
	Kind              string `json:"kind"`
	Attempt           int    `json:"attempt,omitempty"`
	AccountID         int64  `json:"account_id,omitempty"`
	Status            int    `json:"status,omitempty"`
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
	ConcurrentUpstreams          bool      `json:"concurrent_upstreams"`
	DeliveryObservationSupported bool      `json:"delivery_observation_supported"`
	DownstreamFirstWriteUS       *int64    `json:"downstream_first_write_us,omitempty"`
	DownstreamLastWriteUS        *int64    `json:"downstream_last_write_us,omitempty"`
	DownstreamWriteErrorUS       *int64    `json:"downstream_write_error_us,omitempty"`
	DownstreamBytes              int64     `json:"downstream_bytes"`
	ClientDisconnectedUS         *int64    `json:"client_disconnected_us,omitempty"`
	DownstreamFirstOutputUS      *int64    `json:"downstream_first_output_us,omitempty"`
	DownstreamCompleteUS         *int64    `json:"downstream_complete_us,omitempty"`
	DownstreamErrorUS            *int64    `json:"downstream_error_us,omitempty"`
	ClientOutcome                string    `json:"client_outcome"`
	DrainAfterDisconnectUS       int64     `json:"drain_after_disconnect_us"`
	Evidence                     string    `json:"evidence"`
	Notes                        []string  `json:"notes,omitempty"`
	Version                      int       `json:"version"`
	TotalUS                      int64     `json:"total_us"`
	Spans                        []Span    `json:"spans"`
	Events                       []Event   `json:"events"`
	Segments                     []Segment `json:"segments"`
	Dropped                      int       `json:"dropped"`
	Attempts                     int       `json:"attempts"`
	GroupID                      int64     `json:"group_id,omitempty"`
	Model                        string    `json:"model,omitempty"`
	Protocol                     string    `json:"protocol,omitempty"`
	BodyBytes                    int64     `json:"body_bytes,omitempty"`
}
type Recorder struct {
	allowedGroups                                                    []int64
	discarded                                                        bool
	httpActive                                                       int
	parallel                                                         bool
	original                                                         context.Context
	deliverySupported                                                bool
	firstWrite, lastWrite, writeError                                *int64
	writtenBytes                                                     int64
	firstFlush                                                       bool
	stopCancellation                                                 func() bool
	disconnected, deliveredOutput, deliveredComplete, deliveredError *int64
	lastFailed                                                       bool
	turnSpan                                                         int
	lastAccount                                                      int64
	chainStartAttempt                                                int
	turn                                                             int
	mu                                                               sync.Mutex
	start                                                            time.Time
	spans                                                            []Span
	events                                                           []Event
	dropped, attempts                                                int
	group                                                            int64
	model, protocol                                                  string
	body                                                             int64
	closed                                                           bool
}
type contextKey struct{}
type attemptKey struct{}
type attemptRef struct {
	number  int
	account int64
}

func Attach(ctx context.Context, start time.Time) context.Context {
	return AttachScoped(ctx, start, nil)
}
func AttachScoped(ctx context.Context, start time.Time, groups []int64) context.Context {
	r := &Recorder{start: start, original: ctx, allowedGroups: append([]int64(nil), groups...)}
	attached := context.WithValue(ctx, contextKey{}, r)
	r.stopCancellation = context.AfterFunc(ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed || r.disconnected != nil {
			return
		}
		at := r.now()
		r.disconnected = &at
		r.markLocked("client_disconnected", attemptRef{}, 0)
	})
	return attached
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
			if len(model) > 256 {
				model = model[:256]
				r.dropped++
			}
			r.model = strings.Clone(model)
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
	if r.closed {
		return
	}
	if kind == "upstream_response" {
		r.lastFailed = status >= 400
	}
	if kind == "upstream_error" || kind == "network_error" || kind == "timeout" {
		r.lastFailed = true
	}
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
	if r.attempts > r.chainStartAttempt+1 && r.lastFailed && !r.parallel {
		kind := "retry"
		if previous != account {
			kind = "fallback"
		}
		r.markLocked(kind, ref, 0)
	}
	r.lastFailed = false
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
	if r.discarded {
		return nil
	}
	if r.stopCancellation != nil {
		r.stopCancellation()
	}
	total := max(0, at.Sub(r.start).Microseconds())
	if r.disconnected == nil && r.original != nil && r.original.Err() != nil {
		observed := total
		r.disconnected = &observed
		r.markLocked("client_disconnected", attemptRef{}, 0)
	}
	r.closed = true
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
	outcome := "unverified"
	drain := int64(0)
	if r.disconnected != nil {
		drain = max(0, total-*r.disconnected)
	}
	if r.deliveredComplete != nil && (r.disconnected == nil || *r.deliveredComplete <= *r.disconnected) {
		outcome = "completion_written"
	} else if r.deliveredError != nil && (r.disconnected == nil || *r.deliveredError <= *r.disconnected) {
		outcome = "error_written"
	} else if r.disconnected != nil && (r.writeError == nil || *r.disconnected <= *r.writeError) {
		outcome = "disconnected"
	} else if r.writeError != nil {
		outcome = "write_failed"
	} else if r.disconnected != nil {
		outcome = "disconnected"
		drain = max(0, total-*r.disconnected)
	}
	return &Snapshot{ConcurrentUpstreams: r.parallel, DeliveryObservationSupported: r.deliverySupported, DownstreamFirstWriteUS: copyTime(r.firstWrite), DownstreamLastWriteUS: copyTime(r.lastWrite), DownstreamWriteErrorUS: copyTime(r.writeError), DownstreamBytes: r.writtenBytes, ClientDisconnectedUS: copyTime(r.disconnected), DownstreamFirstOutputUS: copyTime(r.deliveredOutput), DownstreamCompleteUS: copyTime(r.deliveredComplete), DownstreamErrorUS: copyTime(r.deliveredError), ClientOutcome: outcome, DrainAfterDisconnectUS: drain, Evidence: "measured", Version: 1, TotalUS: total, Spans: spans, Events: events, Segments: Partition(spans, total), Dropped: r.dropped, Attempts: r.attempts, GroupID: r.group, Model: r.model, Protocol: r.protocol, BodyBytes: r.body}
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

func LocalReselect(ctx context.Context, source, rejected, selected, waitMS int64) {
	r := From(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kind := "local_reselect"
	if selected > 0 {
		kind = "local_reselect_admitted"
	}
	before := len(r.events)
	r.markLocked(kind, attemptRef{account: selected}, 0)
	if len(r.events) > before {
		e := &r.events[len(r.events)-1]
		e.Origin = "local_account_admission"
		e.SourceAccountID = source
		e.RejectedAccountID = rejected
		e.WaitUS = max(0, waitMS) * 1000
	}
}

func AccountContext(ctx context.Context, account int64) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptKey{}, attemptRef{account: account})
}

func copyTime(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// Delivery is called only AFTER an existing successful downstream flush.
// This observes a server write, never claims that the remote client received it.
func Delivery(ctx context.Context, output bool, terminal string) {
	r := From(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.disconnected != nil || r.writeError != nil || (r.original != nil && r.original.Err() != nil) {
		return
	}
	at := r.now()
	if output && r.deliveredOutput == nil {
		r.deliveredOutput = &at
		r.markLocked("downstream_first_output", attemptRef{}, 0)
	}
	if terminal == "complete" && r.deliveredComplete == nil {
		r.deliveredComplete = &at
		r.markLocked("downstream_complete", attemptRef{}, 0)
	}
	if terminal == "error" && r.deliveredError == nil {
		r.deliveredError = &at
		r.markLocked("downstream_error", attemptRef{}, 0)
	}
}

func DeliverySupported(ctx context.Context) {
	if r := From(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.closed {
			r.deliverySupported = true
		}
	}
}
func DownstreamWrite(ctx context.Context, n, expected int, err error) {
	r := From(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	at := r.now()
	if n > 0 {
		r.writtenBytes += int64(n)
		r.lastWrite = &at
		if r.firstWrite == nil {
			r.firstWrite = &at
			r.markLocked("downstream_first_write", attemptRef{}, 0)
		}
	}
	if (err != nil || n < expected) && r.writeError == nil {
		r.writeError = &at
		r.markLocked("downstream_write_error", attemptRef{}, 0)
	}
}
func DownstreamFlush(ctx context.Context) {
	if r := From(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.closed && !r.firstFlush {
			r.firstFlush = true
			r.markLocked("downstream_first_flush", attemptRef{}, 0)
		}
	}
}

func activeHTTP(ctx context.Context) func() {
	r := From(ctx)
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.httpActive++
	if r.httpActive > 1 {
		r.parallel = true
	}
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { r.mu.Lock(); r.httpActive--; r.mu.Unlock() }) }
}

// Restrict collection as soon as authenticated group identity is available.
func AuthorizeGroup(ctx context.Context, group int64) context.Context {
	r := From(ctx)
	if r == nil {
		return ctx
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	allowed := len(r.allowedGroups) == 0
	for _, id := range r.allowedGroups {
		if id == group {
			allowed = true
		}
	}
	if allowed {
		r.group = group
		return ctx
	}
	r.discarded = true
	r.closed = true
	if r.stopCancellation != nil {
		r.stopCancellation()
	}
	r.spans = nil
	r.events = nil
	return context.WithValue(ctx, contextKey{}, (*Recorder)(nil))
}
