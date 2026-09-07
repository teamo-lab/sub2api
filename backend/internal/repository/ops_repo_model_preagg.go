package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	opsModelResolution5m     = 300
	opsModelResolutionHourly = 3600
)

func (r *opsRepository) UpsertModelMetrics5m(ctx context.Context, startTime, endTime time.Time) error {
	return r.upsertModelMetrics(ctx, "ops_model_metrics_5m", opsModelResolution5m, startTime, endTime)
}

func (r *opsRepository) UpsertModelMetricsHourly(ctx context.Context, startTime, endTime time.Time) error {
	return r.upsertModelMetrics(ctx, "ops_model_metrics_hourly", opsModelResolutionHourly, startTime, endTime)
}

func (r *opsRepository) upsertModelMetrics(ctx context.Context, table string, resolution int, startTime, endTime time.Time) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil ops repository")
	}
	if !endTime.After(startTime) {
		return nil
	}
	if (table != "ops_model_metrics_5m" && table != "ops_model_metrics_hourly") || (resolution != 300 && resolution != 3600) {
		return fmt.Errorf("invalid model rollup target")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	start, end := startTime.UTC(), endTime.UTC()
	if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE bucket_start >= $1 AND bucket_start < $2", start, end); err != nil {
		return err
	}

	q := fmt.Sprintf(modelMetricsInsertSQL, table)
	if _, err = tx.ExecContext(ctx, q, start, end, resolution); err != nil {
		return err
	}

	// Mark every complete bucket, including buckets with no model traffic. The
	// metrics replacement and its completion markers commit atomically.
	coverage := `
INSERT INTO ops_model_metrics_coverage (resolution_seconds, bucket_start, completed_at)
SELECT $3, bucket, NOW()
FROM generate_series($1::timestamptz, $2::timestamptz - ($3 * interval '1 second'), $3 * interval '1 second') AS bucket
ON CONFLICT (resolution_seconds, bucket_start) DO UPDATE SET completed_at = EXCLUDED.completed_at`
	if _, err = tx.ExecContext(ctx, coverage, start, end, resolution); err != nil {
		return err
	}
	return tx.Commit()
}

