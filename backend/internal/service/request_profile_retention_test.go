package service

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/lib/pq"
	"os"
	"testing"
	"time"
)

func TestProfileRetentionKeepsAccessLogAndOtherEvidence(t *testing.T) {
	dsn := os.Getenv("PROFILE_EVIDENCE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	schema := fmt.Sprintf("profile_retention_%d", time.Now().UnixNano())
	if _, e = db.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	defer db.Exec("DROP SCHEMA " + schema + " CASCADE")
	scoped, e := sql.Open("postgres", dsn+" search_path="+schema)
	if e != nil {
		t.Fatal(e)
	}
	defer scoped.Close()
	_, e = scoped.Exec(`CREATE TABLE ops_system_logs(id bigint PRIMARY KEY,created_at timestamptz,component text,message text,extra jsonb); INSERT INTO ops_system_logs VALUES(1,now()-interval '7 hours','http.access','http request completed','{"keep":"base","request_profile":{"evidence":"measured"}}'),(2,now()-interval '1 hour','http.access','http request completed','{"request_profile":{"evidence":"measured"}}'),(3,now()-interval '7 hours','http.access','http request completed','{"request_profile":{"evidence":"historical"}}'),(4,now()-interval '7 hours','audit','other','{"keep":"audit"}')`)
	if e != nil {
		t.Fatal(e)
	}
	n, e := trimRequestProfiles(context.Background(), scoped, time.Now().Add(-6*time.Hour))
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var count, profiles int
	var preserved bool
	scoped.QueryRow("SELECT count(*),count(*) FILTER(WHERE extra ? 'request_profile') FROM ops_system_logs").Scan(&count, &profiles)
	scoped.QueryRow("SELECT extra->>'keep'='base' FROM ops_system_logs WHERE id=1").Scan(&preserved)
	if count != 4 || profiles != 2 || !preserved {
		t.Fatal(count, profiles, preserved)
	}
}
