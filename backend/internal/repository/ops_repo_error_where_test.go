package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestBuildOpsErrorLogsWhere_QueryUsesQualifiedColumns(t *testing.T) {
	filter := &service.OpsErrorLogFilter{
		Query: "ACCESS_DENIED",
	}

	where, args := buildOpsErrorLogsWhere(filter)
	if where == "" {
		t.Fatalf("where should not be empty")
	}
	if len(args) != 1 {
		t.Fatalf("args len = %d, want 1", len(args))
	}
	if !strings.Contains(where, "e.request_id ILIKE $") {
		t.Fatalf("where should include qualified request_id condition: %s", where)
	}
	if !strings.Contains(where, "e.client_request_id ILIKE $") {
		t.Fatalf("where should include qualified client_request_id condition: %s", where)
	}
	if !strings.Contains(where, "e.error_message ILIKE $") {
		t.Fatalf("where should include qualified error_message condition: %s", where)
	}
}

func TestBuildOpsErrorLogsWhere_UserQueryUsesExistsSubquery(t *testing.T) {
	filter := &service.OpsErrorLogFilter{
		UserQuery: "admin@",
	}

	where, args := buildOpsErrorLogsWhere(filter)
	if where == "" {
		t.Fatalf("where should not be empty")
	}
	if len(args) != 1 {
		t.Fatalf("args len = %d, want 1", len(args))
	}
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id AND u.email ILIKE $") {
		t.Fatalf("where should include EXISTS user email condition: %s", where)
	}
}

func TestBuildOpsErrorLogsWhere_RouterCorrelationFilters(t *testing.T) {
	accountID := int64(1150)
	filter := &service.OpsErrorLogFilter{
		RouterUserID: "u-2fb74c8f6df14d4d891d8d5bce273bff",
		AnyAccountID: &accountID,
		Query:        "7dca1e02",
	}

	where, args := buildOpsErrorLogsWhere(filter)
	if !strings.Contains(where, "e.router_user_id = $") {
		t.Fatalf("where should filter router_user_id exactly: %s", where)
	}
	if !strings.Contains(where, "e.account_id = $") || !strings.Contains(where, "e.upstream_errors") {
		t.Fatalf("any_account_id should match final account or any upstream hop: %s", where)
	}
	if !strings.Contains(where, "e.router_request_id ILIKE $") || !strings.Contains(where, "e.router_user_id ILIKE $") {
		t.Fatalf("q should search router ids: %s", where)
	}
	foundUID, foundAccount := false, false
	for _, arg := range args {
		if arg == "u-2fb74c8f6df14d4d891d8d5bce273bff" {
			foundUID = true
		}
		if v, ok := arg.(int64); ok && v == accountID {
			foundAccount = true
		}
	}
	if !foundUID || !foundAccount {
		t.Fatalf("args missing router uid or account: %#v", args)
	}
}
