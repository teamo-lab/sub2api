package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUpsertModelMetrics5mReplacesWindowAndMarksCoverageAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ops_model_metrics_5m WHERE bucket_start >= $1 AND bucket_start < $2")).
		WithArgs(start, end).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO ops_model_metrics_5m").WithArgs(start, end, 300).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec("INSERT INTO ops_model_metrics_coverage").WithArgs(start, end, 300).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	repo := &opsRepository{db: db}
	if err := repo.UpsertModelMetrics5m(context.Background(), start, end); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetModelMetricsCoverageBounds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT MAX\\(bucket_start\\)").WithArgs(300).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(want))
	mock.ExpectQuery("SELECT MIN\\(bucket_start\\)").WithArgs(300).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(want))
	mock.ExpectQuery("SELECT MAX\\(s.bucket\\)").WithArgs(300, want, want.Add(10*time.Minute)).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(want.Add(5 * time.Minute)))
	repo := &opsRepository{db: db}
	if got, ok, err := repo.GetLatestModelMetricsBucketStart(context.Background(), 300); err != nil || !ok || !got.Equal(want) {
		t.Fatalf("latest=(%v,%v,%v)", got, ok, err)
	}
	if got, ok, err := repo.GetOldestModelMetricsBucketStart(context.Background(), 300); err != nil || !ok || !got.Equal(want) {
		t.Fatalf("oldest=(%v,%v,%v)", got, ok, err)
	}
	if got, ok, err := repo.GetMissingModelMetricsBucketStart(context.Background(), 300, want, want.Add(10*time.Minute)); err != nil || !ok || !got.Equal(want.Add(5*time.Minute)) {
		t.Fatalf("missing=(%v,%v,%v)", got, ok, err)
	}
}
