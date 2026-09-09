package service

import (
	"context"
	"database/sql"
	"time"
)

// Retire only the profiling payload. The original access log and its normal
// retention policy remain intact. Each run is bounded and skips busy rows.
func trimRequestProfiles(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var locked bool
	if err = tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock(7321909237)").Scan(&locked); err != nil || !locked {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout='250ms'"); err != nil {
		return 0, err
	}
	var total int64
	for i := 0; i < 10; i++ {
		result, e := tx.ExecContext(ctx, `WITH batch AS (SELECT id FROM ops_system_logs WHERE created_at<$1 AND extra ? 'request_profile' AND component='http.access' AND message='http request completed' AND extra->'request_profile'->>'evidence'='measured' ORDER BY created_at,id LIMIT 500 FOR UPDATE SKIP LOCKED) UPDATE ops_system_logs l SET extra=l.extra-'request_profile' FROM batch WHERE l.id=batch.id`, cutoff)
		if e != nil {
			return 0, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return 0, e
		}
		total += n
		if n < 500 {
			break
		}
	}
	return total, tx.Commit()
}
