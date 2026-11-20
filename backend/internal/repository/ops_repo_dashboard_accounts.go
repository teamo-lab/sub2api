package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// opsAccountBreakdownLatencyTimeout bounds the per-account percentile query. On
// timeout the counts are still returned and the latency columns stay empty.
const opsAccountBreakdownLatencyTimeout = 5 * time.Second

// GetDashboardAccountBreakdown splits the dashboard window by upstream account.
// Success/error/token counts reuse the overview's usage and error WHERE builders,
// so the rows of a group sum to the group's overview card.
func (r *opsRepository) GetDashboardAccountBreakdown(ctx context.Context, filter *service.OpsDashboardFilter) (*service.OpsDashboardAccountBreakdownResponse, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil || filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, fmt.Errorf("start_time/end_time required")
	}
	start, end := filter.StartTime.UTC(), filter.EndTime.UTC()

	items, truncated, err := r.queryAccountBreakdownCounts(ctx, filter, start, end)
	if err != nil {
		return nil, err
	}

	out := &service.OpsDashboardAccountBreakdownResponse{
		StartTime: start,
		EndTime:   end,
		Truncated: truncated,
		Items:     items,
	}
	if len(items) == 0 {
		return out, nil
	}

	latencyCtx, cancel := context.WithTimeout(ctx, opsAccountBreakdownLatencyTimeout)
	defer cancel()
	latency, err := r.queryAccountBreakdownLatency(latencyCtx, filter, start, end)
	if err != nil {
		if isQueryTimeoutErr(err) {
			out.LatencyDegraded = true
			return out, nil
		}
		return nil, err
	}
	for _, item := range items {
		if l, ok := latency[item.AccountID]; ok {
			item.TTFTP50MS = l.ttftP50
			item.TTFTP90MS = l.ttftP90
			item.DurationP50MS = l.durationP50
			item.DurationP90MS = l.durationP90
		}
	}
	return out, nil
}

