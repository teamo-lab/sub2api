package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newOpenAIEncryptedRecoveryTestStores(t *testing.T) (*miniredis.Miniredis, service.OpenAIEncryptedRecoveryStore, service.OpenAIEncryptedRecoveryStore) {
	mr := miniredis.RunT(t)
	blue, ok := NewGatewayCache(redis.NewClient(&redis.Options{Addr: mr.Addr()})).(service.OpenAIEncryptedRecoveryStore)
	require.True(t, ok)
	green, ok := NewGatewayCache(redis.NewClient(&redis.Options{Addr: mr.Addr()})).(service.OpenAIEncryptedRecoveryStore)
	require.True(t, ok)
	return mr, blue, green
}

func TestGatewayCacheOpenAIEncryptedRecoverySharedAcrossSlots(t *testing.T) {
	mr, blue, green := newOpenAIEncryptedRecoveryTestStores(t)
	ctx := context.Background()
	key := service.OpenAIEncryptedRecoveryKey{GroupID: 62, SessionHash: "sess", AccountID: 1094}

	require.NoError(t, blue.AddOpenAIEncryptedRejected(ctx, key, []string{"d1", "d2"}, 512, time.Hour))
	require.NoError(t, blue.RecordOpenAIEncryptedProvenance(ctx, 62, "sess", map[string]string{"d3": "host:zero.example.com#1094"}, 512, time.Hour))

	state, err := green.LoadOpenAIEncryptedRecoveryState(ctx, key, []string{"d1", "d3", "d4"})
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"d1": {}}, state.Rejected)
	require.Equal(t, map[string]string{"d3": "host:zero.example.com#1094"}, state.Provenance)
	require.False(t, state.Unrecoverable)

	// 被拒记录按账号隔离。
	other := key
	other.AccountID = 2000
	state, err = green.LoadOpenAIEncryptedRecoveryState(ctx, other, []string{"d1"})
	require.NoError(t, err)
	require.Empty(t, state.Rejected)

	require.NoError(t, green.MarkOpenAIEncryptedUnrecoverable(ctx, key, time.Hour))
	state, err = blue.LoadOpenAIEncryptedRecoveryState(ctx, key, nil)
	require.NoError(t, err)
	require.True(t, state.Unrecoverable)

	// TTL 跟随传入的粘性会话 TTL。
	require.InDelta(t, time.Hour.Seconds(), mr.TTL(openAIEncryptedRecoveryRejectedKey(key)).Seconds(), 1)
	mr.FastForward(time.Hour + time.Second)
	state, err = blue.LoadOpenAIEncryptedRecoveryState(ctx, key, []string{"d1", "d3"})
	require.NoError(t, err)
	require.Empty(t, state.Rejected)
	require.Empty(t, state.Provenance)
	require.False(t, state.Unrecoverable)
}

func TestGatewayCacheOpenAIEncryptedRecoveryCapsPerSession(t *testing.T) {
	mr, store, _ := newOpenAIEncryptedRecoveryTestStores(t)
	ctx := context.Background()
	key := service.OpenAIEncryptedRecoveryKey{GroupID: 62, SessionHash: "cap", AccountID: 1094}
	digests := make([]string, 0, 600)
	entries := make(map[string]string, 600)
	for i := 0; i < 600; i++ {
		d := fmt.Sprintf("d%03d", i)
		digests = append(digests, d)
		entries[d] = "openai#1"
	}
	require.NoError(t, store.AddOpenAIEncryptedRejected(ctx, key, digests, 512, time.Hour))
	require.NoError(t, store.RecordOpenAIEncryptedProvenance(ctx, 62, "cap", entries, 512, time.Hour))
	members, err := mr.SMembers(openAIEncryptedRecoveryRejectedKey(key))
	require.NoError(t, err)
	require.Len(t, members, 512)
	fields, err := mr.HKeys(openAIEncryptedRecoveryProvenanceKey(62, "cap"))
	require.NoError(t, err)
	require.Len(t, fields, 512)
}

func TestGatewayCacheOpenAIEncryptedRecoveryTokenBucket(t *testing.T) {
	_, blue, green := newOpenAIEncryptedRecoveryTestStores(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		ok, err := blue.AllowOpenAIEncryptedRetry(ctx, 1094, 3, now)
		require.NoError(t, err)
		require.True(t, ok)
	}
	// 桶在两个槽之间共享。
	ok, err := green.AllowOpenAIEncryptedRetry(ctx, 1094, 3, now)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = green.AllowOpenAIEncryptedRetry(ctx, 1094, 3, now.Add(20*time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = green.AllowOpenAIEncryptedRetry(ctx, 7, 3, now)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestGatewayCacheOpenAIEncryptedRecoveryMetric(t *testing.T) {
	mr, store, _ := newOpenAIEncryptedRecoveryTestStores(t)
	ctx := context.Background()
	require.NoError(t, store.IncrementOpenAIEncryptedRecoveryMetric(ctx, "strip_retry:acct=1094:mode=on:result=success", 0))
	require.NoError(t, store.IncrementOpenAIEncryptedRecoveryMetric(ctx, "strip_retry:acct=1094:mode=on:result=success", 0))
	key := openAIEncryptedRecoveryMetricKey(time.Now())
	require.Equal(t, "2", mr.HGet(key, "strip_retry:acct=1094:mode=on:result=success"))
	require.Greater(t, mr.TTL(key), 7*24*time.Hour)
}
