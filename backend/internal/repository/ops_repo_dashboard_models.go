package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ListDashboardModels returns models observed in successful or failed requests.
// The current model filter is intentionally ignored so the picker never removes
// the selected option from its own catalog.
func (r *opsRepository) ListDashboardModels(ctx context.Context, filter *service.OpsDashboardFilter) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if filter == nil || filter.StartTime.IsZero() || filter.EndTime.IsZero() {
		return nil, fmt.Errorf("start_time/end_time required")
	}

	catalogFilter := *filter
	catalogFilter.Model = ""
	start, end := filter.StartTime.UTC(), filter.EndTime.UTC()
	usageJoin, usageWhere, usageArgs, next := buildUsageWhere(&catalogFilter, start, end, 1)
	errorWhere, errorArgs, _ := buildErrorWhere(&catalogFilter, start, end, next)

	query := `
SELECT model
FROM (
  SELECT DISTINCT COALESCE(NULLIF(BTRIM(ul.requested_model), ''), ul.model) AS model
  FROM usage_logs ul
  ` + usageJoin + `
  ` + usageWhere + `
  UNION
  SELECT DISTINCT COALESCE(NULLIF(BTRIM(requested_model), ''), model) AS model
  FROM ops_error_logs
  ` + errorWhere + `
) models
WHERE model IS NOT NULL AND BTRIM(model) <> ''
ORDER BY model ASC
LIMIT 1000`

	rows, err := r.db.QueryContext(ctx, query, append(usageArgs, errorArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	models := make([]string, 0, 64)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}
