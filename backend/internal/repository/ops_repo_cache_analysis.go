package repository

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.OpsCacheAnalysisStore = (*opsRepository)(nil)

const opsCacheAnalysisUpsertChunk = 500

// UpsertCacheAnalysisBuckets 把内存聚合累加写进 ops_cache_analysis_5m。
func (r *opsRepository) UpsertCacheAnalysisBuckets(ctx context.Context, rows []service.OpsCacheAnalysisBucket) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil ops repository")
	}
	for start := 0; start < len(rows); start += opsCacheAnalysisUpsertChunk {
		end := start + opsCacheAnalysisUpsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := r.upsertCacheAnalysisChunk(ctx, rows[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (r *opsRepository) upsertCacheAnalysisChunk(ctx context.Context, rows []service.OpsCacheAnalysisBucket) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 10
	var sb strings.Builder
	sb.WriteString(`INSERT INTO ops_cache_analysis_5m
  (bucket_start, user_id, group_id, account_id, model, protocol, cause, request_count, tokens, saved_usd)
VALUES `)
	args := make([]any, 0, len(rows)*cols)
	for i, row := range rows {
		if i > 0 {
			sb.WriteString(",")
		}
		base := i * cols
		sb.WriteString("(")
		for j := 1; j <= cols; j++ {
			if j > 1 {
				sb.WriteString(",")
			}
			sb.WriteString("$" + itoa(base+j))
		}
		sb.WriteString(")")
		model := row.Model
		if len(model) > 255 {
			model = model[:255]
		}
		args = append(args, row.BucketStart.UTC(), row.UserID, row.GroupID, row.AccountID, model, row.Protocol, string(row.Cause), row.RequestCount, row.Tokens, row.SavedUSD)
	}
	sb.WriteString(`
ON CONFLICT (bucket_start, user_id, group_id, account_id, model, protocol, cause) DO UPDATE SET
  request_count = ops_cache_analysis_5m.request_count + EXCLUDED.request_count,
  tokens = ops_cache_analysis_5m.tokens + EXCLUDED.tokens,
  saved_usd = ops_cache_analysis_5m.saved_usd + EXCLUDED.saved_usd,
  updated_at = NOW()`)
	_, err := r.db.ExecContext(ctx, sb.String(), args...)
	return err
}

func buildOpsCacheAnalysisWhere(filter *service.OpsDashboardFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := make([]any, 0, 6)
	if filter == nil {
		return "WHERE 1=1", args
	}
	if !filter.StartTime.IsZero() {
		// 起点落在桶中间时把整个桶算进来（桶粒度 5 分钟）。
		start := filter.StartTime.UTC().Truncate(5 * time.Minute)
		args = append(args, start)
		clauses = append(clauses, "c.bucket_start >= $"+itoa(len(args)))
	}
	if !filter.EndTime.IsZero() {
		args = append(args, filter.EndTime.UTC())
		clauses = append(clauses, "c.bucket_start < $"+itoa(len(args)))
	}
	if filter.UserID != nil && *filter.UserID > 0 {
		args = append(args, *filter.UserID)
		clauses = append(clauses, "c.user_id = $"+itoa(len(args)))
	}
	if filter.GroupID != nil && *filter.GroupID > 0 {
		args = append(args, *filter.GroupID)
		clauses = append(clauses, "c.group_id = $"+itoa(len(args)))
	}
	if filter.AccountID != nil && *filter.AccountID > 0 {
		args = append(args, *filter.AccountID)
		clauses = append(clauses, "c.account_id = $"+itoa(len(args)))
	}
	if m := strings.TrimSpace(filter.Model); m != "" {
		args = append(args, m)
		clauses = append(clauses, "c.model = $"+itoa(len(args)))
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

const opsCacheAnalysisTopAccounts = 10

// QueryCacheAnalysis 汇总窗口内的归因，并给出未命中最多的账号。
func (r *opsRepository) QueryCacheAnalysis(ctx context.Context, filter *service.OpsDashboardFilter) (*service.OpsCacheAnalysisResponse, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	where, args := buildOpsCacheAnalysisWhere(filter)
	rows, err := r.db.QueryContext(ctx, `
SELECT c.account_id, c.cause, COALESCE(SUM(c.request_count), 0), COALESCE(SUM(c.tokens), 0), COALESCE(SUM(c.saved_usd), 0)
FROM ops_cache_analysis_5m c
`+where+`
GROUP BY c.account_id, c.cause`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := &service.OpsCacheAnalysisResponse{}
	if filter != nil {
		out.StartTime = filter.StartTime
		out.EndTime = filter.EndTime
	}
	totals := map[service.CacheCause]*service.OpsCacheAnalysisCauseStat{}
	accounts := map[int64]*service.OpsCacheAnalysisAccountStat{}
	actionable := map[int64]int64{}
	for rows.Next() {
		var accountID, requests, tokens int64
		var cause string
		var saved float64
		if err := rows.Scan(&accountID, &cause, &requests, &tokens, &saved); err != nil {
			return nil, err
		}
		c := service.CacheCause(cause)
		out.SavedUSD += saved
		t := totals[c]
		if t == nil {
			t = &service.OpsCacheAnalysisCauseStat{Cause: c}
			totals[c] = t
		}
		t.Requests += requests
		t.Tokens += tokens
		if accountID <= 0 {
			continue
		}
		acc := accounts[accountID]
		if acc == nil {
			acc = &service.OpsCacheAnalysisAccountStat{AccountID: accountID}
			accounts[accountID] = acc
		}
		acc.PromptTokens += tokens
		if c == service.CacheCauseHit {
			acc.HitTokens += tokens
		}
		acc.Causes = append(acc.Causes, service.OpsCacheAnalysisCauseStat{Cause: c, Requests: requests, Tokens: tokens})
		if service.IsActionableCacheMiss(c) {
			actionable[accountID] += tokens
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range totals {
		out.Causes = append(out.Causes, *t)
	}

	ranked := make([]*service.OpsCacheAnalysisAccountStat, 0, len(accounts))
	for _, acc := range accounts {
		ranked = append(ranked, acc)
	}
	sort.Slice(ranked, func(i, j int) bool {
		ai, aj := actionable[ranked[i].AccountID], actionable[ranked[j].AccountID]
		if ai != aj {
			return ai > aj
		}
		ui, uj := ranked[i].PromptTokens-ranked[i].HitTokens, ranked[j].PromptTokens-ranked[j].HitTokens
		if ui != uj {
			return ui > uj
		}
		return ranked[i].AccountID < ranked[j].AccountID
	})
	if len(ranked) > opsCacheAnalysisTopAccounts {
		ranked = ranked[:opsCacheAnalysisTopAccounts]
	}
	if len(ranked) > 0 {
		ids := make([]int64, 0, len(ranked))
		for _, acc := range ranked {
			ids = append(ids, acc.AccountID)
		}
		names := map[int64]string{}
		nameRows, err := r.db.QueryContext(ctx, `SELECT id, name FROM accounts WHERE id = ANY($1)`, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		for nameRows.Next() {
			var id int64
			var name string
			if err := nameRows.Scan(&id, &name); err != nil {
				_ = nameRows.Close()
				return nil, err
			}
			names[id] = name
		}
		_ = nameRows.Close()
		for _, acc := range ranked {
			acc.AccountName = names[acc.AccountID]
			sort.Slice(acc.Causes, func(i, j int) bool { return acc.Causes[i].Tokens > acc.Causes[j].Tokens })
			out.Accounts = append(out.Accounts, *acc)
		}
	}
	service.FinalizeCacheAnalysis(out)
	return out, nil
}
