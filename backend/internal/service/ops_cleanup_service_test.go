package service

import (
	"testing"
	"time"
)

func TestOpsCleanupPlan(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		days         int
		wantOK       bool
		wantTruncate bool
		wantCutoff   time.Time
	}{
		{name: "negative skips", days: -1, wantOK: false},
		{name: "zero truncates", days: 0, wantOK: true, wantTruncate: true},
		{name: "positive yields past cutoff", days: 7, wantOK: true, wantCutoff: now.AddDate(0, 0, -7)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cutoff, truncate, ok := opsCleanupPlan(now, tc.days)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if truncate != tc.wantTruncate {
				t.Fatalf("truncate = %v, want %v", truncate, tc.wantTruncate)
			}
			if !tc.wantTruncate && !cutoff.Equal(tc.wantCutoff) {
				t.Fatalf("cutoff = %v, want %v", cutoff, tc.wantCutoff)
			}
		})
	}
}

func TestIsMissingRelationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not missing", err: nil, want: false},
		{name: "match relation does not exist", err: fakeErr(`pq: relation "ops_error_logs" does not exist`), want: true},
		{name: "match case-insensitive", err: fakeErr(`ERROR: Relation "x" Does Not Exist`), want: true},
		{name: "non-matching error", err: fakeErr("connection refused"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingRelationError(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpsModelRollupRetentionIsFixed(t *testing.T) {
	if got := opsModel5mRetention / (5 * time.Minute); got != 4032 {
		t.Fatalf("5m retained bucket count = %d", got)
	}
	if got := opsModelHourlyRetention / time.Hour; got != 2160 {
		t.Fatalf("hourly retained bucket count = %d", got)
	}
}

func TestModelAggregationPolicy(t *testing.T) {
	cases := []struct {
		resolution int
		retention  time.Duration
		step       time.Duration
		chunk      time.Duration
	}{
		{300, opsModel5mRetention, 5 * time.Minute, 6 * time.Hour},
		{3600, opsModelHourlyRetention, time.Hour, 24 * time.Hour},
	}
	for _, tc := range cases {
		var retention, step, chunk time.Duration
		switch tc.resolution {
		case 300:
			retention, step, chunk = opsModel5mRetention, 5*time.Minute, 6*time.Hour
		case 3600:
			retention, step, chunk = opsModelHourlyRetention, time.Hour, 24*time.Hour
		}
		if retention != tc.retention || step != tc.step || chunk != tc.chunk {
			t.Fatalf("resolution %d policy=(%s,%s,%s)", tc.resolution, retention, step, chunk)
		}
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
