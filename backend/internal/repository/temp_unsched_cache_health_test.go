package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAPIKeyHealthCacheTripsWithinRollingWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewTempUnschedCache(client).(service.OpenAIAPIKeyHealthCache)
	require.True(t, ok)

	ctx := context.Background()
	for attempt := 1; attempt <= 3; attempt++ {
		count, tripped, err := store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 3)
		require.NoError(t, err)
		require.EqualValues(t, attempt, count)
		require.Equal(t, attempt == 3, tripped)
	}
}

func TestOpenAIAPIKeyHealthCacheDropsFailuresOutsideRollingWindow(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	server.SetTime(now)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewTempUnschedCache(client).(service.OpenAIAPIKeyHealthCache)
	require.True(t, ok)

	ctx := context.Background()
	count, tripped, err := store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.False(t, tripped)

	server.SetTime(now.Add(61 * time.Second))
	count, tripped, err = store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.False(t, tripped)
}

func TestPoolModelFailureCacheCountsEachModelSeparately(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewTempUnschedCache(client).(service.PoolModelFailureCache)
	require.True(t, ok)

	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		count, tripped, err := store.RecordPoolModelFailure(ctx, 1133, "gemini-3.8-flash", 1, 3)
		require.NoError(t, err)
		require.EqualValues(t, attempt, count)
		require.False(t, tripped)
	}
	count, tripped, err := store.RecordPoolModelFailure(ctx, 1133, "gemini-4-carbon", 1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "another model on the same account has its own window")
	require.False(t, tripped)

	count, tripped, err = store.RecordPoolModelFailure(ctx, 1133, "gemini-3.8-flash", 1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
	require.True(t, tripped)

	count, _, err = store.RecordPoolModelFailure(ctx, 1133, "gemini-3.8-flash", 1, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "window resets after tripping")
}
