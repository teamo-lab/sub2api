package service

import "context"

type openAILocalCapacityProbeKey struct{}

// WithOpenAILocalCapacityProbe prevents a candidate search from changing the
// conversation binding before that candidate has actually admitted the request.
// It carries no deadline or quota state; the original request remains its parent.
func WithOpenAILocalCapacityProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAILocalCapacityProbeKey{}, true)
}

func openAILocalCapacityProbe(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	probe, _ := ctx.Value(openAILocalCapacityProbeKey{}).(bool)
	return probe
}
