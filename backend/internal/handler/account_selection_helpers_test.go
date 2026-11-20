package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func waitPlanSelection(id int64) *service.AccountSelectionResult {
	return &service.AccountSelectionResult{
		Account:  &service.Account{ID: id},
		WaitPlan: &service.AccountWaitPlan{AccountID: id, MaxConcurrency: 20},
	}
}

func acquiredSelection(id int64) *service.AccountSelectionResult {
	return &service.AccountSelectionResult{Account: &service.Account{ID: id}, Acquired: true, ReleaseFunc: func() {}}
}

func TestPreferAccountWithFreeSlotMovesOffFullAccount(t *testing.T) {
	first := waitPlanSelection(1132)
	failed := map[int64]struct{}{7: {}}
	var seen []map[int64]struct{}
	got := preferAccountWithFreeSlot(context.Background(), first, failed, func(excluded map[int64]struct{}) (*service.AccountSelectionResult, error) {
		snapshot := map[int64]struct{}{}
		for id := range excluded {
			snapshot[id] = struct{}{}
		}
		seen = append(seen, snapshot)
		if len(seen) == 1 {
			return waitPlanSelection(1130), nil // also full
		}
		return acquiredSelection(1136), nil
	})
	require.Equal(t, int64(1136), got.Account.ID)
	require.True(t, got.Acquired)
	require.Len(t, seen, 2)
	require.Contains(t, seen[0], int64(7), "earlier upstream failures stay excluded")
	require.Contains(t, seen[0], int64(1132))
	require.Contains(t, seen[1], int64(1130))
	require.Len(t, failed, 1, "the failover state is not touched")
}

func TestPreferAccountWithFreeSlotKeepsWaitPlanWhenNothingFree(t *testing.T) {
	first := waitPlanSelection(1132)
	got := preferAccountWithFreeSlot(context.Background(), first, nil, func(map[int64]struct{}) (*service.AccountSelectionResult, error) {
		return nil, service.ErrNoAvailableAccounts
	})
	require.Same(t, first, got)

	calls := 0
	got = preferAccountWithFreeSlot(context.Background(), first, nil, func(map[int64]struct{}) (*service.AccountSelectionResult, error) {
		calls++
		return waitPlanSelection(int64(2000 + calls)), nil
	})
	require.Same(t, first, got)
	require.Equal(t, maxBusyAccountReselects, calls)
}

func TestPreferAccountWithFreeSlotLeavesAcquiredSelectionAlone(t *testing.T) {
	first := acquiredSelection(1)
	got := preferAccountWithFreeSlot(context.Background(), first, nil, func(map[int64]struct{}) (*service.AccountSelectionResult, error) {
		return nil, errors.New("must not be called")
	})
	require.Same(t, first, got)
}

func TestInstallSellRateRangeReadsHeadersFromTrustedCaller(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := &service.APIKey{User: &service.User{Role: service.RoleAdmin}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Nil(t, installSellRateRange(c, admin))
	require.Nil(t, service.SellRateRangeFromContext(c.Request.Context()))

	c.Request.Header.Set(service.SellRateMaxHeader, "3.85")
	r := installSellRateRange(c, admin)
	require.NotNil(t, r)
	require.Same(t, r, service.SellRateRangeFromContext(c.Request.Context()))
	require.InDelta(t, 3.85, *r.Max, 1e-9)
}

func TestInstallSellRateRangeIgnoresUntrustedCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, key := range map[string]*service.APIKey{
		"ordinary user": {User: &service.User{Role: service.RoleUser}},
		"no user":       {},
		"no key":        nil,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Request.Header.Set(service.SellRateMaxHeader, "0.5")
		c.Request.Header.Set(service.SellRateMinHeader, "0.1")
		require.Nil(t, installSellRateRange(c, key), name)
		require.Nil(t, service.SellRateRangeFromContext(c.Request.Context()), name)
	}
}