// Model rows use the same effective requested-model identity as dashboard
// filtering, with a legacy fallback to model. Only three useful granularities
// are emitted to avoid multiplying table size.
const modelMetricsInsertSQL = `
WITH usage_base AS (
  SELECT to_timestamp(floor(extract(epoch FROM ul.created_at) / $3) * $3) AS bucket_start,
         btrim(COALESCE(NULLIF(btrim(ul.requested_model), ''), ul.model)) AS model,
         COALESCE(NULLIF(g.platform, ''), NULLIF(a.platform, ''), 'unknown') AS platform,
         ul.group_id, ul.duration_ms, ul.first_token_ms,
         ul.input_tokens + ul.output_tokens + ul.cache_creation_tokens + ul.cache_read_tokens AS tokens
  FROM usage_logs ul
  LEFT JOIN groups g ON g.id = ul.group_id
  LEFT JOIN accounts a ON a.id = ul.account_id
  WHERE ul.created_at >= $1 AND ul.created_at < $2
    AND btrim(COALESCE(NULLIF(btrim(ul.requested_model), ''), ul.model)) <> ''
), usage_agg AS (
  SELECT bucket_start, model,
         CASE WHEN GROUPING(platform)=1 THEN NULL ELSE platform END AS platform,
         CASE WHEN GROUPING(group_id)=1 THEN NULL ELSE group_id END AS group_id,
         COUNT(*) success_count, COUNT(*) FILTER (WHERE first_token_ms IS NOT NULL) ttft_sample_count,
         COALESCE(SUM(tokens),0) token_consumed,
         percentile_cont(.5) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL) duration_p50,
         percentile_cont(.9) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL) duration_p90,
         percentile_cont(.95) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL) duration_p95,
         percentile_cont(.99) WITHIN GROUP (ORDER BY duration_ms) FILTER (WHERE duration_ms IS NOT NULL) duration_p99,
         AVG(duration_ms) FILTER (WHERE duration_ms IS NOT NULL) duration_avg, MAX(duration_ms) duration_max,
         percentile_cont(.5) WITHIN GROUP (ORDER BY first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL) ttft_p50,
         percentile_cont(.9) WITHIN GROUP (ORDER BY first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL) ttft_p90,
         percentile_cont(.95) WITHIN GROUP (ORDER BY first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL) ttft_p95,
         percentile_cont(.99) WITHIN GROUP (ORDER BY first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL) ttft_p99,
         AVG(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL) ttft_avg, MAX(first_token_ms) ttft_max,
         COUNT(*) FILTER (WHERE duration_ms < 100) latency_0_100,
         COUNT(*) FILTER (WHERE duration_ms >= 100 AND duration_ms < 200) latency_100_200,
         COUNT(*) FILTER (WHERE duration_ms >= 200 AND duration_ms < 500) latency_200_500,
         COUNT(*) FILTER (WHERE duration_ms >= 500 AND duration_ms < 1000) latency_500_1000,
         COUNT(*) FILTER (WHERE duration_ms >= 1000 AND duration_ms < 2000) latency_1000_2000,
         COUNT(*) FILTER (WHERE duration_ms >= 2000) latency_2000_plus
  FROM usage_base GROUP BY GROUPING SETS ((bucket_start,model),(bucket_start,model,platform),(bucket_start,model,platform,group_id))
  HAVING GROUPING(group_id)=1 OR group_id IS NOT NULL
), error_base AS (
  SELECT to_timestamp(floor(extract(epoch FROM created_at) / $3) * $3) AS bucket_start,
         btrim(COALESCE(NULLIF(btrim(requested_model),''),model)) AS model, COALESCE(NULLIF(platform,''),'unknown') platform,
         group_id, is_business_limited, error_owner, status_code,
         COALESCE(upstream_status_code,status_code,0) effective_status_code, upstream_errors
  FROM ops_error_logs WHERE created_at >= $1 AND created_at < $2 AND is_count_tokens=FALSE
    AND btrim(COALESCE(NULLIF(btrim(requested_model),''),model)) <> ''
), error_agg AS (
  SELECT bucket_start,model,
         CASE WHEN GROUPING(platform)=1 THEN NULL ELSE platform END platform,
         CASE WHEN GROUPING(group_id)=1 THEN NULL ELSE group_id END group_id,
         COUNT(*) FILTER (WHERE COALESCE(status_code,0)>=400) error_count_total,
         COUNT(*) FILTER (WHERE COALESCE(status_code,0)>=400 AND is_business_limited) business_limited_count,
         COUNT(*) FILTER (WHERE COALESCE(status_code,0)>=400 AND NOT is_business_limited) error_count_sla,
         COUNT(*) FILTER (WHERE error_owner='provider' AND NOT is_business_limited AND effective_status_code NOT IN (429,529)) upstream_excl,
         COUNT(*) FILTER (WHERE error_owner='provider' AND NOT is_business_limited AND effective_status_code=429) upstream_429,
         COUNT(*) FILTER (WHERE error_owner='provider' AND NOT is_business_limited AND effective_status_code=529) upstream_529,
         COALESCE(SUM((SELECT COUNT(*) FROM jsonb_array_elements(COALESCE(NULLIF(upstream_errors,'null'::jsonb),'[]'::jsonb)) ev
           WHERE split_part(ev->>'kind',':',1) IN ('failover','retry_exhausted_failover','failover_on_400'))),0) switch_count
  FROM error_base GROUP BY GROUPING SETS ((bucket_start,model),(bucket_start,model,platform),(bucket_start,model,platform,group_id))
  HAVING GROUPING(group_id)=1 OR group_id IS NOT NULL
), status_rows AS (
  SELECT bucket_start,model,
         CASE WHEN GROUPING(platform)=1 THEN NULL ELSE platform END platform,
         CASE WHEN GROUPING(group_id)=1 THEN NULL ELSE group_id END group_id, effective_status_code,
         COUNT(*) total, COUNT(*) FILTER (WHERE NOT is_business_limited) sla,
         COUNT(*) FILTER (WHERE is_business_limited) business_limited
  FROM error_base WHERE COALESCE(status_code,0)>=400
  GROUP BY GROUPING SETS ((bucket_start,model,effective_status_code),(bucket_start,model,platform,effective_status_code),(bucket_start,model,platform,group_id,effective_status_code))
  HAVING GROUPING(group_id)=1 OR group_id IS NOT NULL
), status_agg AS (
  SELECT bucket_start,model,platform,group_id,
         jsonb_object_agg(effective_status_code::text,jsonb_build_object('total',total,'sla',sla,'business_limited',business_limited)) error_status_counts
  FROM status_rows GROUP BY 1,2,3,4
), combined AS (
  SELECT COALESCE(u.bucket_start,e.bucket_start) bucket_start, COALESCE(u.model,e.model) model,
         COALESCE(u.platform,e.platform) platform, COALESCE(u.group_id,e.group_id) group_id,
         u.success_count,u.ttft_sample_count,u.token_consumed,
         u.duration_p50,u.duration_p90,u.duration_p95,u.duration_p99,u.duration_avg,u.duration_max,
         u.ttft_p50,u.ttft_p90,u.ttft_p95,u.ttft_p99,u.ttft_avg,u.ttft_max,
         u.latency_0_100,u.latency_100_200,u.latency_200_500,u.latency_500_1000,u.latency_1000_2000,u.latency_2000_plus,
         e.error_count_total,e.business_limited_count,e.error_count_sla,e.upstream_excl,e.upstream_429,e.upstream_529,e.switch_count
  FROM usage_agg u FULL JOIN error_agg e ON u.bucket_start=e.bucket_start AND u.model=e.model
    AND COALESCE(u.platform,'')=COALESCE(e.platform,'') AND COALESCE(u.group_id,0)=COALESCE(e.group_id,0)
)
INSERT INTO %s (bucket_start,model,platform,group_id,success_count,ttft_sample_count,error_count_total,business_limited_count,error_count_sla,
 upstream_error_count_excl_429_529,upstream_429_count,upstream_529_count,token_consumed,switch_count,
 duration_p50_ms,duration_p90_ms,duration_p95_ms,duration_p99_ms,duration_avg_ms,duration_max_ms,
 ttft_p50_ms,ttft_p90_ms,ttft_p95_ms,ttft_p99_ms,ttft_avg_ms,ttft_max_ms,
 latency_0_100,latency_100_200,latency_200_500,latency_500_1000,latency_1000_2000,latency_2000_plus,error_status_counts,computed_at)
SELECT c.bucket_start,c.model,c.platform,c.group_id,COALESCE(c.success_count,0),COALESCE(c.ttft_sample_count,0),COALESCE(c.error_count_total,0),
 COALESCE(c.business_limited_count,0),COALESCE(c.error_count_sla,0),COALESCE(c.upstream_excl,0),COALESCE(c.upstream_429,0),COALESCE(c.upstream_529,0),
 COALESCE(c.token_consumed,0),COALESCE(c.switch_count,0),c.duration_p50::int,c.duration_p90::int,c.duration_p95::int,c.duration_p99::int,c.duration_avg,c.duration_max::int,
 c.ttft_p50::int,c.ttft_p90::int,c.ttft_p95::int,c.ttft_p99::int,c.ttft_avg,c.ttft_max::int,
 COALESCE(c.latency_0_100,0),COALESCE(c.latency_100_200,0),COALESCE(c.latency_200_500,0),COALESCE(c.latency_500_1000,0),COALESCE(c.latency_1000_2000,0),COALESCE(c.latency_2000_plus,0),
 COALESCE(s.error_status_counts,'{}'::jsonb),NOW()
FROM combined c LEFT JOIN status_agg s ON c.bucket_start=s.bucket_start AND c.model=s.model
 AND COALESCE(c.platform,'')=COALESCE(s.platform,'') AND COALESCE(c.group_id,0)=COALESCE(s.group_id,0)`

