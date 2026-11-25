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

// 选号按请求体上限过滤账号时读的也是投影：上限必须穿过投影，否则调度器路径上不生效。
func TestSchedulerProjectionKeepsMaxRequestBodyBytes(t *testing.T) {
	account := service.Account{
		ID:       564,
		Platform: service.PlatformOpenAI,
		Extra: map[string]any{
			service.AccountExtraMaxRequestBodyBytes: float64(32 << 20),
			"some_unrelated_key":                    "dropped",
		},
	}
	projected := buildSchedulerMetadataAccount(account)
	require.Equal(t, int64(32<<20), projected.OpenAIMaxRequestBodyBytes())
	require.NotContains(t, projected.Extra, "some_unrelated_key")

	require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		service.AccountExtraMaxRequestBodyBytes: float64(16 << 20),
	}), "changing the cap must refresh the scheduler snapshot")
}
