//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func sellRangeTestGroup(id int64, rate float64) *Group {
	return &Group{
		ID:               id,
		Name:             "gemini-general",
		Platform:         PlatformGemini,
		Status:           StatusActive,
		Hydrated:         true,
		RateMultiplier:   rate,
		SubscriptionType: SubscriptionTypeStandard,
	}
}

func sellRangeTestAccount(id int64, priority int, sell *float64, groupID int64) Account {
	cost := 1.0
	return Account{
		ID:                 id,
		Name:               "acct",
		Platform:           PlatformGemini,
		Type:               AccountTypeAPIKey,
		Status:             StatusActive,
		Schedulable:        true,
		Concurrency:        2,
		Priority:           priority,
		RateMultiplier:     &cost,
		SellRateMultiplier: sell,
		AccountGroups:      []AccountGroup{{AccountID: id, GroupID: groupID}},
		GroupIDs:           []int64{groupID},
	}
}

func sellRangeTestService(accounts ...Account) *GatewayService {
	byID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		byID[accounts[i].ID] = &accounts[i]
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	return &GatewayService{
		accountRepo:        &mockAccountRepoForPlatform{accounts: accounts, accountsByID: byID},
		cache:              &mockGatewayCacheForPlatform{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}
}

func sellRangeTestContext(group *Group, r *SellRateRange) context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	return WithSellRateRange(ctx, r)
}

func TestParseSellRateRange(t *testing.T) {
	require.Nil(t, ParseSellRateRange("", ""))
	require.Nil(t, ParseSellRateRange("abc", "-1"))
	require.Nil(t, ParseSellRateRange("0", ""), "a zero minimum is no minimum")

	r := ParseSellRateRange("", " 3.85 ")
	require.NotNil(t, r)
	require.Nil(t, r.Min)
	require.InDelta(t, 3.85, *r.Max, 1e-9)

	r = ParseSellRateRange("1.4", "0")
	require.NotNil(t, r)
	require.InDelta(t, 1.4, *r.Min, 1e-9)
	require.InDelta(t, 0, *r.Max, 1e-9, "a zero maximum is a real (free-only) cap")

	require.True(t, (&SellRateRange{Max: f64(3.85)}).Contains(3.85))
	require.False(t, (&SellRateRange{Max: f64(3.85)}).Contains(3.8501))
	require.False(t, (&SellRateRange{Min: f64(1.4)}).Contains(1.0))
	require.True(t, (*SellRateRange)(nil).Contains(99))
}

func TestSellRateRangeSkipsAccountsAboveCapRegardlessOfPriority(t *testing.T) {
	group := sellRangeTestGroup(82, 4.727)
	pricey := sellRangeTestAccount(1132, 1, f64(4.727), group.ID)
	cheap := sellRangeTestAccount(1119, 2, f64(1.6), group.ID)
	svc := sellRangeTestService(pricey, cheap)

	// Without a range the priority-1 account wins.
	result, err := svc.SelectAccountWithLoadAwareness(sellRangeTestContext(group, nil), &group.ID, "", "", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, pricey.ID, result.Account.ID)
	result.ReleaseFunc()

	// A 5.5-fold user (3.85 RMB per official USD) only gets the cheap one.
	r := ParseSellRateRange("", "3.85")
	result, err = svc.SelectAccountWithLoadAwareness(sellRangeTestContext(group, r), &group.ID, "", "", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, cheap.ID, result.Account.ID)
	require.True(t, r.Rejected())
	result.ReleaseFunc()
}

func TestSellRateRangeNoAccountWithinCap(t *testing.T) {
	group := sellRangeTestGroup(82, 4.727)
	pricey := sellRangeTestAccount(1132, 1, f64(4.727), group.ID)
	mid := sellRangeTestAccount(1130, 2, f64(4.065), group.ID)
	svc := sellRangeTestService(pricey, mid)

	r := ParseSellRateRange("", "2.1")
	ctx := sellRangeTestContext(group, r)
	result, err := svc.SelectAccountWithLoadAwareness(ctx, &group.ID, "", "", nil, "", 0)
	require.Nil(t, result)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.True(t, r.Rejected(), "the handler tells this apart from capacity exhaustion")

	lowest, found := svc.LowestSellRateForModel(ctx, &group.ID, "")
	require.True(t, found)
	require.InDelta(t, 4.065, lowest, 1e-9)
	require.Equal(t, "sell_rate_out_of_range: no account within the requested sell rate range; min_available_sell_rate=4.0650", SellRateOutOfRangeMessage(lowest, found))
}

func TestSellRateRangeFallsBackToGroupRate(t *testing.T) {
	group := sellRangeTestGroup(82, 3.5)
	noOverride := sellRangeTestAccount(1, 1, nil, group.ID)
	svc := sellRangeTestService(noOverride)

	r := ParseSellRateRange("", "3.0")
	result, err := svc.SelectAccountWithLoadAwareness(sellRangeTestContext(group, r), &group.ID, "", "", nil, "", 0)
	require.Nil(t, result)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.True(t, r.Rejected())

	r = ParseSellRateRange("", "3.5")
	result, err = svc.SelectAccountWithLoadAwareness(sellRangeTestContext(group, r), &group.ID, "", "", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, noOverride.ID, result.Account.ID)
	require.False(t, r.Rejected())
	result.ReleaseFunc()
}
