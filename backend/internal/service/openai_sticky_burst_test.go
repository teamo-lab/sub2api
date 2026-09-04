package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStickyBurstExtraSlots(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{name: "missing defaults to one", want: 1},
		{name: "explicit zero disables", extra: map[string]any{OpenAIStickyBurstExtraKey: 0}, want: 0},
		{name: "explicit three", extra: map[string]any{OpenAIStickyBurstExtraKey: float64(3)}, want: 3},
		{name: "negative disables", extra: map[string]any{OpenAIStickyBurstExtraKey: -2}, want: 0},
		{name: "bounded", extra: map[string]any{OpenAIStickyBurstExtraKey: 99}, want: OpenAIStickyBurstMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, (&Account{Extra: tt.extra}).OpenAIStickyBurstExtraSlots())
		})
	}
}

func TestNormalizeOpenAIStickyBurstExtra(t *testing.T) {
	extra := map[string]any{OpenAIStickyBurstExtraKey: float64(3)}
	require.NoError(t, NormalizeOpenAIStickyBurstExtra(extra))
	require.Equal(t, 3, extra[OpenAIStickyBurstExtraKey])

	for _, invalid := range []any{-1, 11, 1.5, "not-a-number"} {
		require.Error(t, NormalizeOpenAIStickyBurstExtra(map[string]any{OpenAIStickyBurstExtraKey: invalid}))
	}
}

func TestOpenAIStickyBurstUsesAccountConfiguredExtraSlots(t *testing.T) {
	const accountID = int64(121)
	groupID := openAIStickyBurstCanaryGroupID
	sessionHash := "configured-burst"
	account := Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 3, Priority: 1,
		GroupIDs: []int64{groupID}, Extra: map[string]any{OpenAIStickyBurstExtraKey: 3},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: accountID},
		burstRecent: map[string]int64{
			fmt.Sprintf("%d:openai:%s", groupID, sessionHash): accountID,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
		cache:       cache,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{acquireByLimit: map[int64]map[int]bool{
			accountID: {3: false, 6: true},
		}}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "gpt-5.4-mini", nil)
	require.NoError(t, err)
	require.True(t, selection.Acquired)
	require.True(t, selection.StickyBurstBypass)
	require.Equal(t, int64(1), cache.burstMetrics["121:acquired"])
}

func TestOpenAIStickyBurstDefaultIsOneAndZeroDisables(t *testing.T) {
	groupID := openAIStickyBurstCanaryGroupID
	svc := &OpenAIGatewayService{cache: &stubGatewayCache{}}
	defaultAccount := &Account{ID: 1, Type: AccountTypeOAuth, Concurrency: 3}
	disabledAccount := &Account{ID: 2, Type: AccountTypeOAuth, Concurrency: 3, Extra: map[string]any{OpenAIStickyBurstExtraKey: 0}}
	require.True(t, svc.openAIStickyBurstEligible(&groupID, "gpt-5.4-mini", defaultAccount))
	require.False(t, svc.openAIStickyBurstEligible(&groupID, "gpt-5.4-mini", disabledAccount))
}
