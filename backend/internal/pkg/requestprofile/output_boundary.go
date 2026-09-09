package requestprofile

// splitResponseBody refines only the non-overlapping wall-clock segments.
// The original spans remain untouched. A successful downstream output flush is
// the boundary, not an upstream delta, response headers, or a keepalive write.
// These labels describe observations; they never authorize a replay.
func splitResponseBody(segments []Segment, protocol string, parallel, supported bool, firstOutput *int64) []Segment {
	out := make([]Segment, 0, len(segments)+1)
	for _, segment := range segments {
		if segment.Name != "response_body" {
			out = append(out, segment)
			continue
		}
		if protocol != "sse" || parallel || !supported {
			segment.Name = "response_body_output_unknown"
			out = append(out, segment)
			continue
		}
		if firstOutput == nil {
			segment.Name = "response_body_no_output"
			out = append(out, segment)
			continue
		}
		end := segment.StartUS + segment.DurationUS
		boundary := max(segment.StartUS, min(end, *firstOutput))
		if boundary > segment.StartUS {
			before := segment
			before.Name = "response_body_before_output"
			before.DurationUS = boundary - segment.StartUS
			out = append(out, before)
		}
		if boundary < end {
			after := segment
			after.Name = "response_body_after_output"
			after.StartUS = boundary
			after.DurationUS = end - boundary
			out = append(out, after)
		}
	}
	return out
}
