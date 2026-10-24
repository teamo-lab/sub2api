package service

import (
	"context"
	"sort"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// AdminQualityTrendMaxRange 是管理员大盘质量趋势允许的最大时间跨度。
	// 成功侧走 ops_metrics_hourly 预聚合,失败侧按 created_at 扫 ops_error_logs。
	AdminQualityTrendMaxRange = 93 * 24 * time.Hour

	// opsQualityTrendRawTailMax: 预聚合只算到上一个整点,之后到现在的这一段直接扫 usage_logs。
	// 这段超过该跨度说明预聚合停了,不再扫全站原始表兜底,图上就停在预聚合覆盖到的位置。
	opsQualityTrendRawTailMax = 3 * time.Hour
)

// OpsQualityTrendFilter 是仪表盘「总耗时 P50/P90 + SLA」趋势的查询条件。
// UserID>0 只看该用户;AllUsers=true 看全站(仅管理员入口可设),两者必须二选一。
type OpsQualityTrendFilter struct {
	UserID      int64
	AllUsers    bool
	StartTime   time.Time
	EndTime     time.Time
	Granularity string // "hour" | "day"
}

// OpsQualityTrendBucket 是仓储层按时间桶返回的成功请求数与总耗时分位(ms)。
type OpsQualityTrendBucket struct {
	Bucket        string
	SuccessCount  int64
	DurationP50Ms *float64
	DurationP90Ms *float64
}

// DashboardQualityTrendPoint 只含计数、SLA 和耗时分位,不含任何账号/分组/上游信息,用户侧可直接返回。
type DashboardQualityTrendPoint struct {
	Bucket        string   `json:"bucket"`
	Success       int64    `json:"success"`
	Failed        int64    `json:"failed"`
	SLA           *float64 `json:"sla"` // 0~1;该桶没有样本时为 null
	DurationP50Ms *float64 `json:"duration_p50_ms"`
	DurationP90Ms *float64 `json:"duration_p90_ms"`
}

type DashboardQualityTrend struct {
	StartTime   time.Time                    `json:"start"`
	EndTime     time.Time                    `json:"end"`
	Granularity string                       `json:"granularity"`
	Points      []DashboardQualityTrendPoint `json:"points"`
}

// GetDashboardQualityTrend 返回仪表盘的总耗时 P50/P90 与 SLA 趋势。
//
// 口径与使用记录页原「请求失败率」一致:
// 成功 = usage_logs;失败 = ops_error_logs 中客户端收到 >=400,且不是 499、不是业务限制、不是 count_tokens;
// SLA = 成功 /(成功 + 失败);总耗时分位只看成功请求的 duration_ms。
//
// 用户:直接按 (user_id, created_at) 扫原始表,分位是精确值。
// 全站:成功侧读 ops_metrics_hourly 的全局行(按天时 P50/P90 是各小时按成功数加权的近似值,
// 与 ops_metrics_daily 同一近似),预聚合之后的尾巴扫原始表补齐。
func (s *OpsService) GetDashboardQualityTrend(ctx context.Context, filter *OpsQualityTrendFilter) (*DashboardQualityTrend, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if filter == nil || (filter.UserID <= 0) == !filter.AllUsers {
		return nil, infraerrors.BadRequest("INVALID_USER", "exactly one of user or all users is required")
	}
	if filter.StartTime.IsZero() || filter.EndTime.IsZero() || !filter.EndTime.After(filter.StartTime) {
		return nil, infraerrors.BadRequest("INVALID_TIME_RANGE", "invalid time range")
	}
	maxRange := UserErrorStatsMaxRange
	if filter.AllUsers {
		maxRange = AdminQualityTrendMaxRange
	}
	if filter.EndTime.Sub(filter.StartTime) > maxRange {
		return nil, infraerrors.BadRequest("TIME_RANGE_TOO_LARGE", "time range is too large")
	}
	if s.opsRepo == nil {
		return nil, ErrOpsDisabled
	}
	f := *filter
	if f.Granularity != "hour" {
		f.Granularity = "day"
	}

	var usage []OpsQualityTrendBucket
	failureFilter := f
	if f.AllUsers {
		rows, coveredEnd, err := s.loadAllUsersQualityUsage(ctx, &f, time.Now())
		if err != nil {
			return nil, infraerrors.InternalServer("OPS_QUALITY_TREND_FAILED", "Failed to load quality trend").WithCause(err)
		}
		usage = rows
		// 成功侧没覆盖到的时段不算失败,否则这些桶的 SLA 会被算成 0。
		failureFilter.EndTime = coveredEnd
	} else {
		rows, err := s.opsRepo.GetQualityTrendUsageRaw(ctx, &f)
		if err != nil {
			return nil, infraerrors.InternalServer("OPS_QUALITY_TREND_FAILED", "Failed to load quality trend").WithCause(err)
		}
		usage = rows
	}

	var failures []OpsUserBucketCount
	if failureFilter.EndTime.After(failureFilter.StartTime) {
		rows, err := s.opsRepo.GetQualityTrendFailures(ctx, &failureFilter)
		if err != nil {
			return nil, infraerrors.InternalServer("OPS_QUALITY_TREND_FAILED", "Failed to load quality trend").WithCause(err)
		}
		failures = rows
	}
	return buildDashboardQualityTrend(&f, usage, failures), nil
}

