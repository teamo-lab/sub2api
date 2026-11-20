package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var accountBreakdownCountColumns = []string{
	"account_id", "account_name", "platform", "status",
	"success_count", "error_total", "error_sla",
	"input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
}

func TestGetDashboardAccountBreakdownMergesCountsAndLatency(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	groupID := int64(80)

	mock.ExpectQuery(`FULL OUTER JOIN error_agg e ON e.account_id = u.account_id`).
		WithArgs(start, end, groupID, start, end, groupID).
		WillReturnRows(sqlmock.NewRows(accountBreakdownCountColumns).
			AddRow(int64(1119), "rootflow-a", "gemini", "active", int64(90), int64(12), int64(10), int64(1000), int64(200), int64(0), int64(300)).
			AddRow(int64(0), "", "", "", int64(0), int64(5), int64(5), int64(0), int64(0), int64(0), int64(0)).
			AddRow(int64(1128), "vertex-b", "gemini", "active", int64(0), int64(3), int64(0), int64(0), int64(0), int64(0), int64(0)))
	mock.ExpectQuery(`GROUP BY ul.account_id`).
		WithArgs(start, end, groupID).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "ttft_p50", "ttft_p90", "duration_p50", "duration_p90"}).
			AddRow(int64(1119), 812.4, 2400.0, 9100.0, 30500.6))

	repo := &opsRepository{db: db}
	out, err := repo.GetDashboardAccountBreakdown(context.Background(), &service.OpsDashboardFilter{
		StartTime: start, EndTime: end, GroupID: &groupID, QueryMode: service.OpsQueryModeRaw,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.False(t, out.LatencyDegraded)
	require.False(t, out.Truncated)
	require.Len(t, out.Items, 3)

	first := out.Items[0]
	require.Equal(t, int64(1119), first.AccountID)
	require.Equal(t, int64(102), first.RequestCountTotal)
	require.Equal(t, int64(100), first.RequestCountSLA)
	require.Equal(t, int64(1500), first.TokenConsumed)
	require.NotNil(t, first.SLA)
	require.InDelta(t, 0.9, *first.SLA, 1e-9)
	require.Equal(t, 812, *first.TTFTP50MS)
	require.Equal(t, 2400, *first.TTFTP90MS)
	require.Equal(t, 9100, *first.DurationP50MS)
	require.Equal(t, 30501, *first.DurationP90MS)

	unassigned := out.Items[1]
	require.Equal(t, int64(0), unassigned.AccountID)
	require.NotNil(t, unassigned.SLA)
	require.Equal(t, 0.0, *unassigned.SLA)
	require.Nil(t, unassigned.TTFTP50MS)

	// Only business-limited errors: no SLA samples, so SLA stays empty instead of 0%.
	limited := out.Items[2]
	require.Equal(t, int64(0), limited.RequestCountSLA)
	require.Nil(t, limited.SLA)
}

func TestGetDashboardAccountBreakdownLatencyTimeoutKeepsCounts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	mock.ExpectQuery(`FULL OUTER JOIN error_agg`).
		WillReturnRows(sqlmock.NewRows(accountBreakdownCountColumns).
			AddRow(int64(7), "acct", "openai", "active", int64(4), int64(0), int64(0), int64(10), int64(10), int64(0), int64(0)))
	mock.ExpectQuery(`GROUP BY ul.account_id`).WillReturnError(context.DeadlineExceeded)

	repo := &opsRepository{db: db}
	out, err := repo.GetDashboardAccountBreakdown(context.Background(), &service.OpsDashboardFilter{StartTime: start, EndTime: end})
	require.NoError(t, err)
	require.True(t, out.LatencyDegraded)
	require.Len(t, out.Items, 1)
	require.Equal(t, int64(4), out.Items[0].SuccessCount)
	require.Nil(t, out.Items[0].TTFTP90MS)
}

func TestGetDashboardAccountBreakdownTruncatesAtLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	start := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	rows := sqlmock.NewRows(accountBreakdownCountColumns)
	for i := 0; i <= service.OpsAccountBreakdownLimit; i++ {
		rows.AddRow(int64(i+1), "a", "openai", "active", int64(1), int64(0), int64(0), int64(1), int64(0), int64(0), int64(0))
	}
	mock.ExpectQuery(`LIMIT 501`).WillReturnRows(rows)
	mock.ExpectQuery(`GROUP BY ul.account_id`).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "ttft_p50", "ttft_p90", "duration_p50", "duration_p90"}))

	repo := &opsRepository{db: db}
	out, err := repo.GetDashboardAccountBreakdown(context.Background(), &service.OpsDashboardFilter{StartTime: start, EndTime: end})
	require.NoError(t, err)
	require.True(t, out.Truncated)
	require.Len(t, out.Items, service.OpsAccountBreakdownLimit)
}
