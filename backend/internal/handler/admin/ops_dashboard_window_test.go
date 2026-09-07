package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAlignModelDashboardPresetWindow(t *testing.T) {
	end := time.Date(2026, 9, 7, 10, 7, 43, 0, time.UTC)
	start := end.Add(-time.Hour)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?model=gpt-5&time_range=1h", nil)

	gotStart, gotEnd := alignModelDashboardPresetWindow(c, start, end)
	require.Equal(t, time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC), gotEnd)
	require.Equal(t, gotEnd.Add(-time.Hour), gotStart)
}

func TestAlignModelDashboardPresetWindowHourlyAndCustom(t *testing.T) {
	end := time.Date(2026, 9, 7, 10, 7, 43, 0, time.UTC)
	start := end.Add(-48 * time.Hour)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?model=gpt-5&time_range=48h", nil)
	gotStart, gotEnd := alignModelDashboardPresetWindow(c, start, end)
	require.Equal(t, time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC), gotEnd)
	require.Equal(t, gotEnd.Add(-48*time.Hour), gotStart)

	custom, _ := gin.CreateTestContext(httptest.NewRecorder())
	custom.Request = httptest.NewRequest("GET", "/?model=gpt-5&start_time=x&end_time=y", nil)
	gotStart, gotEnd = alignModelDashboardPresetWindow(custom, start, end)
	require.Equal(t, start, gotStart)
	require.Equal(t, end, gotEnd)
}
