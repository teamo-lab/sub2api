package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type modelRollupSpec struct {
	table   string
	seconds int
}

func modelRollupFor(f *service.OpsDashboardFilter) modelRollupSpec {
	if f.EndTime.Sub(f.StartTime) <= 24*time.Hour {
		return modelRollupSpec{"ops_model_metrics_5m", 300}
	}
	return modelRollupSpec{"ops_model_metrics_hourly", 3600}
}

// modelRollupWindow deliberately requires exact bucket boundaries. Serving an
// overlapping rollup bucket would include requests outside the requested range;
// the safe behavior for those windows is the existing raw query.
func modelRollupWindow(ctx context.Context, r *opsRepository, f *service.OpsDashboardFilter) (modelRollupSpec, bool, error) {
	s := modelRollupFor(f)
	start, end := f.StartTime.UTC(), f.EndTime.UTC()
	step := time.Duration(s.seconds) * time.Second
	if !start.Before(end) || !start.Equal(start.Truncate(step)) || !end.Equal(end.Truncate(step)) {
		return s, false, nil
	}
	expected := int64(end.Sub(start) / step)
	var covered int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_model_metrics_coverage WHERE resolution_seconds=$1 AND bucket_start >= $2 AND bucket_start < $3`, s.seconds, start, end).Scan(&covered)
	if err != nil {
		return s, false, err
	}
	return s, covered == expected, nil
}

func modelRollupWhere(f *service.OpsDashboardFilter, startIndex int) (string, []any) {
	clauses := []string{fmt.Sprintf("model = $%d", startIndex)}
	args := []any{strings.TrimSpace(f.Model)}
	i := startIndex + 1
	if f.GroupID != nil && *f.GroupID > 0 {
		clauses = append(clauses, fmt.Sprintf("group_id = $%d", i))
		args = append(args, *f.GroupID)
		i++
		if p := strings.TrimSpace(strings.ToLower(f.Platform)); p != "" {
			clauses = append(clauses, fmt.Sprintf("platform = $%d", i))
			args = append(args, p)
		}
	} else if p := strings.TrimSpace(strings.ToLower(f.Platform)); p != "" {
		clauses = append(clauses, fmt.Sprintf("platform = $%d", i), "group_id IS NULL")
		args = append(args, p)
	} else {
		clauses = append(clauses, "platform IS NULL", "group_id IS NULL")
	}
	return strings.Join(clauses, " AND "), args
}

func unavailableModelRollup(f *service.OpsDashboardFilter) (bool, error) {
	if f.QueryMode == service.OpsQueryModePreagg {
		return true, service.ErrOpsPreaggregatedNotPopulated
	}
	return false, nil
}

func (r *opsRepository) getModelDashboardOverviewRollup(ctx context.Context, f *service.OpsDashboardFilter) (*service.OpsDashboardOverview, bool, error) {
	s, covered, err := modelRollupWindow(ctx, r, f)
	if err != nil {
		return nil, true, err
	}
	if !covered {
		ok, e := unavailableModelRollup(f)
		return nil, ok, e
	}
	w, args := modelRollupWhere(f, 3)
	args = append([]any{f.StartTime.UTC(), f.EndTime.UTC()}, args...)
	q := fmt.Sprintf(`SELECT COALESCE(SUM(success_count),0),COALESCE(SUM(error_count_total),0),COALESCE(SUM(business_limited_count),0),COALESCE(SUM(error_count_sla),0),COALESCE(SUM(upstream_error_count_excl_429_529),0),COALESCE(SUM(upstream_429_count),0),COALESCE(SUM(upstream_529_count),0),COALESCE(SUM(token_consumed),0),ROUND(SUM(duration_p50_ms::float8*success_count)/NULLIF(SUM(success_count),0)),ROUND(SUM(duration_p90_ms::float8*success_count)/NULLIF(SUM(success_count),0)),MAX(duration_p95_ms),MAX(duration_p99_ms),ROUND(SUM(duration_avg_ms*success_count)/NULLIF(SUM(success_count),0)),MAX(duration_max_ms),ROUND(SUM(ttft_p50_ms::float8*ttft_sample_count)/NULLIF(SUM(ttft_sample_count),0)),ROUND(SUM(ttft_p90_ms::float8*ttft_sample_count)/NULLIF(SUM(ttft_sample_count),0)),MAX(ttft_p95_ms),MAX(ttft_p99_ms),ROUND(SUM(ttft_avg_ms*ttft_sample_count)/NULLIF(SUM(ttft_sample_count),0)),MAX(ttft_max_ms) FROM %s WHERE bucket_start >= $1 AND bucket_start < $2 AND %s`, s.table, w)
	var success, total, biz, slaErr, up, up429, up529, tokens int64
	var vals [12]sql.NullFloat64
	err = r.db.QueryRowContext(ctx, q, args...).Scan(&success, &total, &biz, &slaErr, &up, &up429, &up529, &tokens, &vals[0], &vals[1], &vals[2], &vals[3], &vals[4], &vals[5], &vals[6], &vals[7], &vals[8], &vals[9], &vals[10], &vals[11])
	if err != nil {
		return nil, true, err
	}
	p := func(v sql.NullFloat64) *int {
		if !v.Valid {
			return nil
		}
		n := int(v.Float64)
		return &n
	}
	dur := service.OpsPercentiles{P50: p(vals[0]), P90: p(vals[1]), P95: p(vals[2]), P99: p(vals[3]), Avg: p(vals[4]), Max: p(vals[5])}
	ttft := service.OpsPercentiles{P50: p(vals[6]), P90: p(vals[7]), P95: p(vals[8]), P99: p(vals[9]), Avg: p(vals[10]), Max: p(vals[11])}
	req := success + total
	reqSLA := success + slaErr
	secs := f.EndTime.Sub(f.StartTime).Seconds()
	currentQ, currentT, e := r.queryCurrentRates(ctx, f, f.EndTime.UTC())
	if e != nil {
		return nil, true, e
	}
	peakQ, peakT, e := r.queryPeakRates(ctx, f, f.StartTime.UTC(), f.EndTime.UTC())
	if e != nil {
		return nil, true, e
	}
	out := &service.OpsDashboardOverview{StartTime: f.StartTime.UTC(), EndTime: f.EndTime.UTC(), Platform: strings.TrimSpace(f.Platform), GroupID: f.GroupID, SuccessCount: success, ErrorCountTotal: total, BusinessLimitedCount: biz, ErrorCountSLA: slaErr, RequestCountTotal: req, RequestCountSLA: reqSLA, TokenConsumed: tokens, SLA: roundTo4DP(safeDivideFloat64(float64(success), float64(reqSLA))), ErrorRate: roundTo4DP(safeDivideFloat64(float64(slaErr), float64(reqSLA))), UpstreamErrorRate: roundTo4DP(safeDivideFloat64(float64(up), float64(reqSLA))), UpstreamErrorCountExcl429529: up, Upstream429Count: up429, Upstream529Count: up529, QPS: service.OpsRateSummary{Current: currentQ, Peak: peakQ, Avg: roundTo1DP(float64(req) / secs)}, TPS: service.OpsRateSummary{Current: currentT, Peak: peakT, Avg: roundTo1DP(float64(tokens) / secs)}, Duration: dur, TTFT: ttft}
	return out, true, nil
}

func (r *opsRepository) getModelThroughputTrendRollup(ctx context.Context, f *service.OpsDashboardFilter) (*service.OpsThroughputTrendResponse, bool, error) {
	s, c, e := modelRollupWindow(ctx, r, f)
	if e != nil {
		return nil, true, e
	}
	if !c {
		ok, e := unavailableModelRollup(f)
		return nil, ok, e
	}
	w, args := modelRollupWhere(f, 3)
	args = append([]any{f.StartTime.UTC(), f.EndTime.UTC()}, args...)
	q := fmt.Sprintf(`SELECT bucket_start,success_count+error_count_total,token_consumed,switch_count FROM %s WHERE bucket_start >= $1 AND bucket_start < $2 AND %s ORDER BY bucket_start`, s.table, w)
	rows, e := r.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, true, e
	}
	defer rows.Close()
	pts := []*service.OpsThroughputTrendPoint{}
	for rows.Next() {
		p := &service.OpsThroughputTrendPoint{}
		if e = rows.Scan(&p.BucketStart, &p.RequestCount, &p.TokenConsumed, &p.SwitchCount); e != nil {
			return nil, true, e
		}
		p.QPS = roundTo1DP(float64(p.RequestCount) / float64(s.seconds))
		p.TPS = roundTo1DP(float64(p.TokenConsumed) / float64(s.seconds))
		pts = append(pts, p)
	}
	return &service.OpsThroughputTrendResponse{Bucket: opsBucketLabel(s.seconds), Points: pts}, true, rows.Err()
}

func (r *opsRepository) getModelErrorTrendRollup(ctx context.Context, f *service.OpsDashboardFilter) (*service.OpsErrorTrendResponse, bool, error) {
	s, c, e := modelRollupWindow(ctx, r, f)
	if e != nil {
		return nil, true, e
	}
	if !c {
		ok, e := unavailableModelRollup(f)
		return nil, ok, e
	}
	w, args := modelRollupWhere(f, 3)
	args = append([]any{f.StartTime.UTC(), f.EndTime.UTC()}, args...)
	q := fmt.Sprintf(`SELECT bucket_start,error_count_total,business_limited_count,error_count_sla,upstream_error_count_excl_429_529,upstream_429_count,upstream_529_count FROM %s WHERE bucket_start >= $1 AND bucket_start < $2 AND %s ORDER BY bucket_start`, s.table, w)
	rows, e := r.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, true, e
	}
	defer rows.Close()
	pts := []*service.OpsErrorTrendPoint{}
	for rows.Next() {
		p := &service.OpsErrorTrendPoint{}
		if e = rows.Scan(&p.BucketStart, &p.ErrorCountTotal, &p.BusinessLimitedCount, &p.ErrorCountSLA, &p.UpstreamErrorCountExcl429529, &p.Upstream429Count, &p.Upstream529Count); e != nil {
			return nil, true, e
		}
		pts = append(pts, p)
	}
	return &service.OpsErrorTrendResponse{Bucket: opsBucketLabel(s.seconds), Points: pts}, true, rows.Err()
}

func (r *opsRepository) getModelLatencyHistogramRollup(ctx context.Context, f *service.OpsDashboardFilter) (*service.OpsLatencyHistogramResponse, bool, error) {
	s, c, e := modelRollupWindow(ctx, r, f)
	if e != nil {
		return nil, true, e
	}
	if !c {
		ok, e := unavailableModelRollup(f)
		return nil, ok, e
	}
	w, args := modelRollupWhere(f, 3)
	args = append([]any{f.StartTime.UTC(), f.EndTime.UTC()}, args...)
	q := fmt.Sprintf(`SELECT COALESCE(SUM(latency_0_100),0),COALESCE(SUM(latency_100_200),0),COALESCE(SUM(latency_200_500),0),COALESCE(SUM(latency_500_1000),0),COALESCE(SUM(latency_1000_2000),0),COALESCE(SUM(latency_2000_plus),0) FROM %s WHERE bucket_start >= $1 AND bucket_start < $2 AND %s`, s.table, w)
	var n [6]int64
	if e = r.db.QueryRowContext(ctx, q, args...).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5]); e != nil {
		return nil, true, e
	}
	labels := latencyHistogramOrderedRanges
	b := make([]*service.OpsLatencyHistogramBucket, 6)
	var total int64
	for i := range b {
		b[i] = &service.OpsLatencyHistogramBucket{Range: labels[i], Count: n[i]}
		total += n[i]
	}
	return &service.OpsLatencyHistogramResponse{StartTime: f.StartTime.UTC(), EndTime: f.EndTime.UTC(), Platform: strings.TrimSpace(f.Platform), GroupID: f.GroupID, TotalRequests: total, Buckets: b}, true, nil
}

type statusRollup struct {
	Total           int64 `json:"total"`
	SLA             int64 `json:"sla"`
	BusinessLimited int64 `json:"business_limited"`
}

func (r *opsRepository) getModelErrorDistributionRollup(ctx context.Context, f *service.OpsDashboardFilter) (*service.OpsErrorDistributionResponse, bool, error) {
	s, c, e := modelRollupWindow(ctx, r, f)
	if e != nil {
		return nil, true, e
	}
	if !c {
		ok, e := unavailableModelRollup(f)
		return nil, ok, e
	}
	w, args := modelRollupWhere(f, 3)
	args = append([]any{f.StartTime.UTC(), f.EndTime.UTC()}, args...)
	q := fmt.Sprintf(`SELECT error_status_counts FROM %s WHERE bucket_start >= $1 AND bucket_start < $2 AND %s`, s.table, w)
	rows, e := r.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, true, e
	}
	defer rows.Close()
	all := map[int]statusRollup{}
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			return nil, true, e
		}
		var m map[int]statusRollup
		if e = json.Unmarshal(raw, &m); e != nil {
			return nil, true, e
		}
		for code, v := range m {
			x := all[code]
			x.Total += v.Total
			x.SLA += v.SLA
			x.BusinessLimited += v.BusinessLimited
			all[code] = x
		}
	}
	codes := make([]int, 0, len(all))
	for c := range all {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool { return all[codes[i]].Total > all[codes[j]].Total })
	out := &service.OpsErrorDistributionResponse{}
	for _, c := range codes {
		v := all[c]
		out.Total += v.Total
		out.Items = append(out.Items, &service.OpsErrorDistributionItem{StatusCode: c, Total: v.Total, SLA: v.SLA, BusinessLimited: v.BusinessLimited})
	}
	return out, true, rows.Err()
}
