package service

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func userErrorStatsTestFilter() *OpsUserErrorStatsFilter {
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	return &OpsUserErrorStatsFilter{UserID: 42, StartTime: start, EndTime: start.Add(24 * time.Hour), Granularity: "hour"}
}

func TestBuildUserErrorStats_CountsOnlyRealFailures(t *testing.T) {
	raw := &OpsUserErrorStatsRaw{
		Success: []OpsUserBucketCount{
			{Bucket: "2026-10-24 01:00", Count: 90},
			{Bucket: "2026-10-24 02:00", Count: 5},
		},
		Errors: []OpsUserErrorBucketRow{
			{Bucket: "2026-10-24 01:00", StatusCode: 502, Count: 3},
			{Bucket: "2026-10-24 02:00", StatusCode: 400, Count: 2},
			// 余额不足 / 用户自身并发限流:业务限制,不计入
			{Bucket: "2026-10-24 01:00", StatusCode: 403, BusinessLimited: true, Count: 21},
			{Bucket: "2026-10-24 02:00", StatusCode: 429, BusinessLimited: true, Count: 4},
			// 客户端断开:不计入
			{Bucket: "2026-10-24 02:00", StatusCode: 499, Count: 1},
			// 池内换号后最终成功:不是失败
			{Bucket: "2026-10-24 01:00", StatusCode: 200, Count: 50},
			// 上游 429 没被救回来:算失败
			{Bucket: "2026-10-24 03:00", StatusCode: 429, Count: 1},
		},
	}

	out := buildUserErrorStats(userErrorStatsTestFilter(), raw)

	require.Equal(t, int64(95), out.Success)
	require.Equal(t, int64(6), out.Failed)
	require.Equal(t, int64(101), out.Total)
	require.InDelta(t, 6.0/101.0, out.FailureRate, 1e-9)
	require.Equal(t, UserErrorStatsExcluded{BusinessLimited: 25, ClientClosed: 1}, out.Excluded)
	require.Equal(t, []UserErrorStatusCount{
		{StatusCode: 502, Count: 3},
		{StatusCode: 400, Count: 2},
		{StatusCode: 429, Count: 1},
	}, out.ByStatus)
	require.Equal(t, []UserErrorStatsPoint{
		{Bucket: "2026-10-24 01:00", Total: 93, Failed: 3},
		{Bucket: "2026-10-24 02:00", Total: 7, Failed: 2},
		{Bucket: "2026-10-24 03:00", Total: 1, Failed: 1},
	}, out.Trend)
}

func TestBuildUserErrorStats_EmptyHasZeroRateAndEmptyArrays(t *testing.T) {
	out := buildUserErrorStats(userErrorStatsTestFilter(), &OpsUserErrorStatsRaw{})
	require.Zero(t, out.Total)
	require.Zero(t, out.FailureRate)
	require.NotNil(t, out.ByStatus)
	require.NotNil(t, out.Trend)
}

// 用户侧响应是白名单:新增任何字段都必须先过评审,确认不会泄露账号/上游等内部信息。
func TestUserErrorStats_JSONWhitelist(t *testing.T) {
	out := &UserErrorStats{
		ByStatus: []UserErrorStatusCount{{StatusCode: 502, Count: 1}},
		Trend:    []UserErrorStatsPoint{{Bucket: "2026-10-24", Total: 1, Failed: 1}},
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &top))
	require.Equal(t, []string{"by_status", "end", "excluded", "failed", "failure_rate", "granularity", "start", "success", "total", "trend"}, jsonObjectKeys(top))

	var byStatus []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["by_status"], &byStatus))
	require.Equal(t, []string{"count", "status_code"}, jsonObjectKeys(byStatus[0]))

	var trend []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["trend"], &trend))
	require.Equal(t, []string{"bucket", "failed", "total"}, jsonObjectKeys(trend[0]))

	var excluded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(top["excluded"], &excluded))
	require.Equal(t, []string{"business_limited", "client_closed"}, jsonObjectKeys(excluded))
}

func jsonObjectKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestGetUserErrorStats_ValidatesAndPassesUserScope(t *testing.T) {
	var got *OpsUserErrorStatsFilter
	repo := &opsRepoMock{GetUserErrorStatsRawFn: func(ctx context.Context, filter *OpsUserErrorStatsFilter) (*OpsUserErrorStatsRaw, error) {
		got = filter
		return &OpsUserErrorStatsRaw{}, nil
	}}
	svc := &OpsService{opsRepo: repo}

	f := userErrorStatsTestFilter()
	f.Granularity = "minute"
	_, err := svc.GetUserErrorStats(context.Background(), f)
	require.NoError(t, err)
	require.Equal(t, int64(42), got.UserID)
	require.Equal(t, "day", got.Granularity, "unknown granularity falls back to day")

	noUser := userErrorStatsTestFilter()
	noUser.UserID = 0
	_, err = svc.GetUserErrorStats(context.Background(), noUser)
	require.Error(t, err)

	tooWide := userErrorStatsTestFilter()
	tooWide.EndTime = tooWide.StartTime.Add(UserErrorStatsMaxRange + time.Hour)
	_, err = svc.GetUserErrorStats(context.Background(), tooWide)
	require.Error(t, err)

	inverted := userErrorStatsTestFilter()
	inverted.EndTime = inverted.StartTime
	_, err = svc.GetUserErrorStats(context.Background(), inverted)
	require.Error(t, err)
}
