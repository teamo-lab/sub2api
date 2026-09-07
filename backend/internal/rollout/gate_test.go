package rollout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validMetrics(slot string) Metrics {
	return Metrics{
		Slot:             slot,
		TerminalOutcomes: 200,
		Successes:        198,
		Failures:         2,
		TTFTSamples:      180,
		TTFTP50MS:        1000,
		InputTokens:      10,
		CacheReadTokens:  80,
		CacheWriteTokens: 10,
	}
}

func TestEvaluateExactBoundariesPass(t *testing.T) {
	baseline := validMetrics("blue")
	candidate := validMetrics("green")
	candidate.TTFTP50MS = 1300
	candidate.Successes = 197
	candidate.Failures = 3 // 98.5%, exactly 0.5 points below the 99% baseline.
	candidate.CacheReadTokens = 70
	candidate.InputTokens = 20
	candidate.CacheWriteTokens = 10 // 70%, exactly 10 points below 80%.

	result := Evaluate(baseline, candidate, DefaultThresholds())
	require.True(t, result.Passed, result)
	require.Len(t, result.Checks, 3)
}

func TestEvaluateSmallBoundaryRegressionFails(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{name: "ttft", mutate: func(m *Metrics) { m.TTFTP50MS = 1300.001 }},
		{name: "sla", mutate: func(m *Metrics) { m.Successes = 196; m.Failures = 4 }},
		{name: "cache", mutate: func(m *Metrics) { m.CacheReadTokens = 699; m.InputTokens = 201; m.CacheWriteTokens = 100 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := validMetrics("blue")
			candidate := validMetrics("green")
			tt.mutate(&candidate)
			result := Evaluate(baseline, candidate, DefaultThresholds())
			require.False(t, result.Passed, result)
		})
	}
}

func TestEvaluateFailsClosedOnMissingOrInvalidSamples(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Metrics)
	}{
		{name: "too few terminal outcomes", mutate: func(m *Metrics) { m.TerminalOutcomes = 199; m.Successes = 197 }},
		{name: "missing ttft", mutate: func(m *Metrics) { m.TTFTSamples = 0 }},
		{name: "missing cache denominator", mutate: func(m *Metrics) { m.InputTokens = 0; m.CacheReadTokens = 0; m.CacheWriteTokens = 0 }},
		{name: "inconsistent outcomes", mutate: func(m *Metrics) { m.Failures = 3 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := validMetrics("blue")
			candidate := validMetrics("green")
			tt.mutate(&candidate)
			require.False(t, Evaluate(baseline, candidate, DefaultThresholds()).Passed)
		})
	}
}

func TestMetricsUsePercentagePointsAndTokenWeighting(t *testing.T) {
	metrics := validMetrics("blue")
	sla, ok := metrics.SLA()
	require.True(t, ok)
	require.InDelta(t, 0.99, sla, 1e-12)

	cacheRate, ok := metrics.CacheRate()
	require.True(t, ok)
	require.InDelta(t, 0.80, cacheRate, 1e-12)
}
