package migrations

import (
	"strings"
	"testing"
)

func TestOpsModelMetricsRollupMigrationContract(t *testing.T) {
	raw, err := FS.ReadFile("236_ops_model_metrics_rollups.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS ops_model_metrics_5m",
		"CREATE TABLE IF NOT EXISTS ops_model_metrics_hourly",
		"CREATE TABLE IF NOT EXISTS ops_model_metrics_coverage",
		"error_status_counts JSONB",
		"latency_2000_plus BIGINT",
		"resolution_seconds IN (300, 3600)",
		"idx_ops_model_metrics_5m_unique_dim",
		"idx_ops_model_metrics_hourly_unique_dim",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("migration missing %q", fragment)
		}
	}
}
