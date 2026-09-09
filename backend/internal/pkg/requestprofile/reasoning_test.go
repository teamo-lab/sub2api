package requestprofile

import (
	"context"
	"testing"
	"time"
)

func TestReasoningRefinesBodyWithoutDoubleCounting(t *testing.T) {
	spans := []Span{{ID: 1, Name: "response_body", StartUS: 10, EndUS: 100, Attempt: 1, AccountID: 22}, {ID: 2, Name: "reasoning_observed", StartUS: 20, EndUS: 70, Attempt: 1, AccountID: 22}, {ID: 3, Name: "reasoning_observed", StartUS: 30, EndUS: 80, Attempt: 2, AccountID: 33}}
	first := int64(50)
	segs := splitResponseBody(partitionReasoning(spans, 110), "sse", false, true, &first)
	totals := map[string]int64{}
	pos := int64(0)
	for _, s := range segs {
		if s.StartUS != pos {
			t.Fatalf("gap: %+v", segs)
		}
		pos += s.DurationUS
		totals[s.Name] += s.DurationUS
	}
	if pos != 110 || totals["reasoning_observed_before_output"] != 30 || totals["reasoning_observed_after_output"] != 20 || totals["response_body_before_output"] != 10 || totals["response_body_after_output"] != 30 || totals["unattributed"] != 20 {
		t.Fatal(totals)
	}
	spans[1].Incomplete = true
	for _, s := range partitionReasoning(spans, 110) {
		if s.Name == "reasoning_observed" {
			t.Fatalf("incomplete or other attempt credited: %+v", s)
		}
	}
}
func TestReasoningObserverKeepsMissingBoundaryUnknown(t *testing.T) {
	start := time.Now()
	ctx := Attach(context.Background(), start)
	ctx = NewAttempt(ctx, 22)
	observe := ReasoningObserver(ctx)
	observe(false, false)
	observe(true, false)
	observe(true, false)
	s := Finish(ctx, time.Now())
	if len(s.Spans) != 1 || !s.Spans[0].Incomplete || s.Spans[0].Attempt != 1 {
		t.Fatalf("%+v", s.Spans)
	}
	ctx = Attach(context.Background(), time.Now())
	ctx = NewAttempt(ctx, 33)
	observe = ReasoningObserver(ctx)
	observe(true, false)
	observe(false, false)
	observe(false, true)
	s = Finish(ctx, time.Now())
	if len(s.Spans) != 1 || s.Spans[0].Incomplete || s.Spans[0].AccountID != 33 {
		t.Fatalf("%+v", s.Spans)
	}
}
