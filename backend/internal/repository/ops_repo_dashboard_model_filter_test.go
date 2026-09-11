package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestDashboardWhereBuildersIncludeModel(t *testing.T) {
	start := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	filter := &service.OpsDashboardFilter{Model: "gpt-5.6-sol"}

	_, usageWhere, usageArgs, next := buildUsageWhere(filter, start, end, 1)
	if !strings.Contains(usageWhere, "COALESCE(NULLIF(BTRIM(ul.requested_model), ''), ul.model) = $3") || len(usageArgs) != 3 || usageArgs[2] != "gpt-5.6-sol" {
		t.Fatalf("usage model filter missing: where=%q args=%v", usageWhere, usageArgs)
	}

	errorWhere, errorArgs, _ := buildErrorWhere(filter, start, end, next)
	if !strings.Contains(errorWhere, "COALESCE(NULLIF(BTRIM(requested_model), ''), model) = $6") || len(errorArgs) != 3 || errorArgs[2] != "gpt-5.6-sol" {
		t.Fatalf("error model filter missing: where=%q args=%v", errorWhere, errorArgs)
	}
}

func TestDashboardWhereBuildersIncludeAccount(t *testing.T) {
	start := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	accountID := int64(42)
	filter := &service.OpsDashboardFilter{AccountID: &accountID}

	_, usageWhere, usageArgs, next := buildUsageWhere(filter, start, end, 1)
	if !strings.Contains(usageWhere, "ul.account_id = $3") || len(usageArgs) != 3 || usageArgs[2] != accountID {
		t.Fatalf("usage account filter missing: where=%q args=%v", usageWhere, usageArgs)
	}

	errorWhere, errorArgs, _ := buildErrorWhere(filter, start, end, next)
	if !strings.Contains(errorWhere, "account_id = $6") || len(errorArgs) != 3 || errorArgs[2] != accountID {
		t.Fatalf("error account filter missing: where=%q args=%v", errorWhere, errorArgs)
	}
}
