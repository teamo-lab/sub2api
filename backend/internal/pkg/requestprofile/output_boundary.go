package requestprofile

// splitResponseBody refines only the non-overlapping wall-clock segments.
// The original spans remain untouched. A successful downstream output flush is
// the boundary, not an upstream delta, response headers, or a keepalive write.
// These labels describe observations; they never authorize a replay.
func splitResponseBody(segments []Segment, protocol string, parallel, supported bool, firstOutput *int64) []Segment {
	out := make([]Segment, 0, len(segments)+1)
	for _, segment := range segments {
		if segment.Name != "response_body" && segment.Name != "reasoning_observed" {
			out = append(out, segment)
			continue
		}
		prefix := segment.Name
		if protocol != "sse" || parallel || !supported {
			segment.Name = prefix + "_output_unknown"
			out = append(out, segment)
			continue
		}
		if firstOutput == nil {
			segment.Name = prefix + "_no_output"
			out = append(out, segment)
			continue
		}
		end := segment.StartUS + segment.DurationUS
		boundary := max(segment.StartUS, min(end, *firstOutput))
		if boundary > segment.StartUS {
			before := segment
			before.Name = prefix + "_before_output"
			before.DurationUS = boundary - segment.StartUS
			out = append(out, before)
		}
		if boundary < end {
			after := segment
			after.Name = prefix + "_after_output"
			after.StartUS = boundary
			after.DurationUS = end - boundary
			out = append(out, after)
		}
	}
	return out
}
