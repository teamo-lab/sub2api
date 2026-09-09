package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRequestProfileFiltersAreParameterized(t *testing.T) {
	where, args := requestProfileWhere(service.RequestProfileFilter{Model: "x' OR true --", GroupID: 38, AccountID: 72, ErrorType: "fallback", Evidence: "measured"})
	if strings.Contains(where, "OR true") || !strings.Contains(where, "jsonb_array_elements") || !strings.Contains(where, "l.account_id::text") || strings.Contains(where, "%!") {
		t.Fatal(where)
	}
	if len(args) != 7 {
		t.Fatal(args)
	}
}

// Uses TEMP tables on a single connection, never mutates application data.
func TestRequestProfilePostgresAggregationAndAttemptFiltering(t *testing.T) {
	dsn := os.Getenv("REQUEST_PROFILE_TEST_DSN")
	if dsn == "" {
		t.Skip("set REQUEST_PROFILE_TEST_DSN for PostgreSQL integration")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE ops_system_logs(id bigint,created_at timestamptz,host text,level text,component text,message text,request_id text,client_request_id text,account_id bigint,model text,extra jsonb); CREATE TEMP TABLE accounts(id bigint,name text); CREATE TEMP TABLE groups(id bigint,name text); INSERT INTO accounts VALUES(1,'a'),(2,'b'),(3,'c'); INSERT INTO groups VALUES(2,'group2'),(4,'group4')`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	for i, total := range []int{1000, 2000, 5000} {
		group, model, account, status := 2, "astra", 2, 200
		events := []map[string]any{{"kind": "attempt_start", "account_id": 2}}
		if i == 0 {
			events = []map[string]any{{"kind": "attempt_start", "account_id": 1}, {"kind": "upstream_response", "status": 503, "account_id": 1}, {"kind": "fallback", "account_id": 2}, {"kind": "attempt_start", "account_id": 2}}
		}
		if i == 2 {
			group, model, account, status = 4, "sol", 3, 500
			events = []map[string]any{{"kind": "network_error", "account_id": 3}}
		}
		p := map[string]any{"version": 1, "evidence": "measured", "total_us": total, "group_id": group, "model": model, "protocol": "sse", "dropped": 0, "attempts": 1, "events": events, "spans": []any{}, "segments": []map[string]any{{"name": "body_read", "start_us": 0, "duration_us": total / 10}, {"name": "unattributed", "start_us": total / 10, "duration_us": total - total/10}}}
		raw, _ := json.Marshal(map[string]any{"status_code": status, "request_profile": p})
		_, err = db.ExecContext(ctx, `INSERT INTO ops_system_logs VALUES($1,$2,'local','info','http.access','http request completed',$3,$3,$4,$5,$6)`, i+1, at, model+string(rune('a'+i)), account, model, string(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	r := &opsRepository{db: db}
	f := service.RequestProfileFilter{From: at.Add(-time.Minute), To: at.Add(time.Minute), GroupID: 2, Page: 1, Limit: 1, Evidence: "measured"}
	out, err := r.QueryRequestProfiles(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary.Count != 2 || out.Summary.MeanUS != 1500 || out.Summary.P90US != 1900 || len(out.Rows) != 1 || out.Rows[0].ID != 2 || out.Summary.Fallbacks != 1 {
		t.Fatalf("wrong aggregation %+v", out)
	}
	sum := 0.0
	for _, s := range out.Summary.Stages {
		sum += s.MeanUS
		if s.Name == "body_read" && s.MeanUS != 150 {
			t.Fatal(s)
		}
	}
	if sum != out.Summary.MeanUS {
		t.Fatal("stage sum differs", sum)
	}
	f.GroupID = 0
	f.Models = []string{"astra", "sol"}
	f.GroupIDs = []string{"2", "4"}
	f.AccountIDs = []string{"1", "3"}
	f.IncludeOptions = true
	out, err = r.QueryRequestProfiles(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary.Count != 2 || out.Summary.MeanUS != 3000 || len(out.Options) != 7 {
		t.Fatalf("multi-filter/options: %+v", out)
	}
	foundFirstAccount := false
	for _, o := range out.Options {
		if o.Kind == "account" && o.Value == "1" && o.Label == "a" {
			foundFirstAccount = true
		}
	}
	if !foundFirstAccount {
		t.Fatal("missing attempt account option")
	}
	f.Models = nil
	f.GroupIDs = nil
	f.AccountIDs = nil
	f.IncludeOptions = false
	f.GroupID = 2
	f.AccountID = 1
	out, err = r.QueryRequestProfiles(ctx, f)
	if err != nil || out.Summary.Count != 1 || out.Rows[0].ID != 1 {
		t.Fatalf("first fallback account missing %+v %v", out, err)
	}
	f.AccountID = 0
	f.ErrorType = "fallback"
	out, err = r.QueryRequestProfiles(ctx, f)
	if err != nil || out.Summary.Count != 1 {
		t.Fatalf("fallback filter %+v %v", out, err)
	}
	f.GroupID = 0
	f.ErrorType = "network_error"
	out, err = r.QueryRequestProfiles(ctx, f)
	if err != nil || out.Summary.Count != 1 || out.Rows[0].ID != 3 {
		t.Fatalf("network filter %+v %v", out, err)
	}
	f.ErrorType = ""
	f.Evidence = "historical"
	out, err = r.QueryRequestProfiles(ctx, f)
	if err != nil || out.Summary.Count != 0 {
		t.Fatalf("evidence mixed %+v %v", out, err)
	}
}
