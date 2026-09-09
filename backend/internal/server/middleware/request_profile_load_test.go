package middleware

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

func profileEvidenceDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PROFILE_EVIDENCE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := fmt.Sprintf("profile_evidence_%d", time.Now().UnixNano())
	if _, e = db.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	scoped, e := sql.Open("postgres", dsn+" search_path="+schema)
	if e != nil {
		t.Fatal(e)
	}
	scoped.SetMaxOpenConns(8)
	t.Cleanup(func() { scoped.Close(); db.Exec("DROP SCHEMA " + schema + " CASCADE"); db.Close() })
	_, e = scoped.Exec(`CREATE TABLE ops_system_logs(id bigserial PRIMARY KEY,created_at timestamptz,host text,level text,component text,message text,request_id text,client_request_id text,user_id bigint,api_key_id bigint,account_id bigint,platform text,model text,extra jsonb)`)
	if e != nil {
		t.Fatal(e)
	}
	return scoped
}
func TestProfileConcurrentIndexAllowsOnlineWrites(t *testing.T) {
	db := profileEvidenceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db.ExecContext(ctx, `INSERT INTO ops_system_logs(id,created_at,component,message,extra) VALUES(1,now(),'http.access','http request completed','{}')`)
	blocker, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.ExecContext(ctx, "UPDATE ops_system_logs SET model='held' WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	migration, e := migrations.FS.ReadFile("237_request_profile_lookup_notx.sql")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, err := db.ExecContext(ctx, string(migration)); done <- err }()
	locked := false
	for i := 0; i < 200; i++ {
		if e = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='ops_system_logs'::regclass AND mode='ShareUpdateExclusiveLock' AND granted)`).Scan(&locked); e != nil {
			t.Fatal(e)
		}
		if locked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !locked {
		t.Fatal("concurrent index did not acquire expected online lock")
	}
	writer, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer writer.Rollback()
	writer.ExecContext(ctx, "SET LOCAL lock_timeout='250ms'")
	start := time.Now()
	_, e = writer.ExecContext(ctx, "INSERT INTO ops_system_logs(id,created_at,extra) VALUES(2,now(),'{}')")
	if e != nil {
		t.Fatalf("online writer blocked: %v", e)
	}
	if e = writer.Commit(); e != nil {
		t.Fatal(e)
	}
	t.Logf("insert_during_concurrent_index_ms=%.3f", float64(time.Since(start).Microseconds())/1000)
	blocker.Rollback()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	var valid bool
	if e = db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid='idx_ops_request_profile_created'::regclass`).Scan(&valid); e != nil || !valid {
		t.Fatalf("invalid index %v %v", valid, e)
	}
}
func TestProfileMiddlewareSinkDatabaseCostEvidence(t *testing.T) {
	db := profileEvidenceDB(t)
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                 string
		enabled              bool
		spans, events, count int
	}{{"off", false, 20, 12, 2000}, {"normal", true, 20, 12, 2000}, {"cap", true, 512, 128, 300}} {
		t.Run(tc.name, func(t *testing.T) {
			db.Exec("TRUNCATE ops_system_logs")
			logger.Init(logger.InitOptions{Level: "warn", Format: "json", Sampling: logger.SamplingOptions{Enabled: true, Initial: 1, Thereafter: 100}})
			sink := service.NewOpsSystemLogSink(repository.NewOpsRepository(db))
			logger.SetSink(sink)
			defer logger.SetSink(nil)
			sink.Start()
			r := gin.New()
			r.Use(RequestLoggerWithProfiling(tc.enabled), LoggerWithProfiling(tc.enabled))
			r.POST("/responses", func(c *gin.Context) {
				ctx := c.Request.Context()
				requestprofile.Metadata(ctx, 2, "gpt-6-astra", "http")
				for i := 0; i < tc.spans; i++ {
					requestprofile.Start(ctx, "request_prepare")()
				}
				for i := 0; i < tc.events; i++ {
					requestprofile.Mark(ctx, "upstream_response", 200)
				}
				c.String(200, "same")
			})
			var lsn string
			db.QueryRow("SELECT pg_current_wal_insert_lsn()::text").Scan(&lsn)
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			times := make([]int64, 0, tc.count)
			peak := int64(0)
			begin := time.Now()
			for i := 0; i < tc.count; i++ {
				w := httptest.NewRecorder()
				start := time.Now()
				r.ServeHTTP(w, httptest.NewRequest("POST", "/responses", nil))
				times = append(times, time.Since(start).Nanoseconds())
				if w.Code != 200 || w.Body.String() != "same" {
					t.Fatal("response drift")
				}
				if q := sink.Health().QueueDepth; q > peak {
					peak = q
				}
			}
			serve := time.Since(begin)
			// Measure steady-state ingestion separately from shutdown behavior.
			deadline := time.Now().Add(10 * time.Second)
			if tc.enabled {
				for sink.Health().WrittenCount < uint64(tc.count) && time.Now().Before(deadline) {
					if sink.Health().WriteFailed > 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			runtime.ReadMemStats(&after)
			sink.Stop()
			health := sink.Health()
			var count, wal, stored int64
			var average float64
			db.QueryRow(`SELECT count(*),COALESCE(avg(octet_length(extra::text)),0),pg_total_relation_size('ops_system_logs') FROM ops_system_logs`).Scan(&count, &average, &stored)
			db.QueryRow("SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(),$1::pg_lsn)::bigint", lsn).Scan(&wal)
			sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
			p90 := float64(times[(len(times)-1)*9/10]) / 1000
			t.Logf("shape=%s requests=%d rows=%d handler_us_op=%.3f p90_us=%.3f allocated_bytes_op=%d payload_bytes_row=%.1f table_index_bytes=%d wal_bytes=%d peak_queue=%d dropped=%d write_failed=%d", tc.name, tc.count, count, float64(serve.Microseconds())/float64(tc.count), p90, (after.TotalAlloc-before.TotalAlloc)/uint64(tc.count), average, stored, wal, peak, health.DroppedCount, health.WriteFailed)
			expected := int64(0)
			if tc.enabled {
				expected = int64(tc.count)
			}
			if count != expected || health.DroppedCount != 0 || health.WriteFailed != 0 {
				t.Fatal("unexpected loss", count, health)
			}
		})
	}
}
