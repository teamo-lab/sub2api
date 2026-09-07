package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestModelRollupForWindowBoundary(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	require.Equal(t, modelRollupSpec{"ops_model_metrics_5m", 300}, modelRollupFor(&service.OpsDashboardFilter{StartTime: start, EndTime: start.Add(24 * time.Hour)}))
	require.Equal(t, modelRollupSpec{"ops_model_metrics_hourly", 3600}, modelRollupFor(&service.OpsDashboardFilter{StartTime: start, EndTime: start.Add(24*time.Hour + time.Second)}))
}

func TestModelRollupWhereDimensions(t *testing.T) {
	groupID := int64(7)
	w, args := modelRollupWhere(&service.OpsDashboardFilter{Model: " gpt-5 ", Platform: "OpenAI", GroupID: &groupID}, 3)
	require.Equal(t, "model = $3 AND group_id = $4 AND platform = $5", w)
	require.Equal(t, []any{"gpt-5", int64(7), "openai"}, args)

	w, args = modelRollupWhere(&service.OpsDashboardFilter{Model: "gpt-5"}, 3)
	require.Equal(t, "model = $3 AND platform IS NULL AND group_id IS NULL", w)
	require.Equal(t, []any{"gpt-5"}, args)
}

func TestUnavailableModelRollupMode(t *testing.T) {
	ok, err := unavailableModelRollup(&service.OpsDashboardFilter{QueryMode: service.OpsQueryModeAuto})
	require.False(t, ok)
	require.NoError(t, err)
	ok, err = unavailableModelRollup(&service.OpsDashboardFilter{QueryMode: service.OpsQueryModePreagg})
	require.True(t, ok)
	require.ErrorIs(t, err, service.ErrOpsPreaggregatedNotPopulated)
}
