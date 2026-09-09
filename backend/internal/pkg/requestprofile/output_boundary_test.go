package requestprofile

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestOutputBoundaryPreservesRetryAttemptsAndWallClock(t *testing.T) {
	spans := []Span{{ID: 1, Name: "response_body", StartUS: 0, EndUS: 30, Attempt: 1, AccountID: 22}, {ID: 2, Name: "retry_backoff", StartUS: 30, EndUS: 40}, {ID: 3, Name: "response_body", StartUS: 40, EndUS: 100, Attempt: 2, AccountID: 33}}
	first := int64(60)
	got := splitResponseBody(Partition(spans, 100), "sse", false, true, &first)
	want := []Segment{{Name: "response_body_before_output", StartUS: 0, DurationUS: 30, Attempt: 1, AccountID: 22}, {Name: "retry_backoff", StartUS: 30, DurationUS: 10}, {Name: "response_body_before_output", StartUS: 40, DurationUS: 20, Attempt: 2, AccountID: 33}, {Name: "response_body_after_output", StartUS: 60, DurationUS: 40, Attempt: 2, AccountID: 33}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	for boundary := int64(0); boundary <= 100; boundary++ {
		total := int64(0)
		for _, s := range splitResponseBody(Partition(spans, 100), "sse", false, true, &boundary) {
			if s.StartUS != total || s.DurationUS <= 0 {
				t.Fatalf("invalid interval %+v", s)
			}
			total += s.DurationUS
		}
		if total != 100 {
			t.Fatal(total)
		}
	}
}
func TestOutputBoundaryUnknown(t *testing.T) {
	first := int64(5)
	for _, tc := range []struct {
		name, protocol      string
		parallel, supported bool
		first               *int64
		want                string
	}{
		{"no output", "sse", false, true, nil, "response_body_no_output"},
		{"uncovered", "sse", false, false, nil, "response_body_output_unknown"},
		{"http", "http", false, true, &first, "response_body_output_unknown"},
		{"websocket", "ws", false, true, &first, "response_body_output_unknown"},
		{"parallel", "sse", true, true, &first, "response_body_output_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitResponseBody([]Segment{{Name: "response_body", DurationUS: 10}}, tc.protocol, tc.parallel, tc.supported, tc.first)
			if len(got) != 1 || got[0].Name != tc.want || got[0].DurationUS != 10 {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestFinishUsesDeliveredOutputRatherThanHeartbeatOrUpstreamSemantic(t *testing.T) {
	start := time.Now().Add(-time.Second)
	ctx := Attach(context.Background(), start)
	Metadata(ctx, 1, "local-fixture", "sse")
	DeliverySupported(ctx)
	DownstreamWrite(ctx, 1, 1, nil)
	DownstreamFlush(ctx)
	Mark(ctx, "first_semantic", 0)
	r := From(ctx)
	r.spans = []Span{{ID: 1, Name: "response_body", StartUS: 0, EndUS: 100}}
	got := Finish(ctx, start.Add(100*time.Microsecond))
	if got.Segments[0].Name != "response_body_no_output" {
		t.Fatalf("heartbeat treated as output: %+v", got)
	}
	ctx = Attach(context.Background(), start)
	Metadata(ctx, 1, "local-fixture", "sse")
	DeliverySupported(ctx)
	Delivery(ctx, true, "")
	r = From(ctx)
	boundary := *r.deliveredOutput
	r.spans = []Span{{ID: 1, Name: "response_body", StartUS: 0, EndUS: boundary + 100}}
	got = Finish(ctx, start.Add(time.Duration(boundary+100)*time.Microsecond))
	if len(got.Segments) != 2 || got.Segments[0].DurationUS != boundary || got.Segments[1].DurationUS != 100 {
		t.Fatalf("%+v", got.Segments)
	}
	if got.Spans[0].Name != "response_body" {
		t.Fatal("raw spans changed")
	}
}
