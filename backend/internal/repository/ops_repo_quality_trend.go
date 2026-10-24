package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// qualityTrendScope 返回 usage_logs / ops_error_logs 共用的时间 + 用户过滤条件。
// AllUsers=false 时强制 user_id 过滤,走 (user_id, created_at) 索引。
func qualityTrendScope(filter *service.OpsQualityTrendFilter) (string, []any, error) {
	if filter == nil {
		return "", nil, fmt.Errorf("nil filter")
	}
	where := "created_at >= $1 AND created_at < $2"
	args := []any{filter.StartTime.UTC(), filter.EndTime.UTC()}
	if !filter.AllUsers {
		if filter.UserID <= 0 {
			return "", nil, fmt.Errorf("user_id is required")
		}
		args = append(args, filter.UserID)
		where += " AND user_id = $3"
	}
	return where, args, nil
}

// GetQualityTrendUsageRaw 从 usage_logs 按时间桶返回成功请求数和总耗时 P50/P90(精确分位)。
func (r *opsRepository) GetQualityTrendUsageRaw(ctx context.Context, filter *service.OpsQualityTrendFilter) ([]service.OpsQualityTrendBucket, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	where, args, err := qualityTrendScope(filter)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
SELECT
  TO_CHAR(created_at, '%s') AS bucket,
  COUNT(*),
  percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL),
  percentile_cont(0.90) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL)
FROM usage_logs
WHERE %s
GROUP BY 1`, safeDateFormat(filter.Granularity), where)
	return r.queryQualityTrendBuckets(ctx, q, args...)
}

// GetQualityTrendUsagePreagg 从 ops_metrics_hourly 的全局行(platform/group 都为空)按时间桶返回成功数和总耗时 P50/P90。
// 按天时把各小时的分位按成功数加权平均,与 ops_metrics_daily 的近似口径一致。
func (r *opsRepository) GetQualityTrendUsagePreagg(ctx context.Context, startTime, endTime time.Time, granularity string) ([]service.OpsQualityTrendBucket, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	q := fmt.Sprintf(`
SELECT
  TO_CHAR(bucket_start, '%s') AS bucket,
  COALESCE(SUM(success_count), 0),
  SUM(duration_p50_ms::double precision * success_count) FILTER (WHERE duration_p50_ms IS NOT NULL)
    / NULLIF(SUM(success_count) FILTER (WHERE duration_p50_ms IS NOT NULL), 0),
  SUM(duration_p90_ms::double precision * success_count) FILTER (WHERE duration_p90_ms IS NOT NULL)
    / NULLIF(SUM(success_count) FILTER (WHERE duration_p90_ms IS NOT NULL), 0)
FROM ops_metrics_hourly
WHERE bucket_start >= $1 AND bucket_start < $2
  AND platform IS NULL
  AND group_id IS NULL
GROUP BY 1`, safeDateFormat(granularity))
	return r.queryQualityTrendBuckets(ctx, q, startTime.UTC(), endTime.UTC())
}

// GetQualityTrendFailures 从 ops_error_logs 按时间桶返回计入 SLA 的失败数:
// 客户端收到 >=400,排除 499(客户端断开)、业务限制和 count_tokens。
func (r *opsRepository) GetQualityTrendFailures(ctx context.Context, filter *service.OpsQualityTrendFilter) ([]service.OpsUserBucketCount, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	where, args, err := qualityTrendScope(filter)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
SELECT TO_CHAR(created_at, '%s') AS bucket, COUNT(*)
FROM ops_error_logs
WHERE %s
  AND COALESCE(status_code, 0) >= 400
  AND status_code <> 499
  AND is_business_limited = FALSE
  AND is_count_tokens = FALSE
GROUP BY 1`, safeDateFormat(filter.Granularity), where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.OpsUserBucketCount
	for rows.Next() {
		var row service.OpsUserBucketCount
		if err := rows.Scan(&row.Bucket, &row.Count); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *opsRepository) queryQualityTrendBuckets(ctx context.Context, q string, args ...any) ([]service.OpsQualityTrendBucket, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.OpsQualityTrendBucket
	for rows.Next() {
		var (
			row      service.OpsQualityTrendBucket
			p50, p90 sql.NullFloat64
		)
		if err := rows.Scan(&row.Bucket, &row.SuccessCount, &p50, &p90); err != nil {
			return nil, err
		}
		if p50.Valid {
			v := p50.Float64
			row.DurationP50Ms = &v
		}
		if p90.Valid {
			v := p90.Float64
			row.DurationP90Ms = &v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
