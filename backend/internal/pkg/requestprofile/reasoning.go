package requestprofile

import (
	"context"
	"sort"
)

// ReasoningObserver records an observed reasoning phase between semantic SSE
// boundaries. It measures elapsed event time, never upstream compute time.
// A phase without a closing boundary is retained as an incomplete raw span
// but is not attributed as reasoning in the wall-clock summary.
func ReasoningObserver(ctx context.Context) func(reasoning, boundary bool) {
	r := From(ctx)
	if r == nil {
		return func(bool, bool) {}
	}
	r.mu.Lock()
	if r.parallel || r.closed {
		r.mu.Unlock()
		return func(bool, bool) {}
	}
	ctx = context.WithValue(ctx, attemptKey{}, attemptRef{r.attempts, r.lastAccount})
	r.mu.Unlock()
	var end func()
	return func(reasoning, boundary bool) {
		if reasoning && end == nil {
			end = Start(ctx, "reasoning_observed")
		}
		if boundary && end != nil {
			end()
			end = nil
		}
	}
}

func partitionReasoning(spans []Span, total int64) []Segment {
	base := make([]Span, 0, len(spans))
	regions := make([]Span, 0)
	for _, s := range spans {
		if s.Name != "reasoning_observed" {
			base = append(base, s)
		} else if !s.Incomplete {
			regions = append(regions, s)
		}
	}
	segments := Partition(base, total)
	if len(regions) == 0 {
		return segments
	}
	out := make([]Segment, 0, len(segments)+len(regions)*2)
	for _, s := range segments {
		if s.Name != "response_body" {
			out = append(out, s)
			continue
		}
		end := s.StartUS + s.DurationUS
		bounds := []int64{s.StartUS, end}
		for _, r := range regions {
			if r.Attempt == s.Attempt && r.AccountID == s.AccountID && r.StartUS < end && r.EndUS > s.StartUS {
				bounds = append(bounds, max(s.StartUS, r.StartUS), min(end, r.EndUS))
			}
		}
		sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
		for i := 1; i < len(bounds); i++ {
			a, b := bounds[i-1], bounds[i]
			if a == b {
				continue
			}
			part := s
			part.StartUS = a
			part.DurationUS = b - a
			for _, r := range regions {
				if r.Attempt == s.Attempt && r.AccountID == s.AccountID && r.StartUS <= a && r.EndUS >= b {
					part.Name = "reasoning_observed"
					break
				}
			}
			out = append(out, part)
		}
	}
	return out
}
