package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 选号读的是调度快照投影：能力声明必须穿过投影，否则 route_missing_caps 在线上不生效。
func TestSchedulerProjectionKeepsRouteMissingCaps(t *testing.T) {
	account := service.Account{
		ID:       1136,
		Platform: service.PlatformGemini,
		Extra: map[string]any{
			service.AccountRouteMissingCapsExtraKey: []any{service.RouteCapResponseSchema, service.RouteCapOutputLimits},
			"some_unrelated_key":                    "dropped",
		},
	}
	projected := buildSchedulerMetadataAccount(account)
	require.True(t, projected.RouteMissingCapability(service.RouteCapResponseSchema))
	require.True(t, projected.RouteMissingCapability(service.RouteCapOutputLimits))
	require.NotContains(t, projected.Extra, "some_unrelated_key")

	require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		service.AccountRouteMissingCapsExtraKey: []any{service.RouteCapResponseSchema},
	}), "changing the declaration must refresh the scheduler snapshot")
}
