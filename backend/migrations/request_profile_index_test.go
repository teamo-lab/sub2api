package migrations

import (
	"strings"
	"testing"
)

func TestRequestProfileIndexIsNonTransactionalConcurrent(t *testing.T) {
	b, e := FS.ReadFile("237_request_profile_lookup_notx.sql")
	if e != nil {
		t.Fatal(e)
	}
	s := string(b)
	if !strings.Contains(s, "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Fatal("blocking index migration")
	}
	if _, e = FS.ReadFile("237_request_profile_lookup.sql"); e == nil {
		t.Fatal("transactional predecessor must not ship")
	}
}
