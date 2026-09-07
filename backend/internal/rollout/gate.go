package rollout

import (
	"fmt"
	"math"
)

const (
	DefaultMaxTTFTP50Increase = 0.30
	DefaultMaxSLADrop         = 0.005
	DefaultMaxCacheRateDrop   = 0.10
	DefaultMinCandidateCount  = int64(200)
	comparisonEpsilon         = 1e-12
)

// Metrics is the deployment-scoped, same-window snapshot consumed by the
// rollout controller. Rates use [0,1], while latencies use milliseconds.
type Metrics struct {
	Slot             string  `json:"slot"`
	TerminalOutcomes int64   `json:"terminal_outcomes"`
	Successes        int64   `json:"successes"`
	Failures         int64   `json:"failures"`
	TTFTSamples      int64   `json:"ttft_samples"`
	TTFTP50MS        float64 `json:"ttft_p50_ms"`
	TTFTP95MS        float64 `json:"ttft_p95_ms"`
	DurationP50MS    float64 `json:"duration_p50_ms"`
	DurationP95MS    float64 `json:"duration_p95_ms"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_creation_tokens"`
	ActualCostUSD    float64 `json:"actual_cost_usd"`
	Upstream429      int64   `json:"upstream_429"`
	Upstream529      int64   `json:"upstream_529"`
}

func (m Metrics) SLA() (float64, bool) {
	if m.TerminalOutcomes <= 0 || m.Successes < 0 || m.Failures < 0 || m.Successes+m.Failures != m.TerminalOutcomes {
		return 0, false
	}
	return float64(m.Successes) / float64(m.TerminalOutcomes), true
}

func (m Metrics) CacheRate() (float64, bool) {
	denominator := m.InputTokens + m.CacheReadTokens + m.CacheWriteTokens
	if denominator <= 0 || m.CacheReadTokens < 0 {
		return 0, false
	}
	return float64(m.CacheReadTokens) / float64(denominator), true
}

type Thresholds struct {
	MaxTTFTP50Increase float64 `json:"max_ttft_p50_increase"`
	MaxSLADrop         float64 `json:"max_sla_drop"`
	MaxCacheRateDrop   float64 `json:"max_cache_rate_drop"`
	MinCandidateCount  int64   `json:"min_candidate_terminal_outcomes"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		MaxTTFTP50Increase: DefaultMaxTTFTP50Increase,
		MaxSLADrop:         DefaultMaxSLADrop,
		MaxCacheRateDrop:   DefaultMaxCacheRateDrop,
		MinCandidateCount:  DefaultMinCandidateCount,
	}
}

type Check struct {
	Name      string  `json:"name"`
	Passed    bool    `json:"passed"`
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Limit     float64 `json:"limit"`
	Reason    string  `json:"reason,omitempty"`
}

type Evaluation struct {
	Passed bool    `json:"passed"`
	Reason string  `json:"reason,omitempty"`
	Checks []Check `json:"checks"`
}

func Evaluate(baseline, candidate Metrics, thresholds Thresholds) Evaluation {
	result := Evaluation{Checks: make([]Check, 0, 3)}
	if err := validateThresholds(thresholds); err != nil {
		result.Reason = err.Error()
		return result
	}
	if baseline.Slot == "" || candidate.Slot == "" || baseline.Slot == candidate.Slot {
		result.Reason = "baseline and candidate slots must be distinct and non-empty"
		return result
	}
	if candidate.TerminalOutcomes < thresholds.MinCandidateCount {
		result.Reason = fmt.Sprintf("candidate terminal outcomes %d below minimum %d", candidate.TerminalOutcomes, thresholds.MinCandidateCount)
		return result
	}
	if baseline.TTFTSamples <= 0 || candidate.TTFTSamples <= 0 || baseline.TTFTP50MS <= 0 || candidate.TTFTP50MS <= 0 {
		result.Reason = "missing TTFT samples"
		return result
	}

	baselineSLA, baselineSLAOK := baseline.SLA()
	candidateSLA, candidateSLAOK := candidate.SLA()
	baselineCache, baselineCacheOK := baseline.CacheRate()
	candidateCache, candidateCacheOK := candidate.CacheRate()
	if !baselineSLAOK || !candidateSLAOK {
		result.Reason = "missing or inconsistent terminal outcomes"
		return result
	}
	if !baselineCacheOK || !candidateCacheOK {
		result.Reason = "missing token usage for cache rate"
		return result
	}

	ttftLimit := baseline.TTFTP50MS * (1 + thresholds.MaxTTFTP50Increase)
	slaLimit := baselineSLA - thresholds.MaxSLADrop
	cacheLimit := baselineCache - thresholds.MaxCacheRateDrop
	result.Checks = append(result.Checks,
		newUpperBoundCheck("ttft_p50", baseline.TTFTP50MS, candidate.TTFTP50MS, ttftLimit),
		newLowerBoundCheck("sla", baselineSLA, candidateSLA, slaLimit),
		newLowerBoundCheck("cache_rate", baselineCache, candidateCache, cacheLimit),
	)

	result.Passed = true
	for _, check := range result.Checks {
		if !check.Passed {
			result.Passed = false
		}
	}
	if !result.Passed {
		result.Reason = "one or more rollout gates failed"
	}
	return result
}

func validateThresholds(t Thresholds) error {
	if t.MaxTTFTP50Increase < 0 || t.MaxSLADrop < 0 || t.MaxCacheRateDrop < 0 || t.MinCandidateCount <= 0 {
		return fmt.Errorf("invalid rollout thresholds")
	}
	return nil
}

func newUpperBoundCheck(name string, baseline, candidate, limit float64) Check {
	check := Check{Name: name, Baseline: baseline, Candidate: candidate, Limit: limit}
	check.Passed = finite(candidate) && candidate <= limit+comparisonEpsilon
	if !check.Passed {
		check.Reason = fmt.Sprintf("candidate %.6f exceeds maximum %.6f", candidate, limit)
	}
	return check
}

func newLowerBoundCheck(name string, baseline, candidate, limit float64) Check {
	check := Check{Name: name, Baseline: baseline, Candidate: candidate, Limit: limit}
	check.Passed = finite(candidate) && candidate+comparisonEpsilon >= limit
	if !check.Passed {
		check.Reason = fmt.Sprintf("candidate %.6f below minimum %.6f", candidate, limit)
	}
	return check
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
