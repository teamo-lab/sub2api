package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetThroughputTrendIncludesTTFTPercentiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	bucket := start.Add(10 * time.Minute)
	accountID := int64(42)

	mock.ExpectQuery(regexp.QuoteMeta("ttft_p50_ms,\n  ttft_p90_ms\nFROM combined")).
		WithArgs(start, end, accountID, start, end, accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket", "request_count", "token_consumed", "input_tokens", "output_tokens",
			"cache_creation_tokens", "cache_read_tokens", "switch_count", "ttft_p50_ms", "ttft_p90_ms",
		}).AddRow(bucket, int64(8), int64(1000), int64(600), int64(300), int64(50), int64(50), int64(1), int64(240), int64(810)))
	repo := &opsRepository{db: db}
	result, err := repo.GetThroughputTrend(context.Background(), &service.OpsDashboardFilter{
		StartTime: start,
		EndTime:   end,
		AccountID: &accountID,
		QueryMode: service.OpsQueryModeRaw,
	}, 300)
	require.NoError(t, err)
	require.NotEmpty(t, result.Points)

	var point *service.OpsThroughputTrendPoint
	for _, candidate := range result.Points {
		if candidate.BucketStart.Equal(bucket) {
			point = candidate
			break
		}
	}
	require.NotNil(t, point)
	require.NotNil(t, point.TTFTP50MS)
	require.NotNil(t, point.TTFTP90MS)
	require.Equal(t, int64(240), *point.TTFTP50MS)
	require.Equal(t, int64(810), *point.TTFTP90MS)
	require.NoError(t, mock.ExpectationsWereMet())
}
