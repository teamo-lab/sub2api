package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestBuildOpsAlertEventsWhereIncludesAccount(t *testing.T) {
	accountID := int64(42)
	where, args := buildOpsAlertEventsWhere(&service.OpsAlertEventFilter{AccountID: &accountID})
	if !strings.Contains(where, "(dimensions->>'account_id') = $1") {
		t.Fatalf("alert account filter missing: where=%q", where)
	}
	if len(args) != 1 || args[0] != "42" {
		t.Fatalf("unexpected alert account args: %v", args)
	}
}
