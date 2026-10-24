package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }

func TestBuildDashboardQualityTrend_SLAAndWeightedPercentiles(t *testing.T) {
	f := &OpsQualityTrendFilter{UserID: 42, Granularity: "day"}
	usage := []OpsQualityTrendBucket{
		// 同一天:预聚合 + 原始尾巴,分位按成功数加权
		{Bucket: "2026-10-24", SuccessCount: 300, DurationP50Ms: f64(1000), DurationP90Ms: f64(4000)},
		{Bucket: "2026-10-24", SuccessCount: 100, DurationP50Ms: f64(2000), DurationP90Ms: f64(8000)},
		{Bucket: "2026-10-23", SuccessCount: 99, DurationP50Ms: f64(1500), DurationP90Ms: nil},
	}
	failures := []OpsUserBucketCount{
		{Bucket: "2026-10-23", Count: 1},
		// 只有失败、没有成功的桶:SLA=0,没有耗时分位
		{Bucket: "2026-10-22", Count: 3},
	}

	out := buildDashboardQualityTrend(f, usage, failures)

	require.Len(t, out.Points, 3)
	require.Equal(t, "2026-10-22", out.Points[0].Bucket)
	require.InDelta(t, 0.0, *out.Points[0].SLA, 1e-9)
	require.Nil(t, out.Points[0].DurationP50Ms)

	require.Equal(t, int64(99), out.Points[1].Success)
	require.Equal(t, int64(1), out.Points[1].Failed)
	require.InDelta(t, 0.99, *out.Points[1].SLA, 1e-9)
	require.InDelta(t, 1500, *out.Points[1].DurationP50Ms, 1e-9)
	require.Nil(t, out.Points[1].DurationP90Ms)

	require.Equal(t, int64(400), out.Points[2].Success)
	require.InDelta(t, 1.0, *out.Points[2].SLA, 1e-9)
	require.InDelta(t, 1250, *out.Points[2].DurationP50Ms, 1e-9)
	require.InDelta(t, 5000, *out.Points[2].DurationP90Ms, 1e-9)
}

func TestBuildDashboardQualityTrend_EmptyHasEmptyPoints(t *testing.T) {
	out := buildDashboardQualityTrend(&OpsQualityTrendFilter{Granularity: "hour"}, nil, nil)
	require.NotNil(t, out.Points)
	require.Empty(t, out.Points)
}

// 用户侧也会拿到这个响应:新增任何字段都必须先过评审,确认不会泄露账号/上游等内部信息。
func TestDashboardQualityTrend_JSONWhitelist(t *testing.T) {
	b, err := json.Marshal(&DashboardQualityTrend{Points: []DashboardQualityTrendPoint{{Bucket: "2026-10-24"}}})
	require.NoError(t, err)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &top))
	require.Equal(t, []string{"end", "granularity", "points", "start"}, jsonObjectKeys(top))
	var points []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["points"], &points))
	require.Equal(t, []string{"bucket", "duration_p50_ms", "duration_p90_ms", "failed", "sla", "success"}, jsonObjectKeys(points[0]))
}

func TestGetDashboardQualityTrend_UserScopeUsesRawTables(t *testing.T) {
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	var usageFilter, failureFilter *OpsQualityTrendFilter
	repo := &opsRepoMock{
		GetQualityTrendUsageRawFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsQualityTrendBucket, error) {
			usageFilter = filter
			return []OpsQualityTrendBucket{{Bucket: "2026-10-24", SuccessCount: 9, DurationP50Ms: f64(800)}}, nil
		},
		GetQualityTrendFailuresFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsUserBucketCount, error) {
			failureFilter = filter
			return []OpsUserBucketCount{{Bucket: "2026-10-24", Count: 1}}, nil
		},
		GetQualityTrendUsagePreaggFn: func(ctx context.Context, startTime, endTime time.Time, granularity string) ([]OpsQualityTrendBucket, error) {
			t.Fatal("user scope must not read site-wide preaggregation")
			return nil, nil
		},
	}
	svc := &OpsService{opsRepo: repo}

	out, err := svc.GetDashboardQualityTrend(context.Background(), &OpsQualityTrendFilter{
		UserID: 42, StartTime: start, EndTime: start.Add(24 * time.Hour), Granularity: "minute",
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), usageFilter.UserID)
	require.False(t, usageFilter.AllUsers)
	require.Equal(t, "day", usageFilter.Granularity)
	require.Equal(t, int64(42), failureFilter.UserID)
	require.Len(t, out.Points, 1)
	require.InDelta(t, 0.9, *out.Points[0].SLA, 1e-9)
}

func TestGetDashboardQualityTrend_ValidatesScopeAndRange(t *testing.T) {
	svc := &OpsService{opsRepo: &opsRepoMock{}}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []*OpsQualityTrendFilter{
		nil,
		{StartTime: start, EndTime: start.Add(time.Hour)},                             // 既没有用户也不是全站
		{UserID: 42, AllUsers: true, StartTime: start, EndTime: start.Add(time.Hour)}, // 两者都设
		{UserID: 42, StartTime: start, EndTime: start},                                // 时间倒置
		{UserID: 42, StartTime: start, EndTime: start.Add(UserErrorStatsMaxRange + time.Hour)},
		{AllUsers: true, StartTime: start, EndTime: start.Add(AdminQualityTrendMaxRange + time.Hour)},
	}
	for i, f := range cases {
		_, err := svc.GetDashboardQualityTrend(context.Background(), f)
		require.Error(t, err, "case %d", i)
	}
}

