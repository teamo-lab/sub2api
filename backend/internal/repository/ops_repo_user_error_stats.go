package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GetUserErrorStatsRaw 返回某用户的成功数(usage_logs)和错误计数(ops_error_logs),按时间桶聚合。
// 两边都强制 user_id 过滤;错误侧只取客户端实际收到的 status_code(不看上游状态码),
// 并排除 count_tokens 噪声和最终成功(status<400)的池内重试记录。
func (r *opsRepository) GetUserErrorStatsRaw(ctx context.Context, filter *service.OpsUserErrorStatsFilter) (*service.OpsUserErrorStatsRaw, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil || filter.UserID <= 0 {
		return nil, fmt.Errorf("user_id is required")
	}
	dateFormat := safeDateFormat(filter.Granularity)

	// usage_logs 与 ops_error_logs 的过滤列同名,两条查询共用同一组条件和参数。
	where, args := func() (string, []any) {
		conditions := []string{"user_id = $1", "created_at >= $2", "created_at < $3"}
		args := []any{filter.UserID, filter.StartTime.UTC(), filter.EndTime.UTC()}
		if filter.APIKeyID > 0 {
			args = append(args, filter.APIKeyID)
			conditions = append(conditions, fmt.Sprintf("api_key_id = $%d", len(args)))
		}
		if filter.GroupID > 0 {
			args = append(args, filter.GroupID)
			conditions = append(conditions, fmt.Sprintf("group_id = $%d", len(args)))
		}
		if model := strings.TrimSpace(filter.Model); model != "" {
			args = append(args, model)
			conditions = append(conditions, fmt.Sprintf("COALESCE(NULLIF(TRIM(requested_model), ''), model) = $%d", len(args)))
		}
		return strings.Join(conditions, " AND "), args
	}()

	out := &service.OpsUserErrorStatsRaw{}

	successQuery := fmt.Sprintf(`
SELECT TO_CHAR(created_at, '%s') AS bucket, COUNT(*)
FROM usage_logs
WHERE %s
GROUP BY 1`, dateFormat, where)
	rows, err := r.db.QueryContext(ctx, successQuery, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row service.OpsUserBucketCount
		if err := rows.Scan(&row.Bucket, &row.Count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out.Success = append(out.Success, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	errorQuery := fmt.Sprintf(`
SELECT TO_CHAR(created_at, '%s') AS bucket, status_code, is_business_limited, COUNT(*)
FROM ops_error_logs
WHERE %s
  AND COALESCE(status_code, 0) >= 400
  AND is_count_tokens = FALSE
GROUP BY 1, 2, 3`, dateFormat, where)
	rows, err = r.db.QueryContext(ctx, errorQuery, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row service.OpsUserErrorBucketRow
		if err := rows.Scan(&row.Bucket, &row.StatusCode, &row.BusinessLimited, &row.Count); err != nil {
			return nil, err
		}
		out.Errors = append(out.Errors, row)
	}
	return out, rows.Err()
}
