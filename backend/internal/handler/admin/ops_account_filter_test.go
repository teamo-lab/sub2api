package admin

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestParseOpsDashboardFilterAccountForcesRaw(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?time_range=1h&account_id=42&mode=preagg", nil)

	filter, err := parseOpsDashboardFilter(c, "1h")
	require.NoError(t, err)
	require.NotNil(t, filter.AccountID)
	require.Equal(t, int64(42), *filter.AccountID)
	require.Equal(t, service.OpsQueryModeRaw, filter.QueryMode)
}

func TestParseOpsDashboardFilterRejectsInvalidAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?account_id=invalid", nil)

	_, err := parseOpsDashboardFilter(c, "1h")
	require.EqualError(t, err, "Invalid account_id")
}

func TestOpsSnapshotCacheKeyIncludesAccount(t *testing.T) {
	first := int64(1)
	second := int64(2)
	a, err := json.Marshal(opsDashboardSnapshotV2CacheKey{AccountID: &first})
	require.NoError(t, err)
	b, err := json.Marshal(opsDashboardSnapshotV2CacheKey{AccountID: &second})
	require.NoError(t, err)
	require.NotEqual(t, string(a), string(b))
}