func TestGetDashboardQualityTrend_AllUsersCombinesPreaggAndRawTail(t *testing.T) {
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	now := start.Add(10*time.Hour + 20*time.Minute)
	latestHour := start.Add(9 * time.Hour) // 预聚合覆盖到 10:00

	var preStart, preEnd time.Time
	var tailFilter *OpsQualityTrendFilter
	repo := &opsRepoMock{
		GetLatestHourlyBucketStartFn: func(ctx context.Context) (time.Time, bool, error) { return latestHour, true, nil },
		GetQualityTrendUsagePreaggFn: func(ctx context.Context, s, e time.Time, granularity string) ([]OpsQualityTrendBucket, error) {
			preStart, preEnd = s, e
			return []OpsQualityTrendBucket{{Bucket: "2026-10-24", SuccessCount: 900, DurationP50Ms: f64(1000)}}, nil
		},
		GetQualityTrendUsageRawFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsQualityTrendBucket, error) {
			tailFilter = filter
			return []OpsQualityTrendBucket{{Bucket: "2026-10-24", SuccessCount: 100, DurationP50Ms: f64(2000)}}, nil
		},
	}
	svc := &OpsService{opsRepo: repo}
	f := &OpsQualityTrendFilter{AllUsers: true, StartTime: start, EndTime: end, Granularity: "day"}

	rows, coveredEnd, err := svc.loadAllUsersQualityUsage(context.Background(), f, now)
	require.NoError(t, err)
	require.Equal(t, start, preStart)
	require.Equal(t, start.Add(10*time.Hour), preEnd)
	require.True(t, tailFilter.AllUsers)
	require.Equal(t, start.Add(10*time.Hour), tailFilter.StartTime)
	require.Equal(t, now, tailFilter.EndTime)
	require.Equal(t, now, coveredEnd)
	require.Len(t, rows, 2)

}

func TestGetDashboardQualityTrend_AllUsersPastRangeUsesPreaggOnly(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	var failureFilter *OpsQualityTrendFilter
	repo := &opsRepoMock{
		GetLatestHourlyBucketStartFn: func(ctx context.Context) (time.Time, bool, error) { return end.Add(30 * 24 * time.Hour), true, nil },
		GetQualityTrendUsagePreaggFn: func(ctx context.Context, s, e time.Time, granularity string) ([]OpsQualityTrendBucket, error) {
			require.Equal(t, start, s)
			require.Equal(t, end, e)
			return []OpsQualityTrendBucket{{Bucket: "2026-09-01", SuccessCount: 999, DurationP50Ms: f64(1000)}}, nil
		},
		GetQualityTrendUsageRawFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsQualityTrendBucket, error) {
			t.Fatal("fully preaggregated range must not scan usage_logs")
			return nil, nil
		},
		GetQualityTrendFailuresFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsUserBucketCount, error) {
			failureFilter = filter
			return []OpsUserBucketCount{{Bucket: "2026-09-01", Count: 1}}, nil
		},
	}
	svc := &OpsService{opsRepo: repo}

	out, err := svc.GetDashboardQualityTrend(context.Background(), &OpsQualityTrendFilter{AllUsers: true, StartTime: start, EndTime: end, Granularity: "day"})
	require.NoError(t, err)
	require.True(t, failureFilter.AllUsers)
	require.Equal(t, end, failureFilter.EndTime)
	require.Len(t, out.Points, 1)
	require.InDelta(t, 0.999, *out.Points[0].SLA, 1e-9)
}

func TestLoadAllUsersQualityUsage_StalePreaggSkipsRawScan(t *testing.T) {
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	repo := &opsRepoMock{
		GetLatestHourlyBucketStartFn: func(ctx context.Context) (time.Time, bool, error) { return start.Add(2 * time.Hour), true, nil },
		GetQualityTrendUsagePreaggFn: func(ctx context.Context, s, e time.Time, granularity string) ([]OpsQualityTrendBucket, error) {
			return []OpsQualityTrendBucket{{Bucket: "2026-10-24 01:00", SuccessCount: 5}}, nil
		},
		GetQualityTrendUsageRawFn: func(ctx context.Context, filter *OpsQualityTrendFilter) ([]OpsQualityTrendBucket, error) {
			t.Fatal("must not scan site-wide usage_logs when preaggregation is far behind")
			return nil, nil
		},
	}
	svc := &OpsService{opsRepo: repo}
	f := &OpsQualityTrendFilter{AllUsers: true, StartTime: start, EndTime: start.Add(24 * time.Hour), Granularity: "hour"}

	rows, coveredEnd, err := svc.loadAllUsersQualityUsage(context.Background(), f, start.Add(12*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, start.Add(3*time.Hour), coveredEnd, "failures beyond the covered window would otherwise show SLA 0")
}
