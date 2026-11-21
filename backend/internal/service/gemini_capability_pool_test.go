//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func capabilityPoolTestContext(group *Group) context.Context {
	return context.WithValue(context.Background(), ctxkey.Group, group)
}

func TestGeminiCapabilityMissCoversPool(t *testing.T) {
	group := sellRangeTestGroup(82, 1)
	a := sellRangeTestAccount(1, 1, nil, group.ID)
	b := sellRangeTestAccount(2, 1, nil, group.ID)

	t.Run("every account tried", func(t *testing.T) {
		svc := sellRangeTestService(a, b)
		require.True(t, svc.GeminiCapabilityMissCoversPool(capabilityPoolTestContext(group), &group.ID, "", map[int64]struct{}{1: {}, 2: {}}))
	})

	t.Run("untried account is only rate limited", func(t *testing.T) {
		// A 不支持，B 暂时限流（选号会过滤掉它）：B 没被验证过，不能证明所有账号都不支持。
		limited := b
		until := time.Now().Add(time.Minute)
		limited.RateLimitResetAt = &until
		svc := sellRangeTestService(a, limited)
		result, err := svc.SelectAccountWithLoadAwareness(capabilityPoolTestContext(group), &group.ID, "", "", map[int64]struct{}{1: {}}, "", 0)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrNoAvailableAccounts, "selection fails although B was never tried")
		require.False(t, svc.GeminiCapabilityMissCoversPool(capabilityPoolTestContext(group), &group.ID, "", map[int64]struct{}{1: {}}))
	})

	t.Run("disabled account does not count", func(t *testing.T) {
		off := b
		off.Schedulable = false
		svc := sellRangeTestService(a, off)
		require.True(t, svc.GeminiCapabilityMissCoversPool(capabilityPoolTestContext(group), &group.ID, "", map[int64]struct{}{1: {}}))
	})

	t.Run("nothing tried", func(t *testing.T) {
		svc := sellRangeTestService(a, b)
		require.False(t, svc.GeminiCapabilityMissCoversPool(capabilityPoolTestContext(group), &group.ID, "", nil))
	})
}