// loadAllUsersQualityUsage 读全站成功侧:预聚合覆盖的部分走 ops_metrics_hourly,
// 之后到 now 的尾巴(不超过 opsQualityTrendRawTailMax)扫 usage_logs。返回成功侧实际覆盖到的结束时间。
func (s *OpsService) loadAllUsersQualityUsage(ctx context.Context, f *OpsQualityTrendFilter, now time.Time) ([]OpsQualityTrendBucket, time.Time, error) {
	coveredUntil := f.StartTime
	latest, ok, err := s.opsRepo.GetLatestHourlyBucketStart(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	if ok && latest.Add(time.Hour).After(coveredUntil) {
		coveredUntil = latest.Add(time.Hour)
	}

	var rows []OpsQualityTrendBucket
	coveredEnd := f.StartTime
	if preEnd := minTime(f.EndTime, coveredUntil); preEnd.After(f.StartTime) {
		pre, err := s.opsRepo.GetQualityTrendUsagePreagg(ctx, f.StartTime, preEnd, f.Granularity)
		if err != nil {
			return nil, time.Time{}, err
		}
		rows = append(rows, pre...)
		coveredEnd = preEnd
	}

	tailStart := coveredEnd
	tailEnd := minTime(f.EndTime, now)
	if tailEnd.After(tailStart) && tailEnd.Sub(tailStart) <= opsQualityTrendRawTailMax {
		tail := *f
		tail.StartTime = tailStart
		tail.EndTime = tailEnd
		raw, err := s.opsRepo.GetQualityTrendUsageRaw(ctx, &tail)
		if err != nil {
			return nil, time.Time{}, err
		}
		rows = append(rows, raw...)
		coveredEnd = tailEnd
	}
	return rows, coveredEnd, nil
}

func buildDashboardQualityTrend(f *OpsQualityTrendFilter, usage []OpsQualityTrendBucket, failures []OpsUserBucketCount) *DashboardQualityTrend {
	out := &DashboardQualityTrend{
		StartTime:   f.StartTime,
		EndTime:     f.EndTime,
		Granularity: f.Granularity,
		Points:      []DashboardQualityTrendPoint{},
	}

	type acc struct {
		point             DashboardQualityTrendPoint
		p50Sum, p50Weight float64
		p90Sum, p90Weight float64
	}
	buckets := map[string]*acc{}
	bucket := func(key string) *acc {
		a, ok := buckets[key]
		if !ok {
			a = &acc{point: DashboardQualityTrendPoint{Bucket: key}}
			buckets[key] = a
		}
		return a
	}

	// 同一个桶可能同时来自预聚合和原始尾巴(按天时),分位按成功数加权合并。
	for _, row := range usage {
		a := bucket(row.Bucket)
		a.point.Success += row.SuccessCount
		weight := float64(row.SuccessCount)
		if weight <= 0 {
			continue
		}
		if row.DurationP50Ms != nil {
			a.p50Sum += *row.DurationP50Ms * weight
			a.p50Weight += weight
		}
		if row.DurationP90Ms != nil {
			a.p90Sum += *row.DurationP90Ms * weight
			a.p90Weight += weight
		}
	}
	for _, row := range failures {
		bucket(row.Bucket).point.Failed += row.Count
	}

	for _, a := range buckets {
		p := a.point
		if total := p.Success + p.Failed; total > 0 {
			sla := float64(p.Success) / float64(total)
			p.SLA = &sla
		}
		if a.p50Weight > 0 {
			v := a.p50Sum / a.p50Weight
			p.DurationP50Ms = &v
		}
		if a.p90Weight > 0 {
			v := a.p90Sum / a.p90Weight
			p.DurationP90Ms = &v
		}
		out.Points = append(out.Points, p)
	}
	sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].Bucket < out.Points[j].Bucket })
	return out
}
