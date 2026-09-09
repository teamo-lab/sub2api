package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"strings"
	"time"
)

func requestProfileWhere(f service.RequestProfileFilter) (string, []any) {
	clauses := []string{"l.extra ? 'request_profile'", "jsonb_typeof(l.extra->'request_profile') = 'object'", "l.extra->'request_profile'->>'version' = '1'", "l.component = 'http.access'", "l.message = 'http request completed'", "l.created_at >= $1", "l.created_at < $2"}
	args := []any{f.From, f.To}
	add := func(expr string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if f.Evidence != "" {
		add("COALESCE(l.extra->'request_profile'->>'evidence','measured')=$%d", f.Evidence)
	}
	if f.Model != "" {
		add("l.extra->'request_profile'->>'model' = $%d", f.Model)
	}
	if f.Protocol != "" {
		add("l.extra->'request_profile'->>'protocol' = $%d", f.Protocol)
	}
	if f.GroupID > 0 {
		add("l.extra->'request_profile'->>'group_id' = $%d", fmt.Sprint(f.GroupID))
	}
	if f.AccountID > 0 {
		add("(l.account_id::text=$%[1]d OR EXISTS (SELECT 1 FROM jsonb_array_elements(l.extra->'request_profile'->'events') e WHERE e->>'account_id'=$%[1]d))", fmt.Sprint(f.AccountID))
	}
	if len(f.Models) > 0 {
		add("l.extra->'request_profile'->>'model' = ANY($%d::text[])", pq.Array(f.Models))
	}
	if len(f.GroupIDs) > 0 {
		add("l.extra->'request_profile'->>'group_id' = ANY($%d::text[])", pq.Array(f.GroupIDs))
	}
	if len(f.AccountIDs) > 0 {
		add("(l.account_id::text = ANY($%[1]d::text[]) OR EXISTS (SELECT 1 FROM jsonb_array_elements(l.extra->'request_profile'->'events') e WHERE e->>'account_id' = ANY($%[1]d::text[])))", pq.Array(f.AccountIDs))
	}
	if f.ID > 0 {
		add("l.id=$%d", f.ID)
	}
	if f.RequestID != "" {
		args = append(args, f.RequestID)
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(l.request_id=$%d OR l.client_request_id=$%d)", n, n))
	}
	switch f.ErrorType {
	case "http_4xx":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM jsonb_array_elements(l.extra->'request_profile'->'events') e WHERE (e->>'status')::int BETWEEN 400 AND 499)")
	case "http_5xx":
		clauses = append(clauses, "EXISTS (SELECT 1 FROM jsonb_array_elements(l.extra->'request_profile'->'events') e WHERE (e->>'status')::int BETWEEN 500 AND 599)")
	case "retry", "fallback", "network_error", "timeout", "upstream_cancelled", "client_cancelled", "client_disconnected", "downstream_write_error", "local_reselect":
		add("EXISTS (SELECT 1 FROM jsonb_array_elements(l.extra->'request_profile'->'events') e WHERE e->>'kind'=$%d)", f.ErrorType)
	case "failed":
		clauses = append(clauses, "(l.extra->>'status_code')::int >= 400")
	}
	return strings.Join(clauses, " AND "), args
}
func (r *opsRepository) QueryRequestProfiles(ctx context.Context, f service.RequestProfileFilter) (*service.RequestProfileResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	where, args := requestProfileWhere(f)
	base := "WITH selected AS (SELECT l.*,l.extra->'request_profile' AS p FROM ops_system_logs l WHERE " + where + ") "
	result := &service.RequestProfileResult{Rows: []service.RequestProfileRow{}, Page: f.Page, Limit: f.Limit}
	result.Summary.Stages = []service.RequestProfileStage{}
	err = tx.QueryRowContext(ctx, base+`SELECT count(*),COALESCE(avg((p->>'total_us')::bigint),0),COALESCE(percentile_cont(0.9) WITHIN GROUP(ORDER BY (p->>'total_us')::bigint),0),count(*) FILTER(WHERE (p->>'dropped')::int>0),COALESCE(sum((SELECT count(*) FROM jsonb_array_elements(p->'events') e WHERE e->>'kind'='retry')),0),COALESCE(sum((SELECT count(*) FROM jsonb_array_elements(p->'events') e WHERE e->>'kind'='fallback')),0),COALESCE(sum((SELECT count(*) FROM jsonb_array_elements(p->'events') e WHERE e->>'kind'='local_reselect')),0) FROM selected`, args...).Scan(&result.Summary.Count, &result.Summary.MeanUS, &result.Summary.P90US, &result.Summary.Truncated, &result.Summary.Retries, &result.Summary.Fallbacks, &result.Summary.LocalReselect)
	if err != nil {
		return nil, err
	}
	stages, err := tx.QueryContext(ctx, base+`SELECT s->>'name',sum((s->>'duration_us')::bigint)::float8/GREATEST((SELECT count(*) FROM selected),1) FROM selected CROSS JOIN LATERAL jsonb_array_elements(p->'segments') s GROUP BY s->>'name' ORDER BY 2 DESC`, args...)
	if err != nil {
		return nil, err
	}
	for stages.Next() {
		var s service.RequestProfileStage
		if err = stages.Scan(&s.Name, &s.MeanUS); err != nil {
			stages.Close()
			return nil, err
		}
		result.Summary.Stages = append(result.Summary.Stages, s)
	}
	err = stages.Err()
	stages.Close()
	if err != nil {
		return nil, err
	}
	listArgs := append(append([]any{}, args...), f.Limit, (f.Page-1)*f.Limit)
	rows, err := tx.QueryContext(ctx, base+fmt.Sprintf(`SELECT l.id,l.created_at,COALESCE(l.request_id,''),COALESCE(l.client_request_id,''),COALESCE(l.account_id,0),COALESCE(a.name,''),COALESCE(g.name,''),COALESCE((l.extra->>'status_code')::int,0),l.p FROM selected l LEFT JOIN accounts a ON a.id=l.account_id LEFT JOIN groups g ON g.id::text=l.p->>'group_id' ORDER BY (l.p->>'total_us')::bigint DESC,l.id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), listArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row service.RequestProfileRow
		var raw []byte
		if err = rows.Scan(&row.ID, &row.CreatedAt, &row.RequestID, &row.ClientRequestID, &row.AccountID, &row.AccountName, &row.GroupName, &row.Status, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(raw, &row.Profile); err != nil {
			rows.Close()
			return nil, err
		}
		result.Rows = append(result.Rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if f.IncludeOptions {
		// Options cover the whole time/protocol/evidence window, not the selected page
		// or dimensions, so choosing one value never hides the remaining choices.
		optionFilter := service.RequestProfileFilter{From: f.From, To: f.To, Protocol: f.Protocol, Evidence: f.Evidence}
		optionWhere, optionArgs := requestProfileWhere(optionFilter)
		options, e := tx.QueryContext(ctx, `WITH profiles AS MATERIALIZED (
   SELECT l.account_id,l.extra->'request_profile' AS p FROM ops_system_logs l WHERE `+optionWhere+`
  ), option_values AS (
   SELECT 'model' AS kind,p->>'model' AS value FROM profiles
   UNION SELECT 'group',p->>'group_id' FROM profiles
   UNION SELECT 'account',account_id::text FROM profiles
   UNION SELECT 'account',e->>'account_id' FROM profiles CROSS JOIN LATERAL jsonb_array_elements(p->'events') e
  ) SELECT v.kind,v.value,COALESCE(NULLIF(CASE WHEN v.kind='group' THEN g.name WHEN v.kind='account' THEN a.name ELSE v.value END,''), CASE WHEN v.kind='group' THEN '已删除分组' ELSE '已删除账号' END)
  FROM option_values v LEFT JOIN groups g ON v.kind='group' AND g.id::text=v.value LEFT JOIN accounts a ON v.kind='account' AND a.id::text=v.value
  WHERE v.value IS NOT NULL AND v.value<>'' AND (v.kind='model' OR v.value<>'0') ORDER BY v.kind,3,v.value`, optionArgs...)
		if e != nil {
			return nil, e
		}
		for options.Next() {
			var o service.RequestProfileOption
			if e = options.Scan(&o.Kind, &o.Value, &o.Label); e != nil {
				options.Close()
				return nil, e
			}
			result.Options = append(result.Options, o)
		}
		e = options.Err()
		options.Close()
		if e != nil {
			return nil, e
		}
	}
	return result, tx.Commit()
}