func (r *opsRepository) queryAccountBreakdownCounts(ctx context.Context, filter *service.OpsDashboardFilter, start, end time.Time) ([]*service.OpsDashboardAccountBreakdownItem, bool, error) {
	usageJoin, usageWhere, usageArgs, next := buildUsageWhere(filter, start, end, 1)
	errorWhere, errorArgs, _ := buildErrorWhere(filter, start, end, next)

	// Error rows without an account (failed before scheduling) are grouped under 0.
	q := `
WITH usage_agg AS (
  SELECT
    ul.account_id AS account_id,
    COUNT(*) AS success_count,
    COALESCE(SUM(ul.input_tokens), 0) AS input_tokens,
    COALESCE(SUM(ul.output_tokens), 0) AS output_tokens,
    COALESCE(SUM(ul.cache_creation_tokens), 0) AS cache_creation_tokens,
    COALESCE(SUM(ul.cache_read_tokens), 0) AS cache_read_tokens
  FROM usage_logs ul
  ` + usageJoin + `
  ` + usageWhere + `
  GROUP BY ul.account_id
),
error_agg AS (
  SELECT
    COALESCE(account_id, 0) AS account_id,
    COUNT(*) FILTER (WHERE COALESCE(status_code, 0) >= 400) AS error_total,
    COUNT(*) FILTER (WHERE COALESCE(status_code, 0) >= 400 AND NOT is_business_limited) AS error_sla
  FROM ops_error_logs
  ` + errorWhere + `
  GROUP BY 1
)
SELECT
  COALESCE(u.account_id, e.account_id) AS account_id,
  COALESCE(a.name, '') AS account_name,
  COALESCE(a.platform, '') AS platform,
  COALESCE(a.status, '') AS status,
  a.rate_multiplier::float8 AS rate_multiplier,
  a.sell_rate_multiplier::float8 AS sell_rate_multiplier,
  COALESCE(u.success_count, 0) AS success_count,
  COALESCE(e.error_total, 0) AS error_total,
  COALESCE(e.error_sla, 0) AS error_sla,
  COALESCE(u.input_tokens, 0) AS input_tokens,
  COALESCE(u.output_tokens, 0) AS output_tokens,
  COALESCE(u.cache_creation_tokens, 0) AS cache_creation_tokens,
  COALESCE(u.cache_read_tokens, 0) AS cache_read_tokens
FROM usage_agg u
FULL OUTER JOIN error_agg e ON e.account_id = u.account_id
LEFT JOIN accounts a ON a.id = COALESCE(u.account_id, e.account_id)
WHERE COALESCE(u.success_count, 0) + COALESCE(e.error_total, 0) > 0
ORDER BY COALESCE(u.success_count, 0) + COALESCE(e.error_total, 0) DESC, 1 ASC
LIMIT ` + fmt.Sprint(service.OpsAccountBreakdownLimit+1)

	rows, err := r.db.QueryContext(ctx, q, append(usageArgs, errorArgs...)...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]*service.OpsDashboardAccountBreakdownItem, 0, 32)
	for rows.Next() {
		item := &service.OpsDashboardAccountBreakdownItem{}
		var costRate, sellRate sql.NullFloat64
		if err := rows.Scan(
			&item.AccountID, &item.AccountName, &item.Platform, &item.Status, &costRate, &sellRate,
			&item.SuccessCount, &item.ErrorCountTotal, &item.ErrorCountSLA,
			&item.InputTokens, &item.OutputTokens, &item.CacheCreationTokens, &item.CacheReadTokens,
		); err != nil {
			return nil, false, err
		}
		item.RateMultiplier, item.SellRateMultiplier = accountBreakdownRates(costRate, sellRate)
		item.RequestCountTotal = item.SuccessCount + item.ErrorCountTotal
		item.RequestCountSLA = item.SuccessCount + item.ErrorCountSLA
		item.TokenConsumed = item.InputTokens + item.OutputTokens + item.CacheCreationTokens + item.CacheReadTokens
		if item.RequestCountSLA > 0 {
			sla := roundTo4DP(float64(item.SuccessCount) / float64(item.RequestCountSLA))
			item.SLA = &sla
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	truncated := len(items) > service.OpsAccountBreakdownLimit
	if truncated {
		items = items[:service.OpsAccountBreakdownLimit]
	}
	return items, truncated, nil
}

// accountBreakdownRates normalizes the account's current rates with the same rules
// billing uses (Account.BillingRateMultiplier / SellRateOverride), so the table shows
// what the account is actually charged and sold at. Both stay nil when the row has no
// account (unassigned, or the account was deleted); the sell rate also stays nil when
// it is unset, meaning the group price applies.
func accountBreakdownRates(costRate, sellRate sql.NullFloat64) (*float64, *float64) {
	if !costRate.Valid {
		return nil, nil
	}
	account := &service.Account{RateMultiplier: &costRate.Float64}
	if sellRate.Valid {
		account.SellRateMultiplier = &sellRate.Float64
	}
	cost := account.BillingRateMultiplier()
	sell, ok := account.SellRateOverride()
	if !ok {
		return &cost, nil
	}
	return &cost, &sell
}

type opsAccountLatency struct {
	ttftP50, ttftP90, durationP50, durationP90 *int
}

func (r *opsRepository) queryAccountBreakdownLatency(ctx context.Context, filter *service.OpsDashboardFilter, start, end time.Time) (map[int64]opsAccountLatency, error) {
	join, where, args, _ := buildUsageWhere(filter, start, end, 1)
	q := `
SELECT
  ul.account_id,
  percentile_cont(0.50) WITHIN GROUP (ORDER BY ul.first_token_ms) FILTER (WHERE ul.first_token_ms IS NOT NULL) AS ttft_p50,
  percentile_cont(0.90) WITHIN GROUP (ORDER BY ul.first_token_ms) FILTER (WHERE ul.first_token_ms IS NOT NULL) AS ttft_p90,
  percentile_cont(0.50) WITHIN GROUP (ORDER BY ul.duration_ms) FILTER (WHERE ul.duration_ms IS NOT NULL) AS duration_p50,
  percentile_cont(0.90) WITHIN GROUP (ORDER BY ul.duration_ms) FILTER (WHERE ul.duration_ms IS NOT NULL) AS duration_p90
FROM usage_logs ul
` + join + `
` + where + `
GROUP BY ul.account_id`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]opsAccountLatency)
	for rows.Next() {
		var accountID int64
		var tP50, tP90, dP50, dP90 sql.NullFloat64
		if err := rows.Scan(&accountID, &tP50, &tP90, &dP50, &dP90); err != nil {
			return nil, err
		}
		out[accountID] = opsAccountLatency{
			ttftP50:     floatToIntPtr(tP50),
			ttftP90:     floatToIntPtr(tP90),
			durationP50: floatToIntPtr(dP50),
			durationP90: floatToIntPtr(dP90),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
