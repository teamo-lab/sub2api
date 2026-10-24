package service

import (
	"context"
	"sort"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// UserErrorStatsMaxRange 是用户侧失败率统计允许的最大时间跨度。
// 查询强制带 user_id 走 (user_id, created_at) 索引,31 天足够覆盖使用记录页的常用区间。
const UserErrorStatsMaxRange = 31 * 24 * time.Hour

// OpsUserErrorStatsFilter 是用户侧失败率统计的查询条件。UserID 必填且由 handler 从登录态写入。
type OpsUserErrorStatsFilter struct {
	UserID      int64
	APIKeyID    int64
	GroupID     int64
	Model       string
	StartTime   time.Time
	EndTime     time.Time
	Granularity string // "hour" | "day"
}

// OpsUserBucketCount 是 usage_logs 按时间桶的成功请求数。
type OpsUserBucketCount struct {
	Bucket string
	Count  int64
}

// OpsUserErrorBucketRow 是 ops_error_logs 按时间桶 + 客户端状态码 + 是否业务限制的计数。
type OpsUserErrorBucketRow struct {
	Bucket          string
	StatusCode      int
	BusinessLimited bool
	Count           int64
}

// OpsUserErrorStatsRaw 是仓储层返回的原始计数,口径判断统一在 service 层做。
type OpsUserErrorStatsRaw struct {
	Success []OpsUserBucketCount
	Errors  []OpsUserErrorBucketRow
}

// UserErrorStats 是面向终端用户的失败率统计(白名单)。
// 只含计数和客户端实际收到的状态码;严禁加入报错文案、账号、分组、上游状态码等任何内部字段。
type UserErrorStats struct {
	StartTime   time.Time              `json:"start"`
	EndTime     time.Time              `json:"end"`
	Granularity string                 `json:"granularity"`
	Total       int64                  `json:"total"`
	Success     int64                  `json:"success"`
	Failed      int64                  `json:"failed"`
	FailureRate float64                `json:"failure_rate"`
	ByStatus    []UserErrorStatusCount `json:"by_status"`
	Trend       []UserErrorStatsPoint  `json:"trend"`
	Excluded    UserErrorStatsExcluded `json:"excluded"`
}

type UserErrorStatusCount struct {
	StatusCode int   `json:"status_code"`
	Count      int64 `json:"count"`
}

type UserErrorStatsPoint struct {
	Bucket string `json:"bucket"`
	Total  int64  `json:"total"`
	Failed int64  `json:"failed"`
}

// UserErrorStatsExcluded 是不计入失败率的请求数(分子分母都不计)。
type UserErrorStatsExcluded struct {
	// BusinessLimited: 余额不足、用户自身并发/限额等账户层面的限制(ops_error_logs.is_business_limited)。
	BusinessLimited int64 `json:"business_limited"`
	// ClientClosed: 客户端主动断开(499)。
	ClientClosed int64 `json:"client_closed"`
}

const opsStatusClientClosed = 499

// GetUserErrorStats 返回某用户自己的失败率与状态码分布。
// 失败 = status_code>=400,且不是 499、不是业务限制、不是 count_tokens;
// 成功 = usage_logs;失败率 = 失败 /(成功 + 失败)。
// 池内换号后最终成功的上游错误(status<400)不计入。
func (s *OpsService) GetUserErrorStats(ctx context.Context, filter *OpsUserErrorStatsFilter) (*UserErrorStats, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if filter == nil || filter.UserID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_USER", "user is required")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() || !filter.EndTime.After(filter.StartTime) {
		return nil, infraerrors.BadRequest("INVALID_TIME_RANGE", "invalid time range")
	}
	if filter.EndTime.Sub(filter.StartTime) > UserErrorStatsMaxRange {
		return nil, infraerrors.BadRequest("TIME_RANGE_TOO_LARGE", "time range must not exceed 31 days")
	}
	f := *filter
	if f.Granularity != "hour" {
		f.Granularity = "day"
	}
	if s.opsRepo == nil {
		return nil, ErrOpsDisabled
	}

	raw, err := s.opsRepo.GetUserErrorStatsRaw(ctx, &f)
	if err != nil {
		return nil, infraerrors.InternalServer("OPS_USER_ERROR_STATS_FAILED", "Failed to load error stats").WithCause(err)
	}
	return buildUserErrorStats(&f, raw), nil
}

func buildUserErrorStats(f *OpsUserErrorStatsFilter, raw *OpsUserErrorStatsRaw) *UserErrorStats {
	out := &UserErrorStats{
		StartTime:   f.StartTime,
		EndTime:     f.EndTime,
		Granularity: f.Granularity,
		ByStatus:    []UserErrorStatusCount{},
		Trend:       []UserErrorStatsPoint{},
	}
	if raw == nil {
		return out
	}

	buckets := map[string]*UserErrorStatsPoint{}
	bucket := func(key string) *UserErrorStatsPoint {
		p, ok := buckets[key]
		if !ok {
			p = &UserErrorStatsPoint{Bucket: key}
			buckets[key] = p
		}
		return p
	}

	for _, row := range raw.Success {
		out.Success += row.Count
		bucket(row.Bucket).Total += row.Count
	}

	byStatus := map[int]int64{}
	for _, row := range raw.Errors {
		switch {
		case row.StatusCode < 400:
			continue
		case row.StatusCode == opsStatusClientClosed:
			out.Excluded.ClientClosed += row.Count
		case row.BusinessLimited:
			out.Excluded.BusinessLimited += row.Count
		default:
			out.Failed += row.Count
			byStatus[row.StatusCode] += row.Count
			p := bucket(row.Bucket)
			p.Total += row.Count
			p.Failed += row.Count
		}
	}

	out.Total = out.Success + out.Failed
	if out.Total > 0 {
		out.FailureRate = float64(out.Failed) / float64(out.Total)
	}

	for code, n := range byStatus {
		out.ByStatus = append(out.ByStatus, UserErrorStatusCount{StatusCode: code, Count: n})
	}
	sort.Slice(out.ByStatus, func(i, j int) bool {
		if out.ByStatus[i].Count != out.ByStatus[j].Count {
			return out.ByStatus[i].Count > out.ByStatus[j].Count
		}
		return out.ByStatus[i].StatusCode < out.ByStatus[j].StatusCode
	})

	for _, p := range buckets {
		out.Trend = append(out.Trend, *p)
	}
	sort.Slice(out.Trend, func(i, j int) bool { return out.Trend[i].Bucket < out.Trend[j].Bucket })
	return out
}