func (r *opsRepository) GetLatestModelMetricsBucketStart(ctx context.Context, resolutionSeconds int) (time.Time, bool, error) {
	if r == nil || r.db == nil {
		return time.Time{}, false, fmt.Errorf("nil ops repository")
	}
	var value sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT MAX(bucket_start) FROM ops_model_metrics_coverage WHERE resolution_seconds=$1`, resolutionSeconds).Scan(&value)
	if err != nil || !value.Valid {
		return time.Time{}, false, err
	}
	return value.Time.UTC(), true, nil
}

func (r *opsRepository) GetOldestModelMetricsBucketStart(ctx context.Context, resolutionSeconds int) (time.Time, bool, error) {
	if r == nil || r.db == nil {
		return time.Time{}, false, fmt.Errorf("nil ops repository")
	}
	var value sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT MIN(bucket_start) FROM ops_model_metrics_coverage WHERE resolution_seconds=$1`, resolutionSeconds).Scan(&value)
	if err != nil || !value.Valid {
		return time.Time{}, false, err
	}
	return value.Time.UTC(), true, nil
}

func (r *opsRepository) GetMissingModelMetricsBucketStart(ctx context.Context, resolutionSeconds int, start, end time.Time) (time.Time, bool, error) {
	if r == nil || r.db == nil {
		return time.Time{}, false, fmt.Errorf("nil ops repository")
	}
	var value sql.NullTime
	q := `SELECT MAX(s.bucket) FROM generate_series($2::timestamptz, $3::timestamptz - ($1 * interval '1 second'), $1 * interval '1 second') s(bucket) LEFT JOIN ops_model_metrics_coverage c ON c.resolution_seconds=$1 AND c.bucket_start=s.bucket WHERE c.bucket_start IS NULL`
	err := r.db.QueryRowContext(ctx, q, resolutionSeconds, start.UTC(), end.UTC()).Scan(&value)
	if err != nil || !value.Valid {
		return time.Time{}, false, err
	}
	return value.Time.UTC(), true, nil
}
