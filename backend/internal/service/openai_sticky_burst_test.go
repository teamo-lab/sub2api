package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"

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
	groupID := int64(80)
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
	groupID := int64(80)
	svc := &OpenAIGatewayService{cache: &stubGatewayCache{}}
	defaultAccount := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3}
	disabledAccount := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3, Extra: map[string]any{OpenAIStickyBurstExtraKey: 0}}
	require.True(t, svc.openAIStickyBurstEligible(&groupID, "gpt-5.4-mini", defaultAccount))
	require.False(t, svc.openAIStickyBurstEligible(&groupID, "gpt-5.4-mini", disabledAccount))
}

// A stateful counter catches accidental over-admission and verifies slot release,
// rather than merely stubbing every request at a particular limit as successful.
type stickyBurstSlots struct {
	stubConcurrencyCache
	mu     sync.Mutex
	active map[string]struct{}
}

func (c *stickyBurstSlots) AcquireAccountSlot(_ context.Context, _ int64, limit int, requestID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.active) >= limit {
		return false, nil
	}
	c.active[requestID] = struct{}{}
	return true, nil
}

func (c *stickyBurstSlots) ReleaseAccountSlot(_ context.Context, _ int64, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, requestID)
	return nil
}

func TestOpenAIStickyBurstFivePlusFiveAcrossSchedulingPaths(t *testing.T) {
	for _, groupID := range []int64{2, 3, 80, 987} {
		for _, path := range []string{"legacy", "legacy_no_batch", "advanced", "weighted_fallback"} {
			t.Run(fmt.Sprintf("group_%d/%s", groupID, path), func(t *testing.T) {
				ctx := context.Background()
				account := Account{ID: 121, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 1,
					GroupIDs: []int64{groupID}, Extra: map[string]any{OpenAIStickyBurstExtraKey: 5}}
				cache := &stubGatewayCache{sessionBindings: map[string]int64{"openai:warm": account.ID}}
				slots := &stickyBurstSlots{active: make(map[string]struct{})}
				svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{account}},
					cache: cache, concurrencyService: NewConcurrencyService(slots)}
				cfg := svc.schedulingConfig()
				if path == "legacy_no_batch" {
					cfg.LoadBatchEnabled = false
				}
				svc.cfg = &config.Config{Gateway: config.GatewayConfig{Scheduling: cfg}}
				// Keep the 11th request on the sticky wait path rather than testing
				// the independently configured capacity-escape policy here.
				svc.cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
				svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = false
				// Exercise the real observation hook: non-80 groups must receive a marker.
				svc.ObserveOpenAIStickyBurstResult(ctx, &groupID, "warm", account.ID, "gpt-5.6-sol", false, true, nil)
				selectSticky := func() (*AccountSelectionResult, error) {
					if path == "legacy" || path == "legacy_no_batch" {
						return svc.SelectAccountWithLoadAwareness(ctx, &groupID, "warm", "gpt-5.6-sol", nil)
					}
					scheduler := &defaultOpenAIAccountScheduler{service: svc, stats: newOpenAIAccountRuntimeStats()}
					req := OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI,
						SessionHash: "warm", StickyAccountID: account.ID, RequestedModel: "gpt-5.6-sol",
						RequiredTransport: OpenAIUpstreamTransportAny}
					if path == "weighted_fallback" {
						req.StickyWeighted = true
						return scheduler.tryFallbackToWeightedSticky(ctx, req)
					}
					selection, _, err := scheduler.selectBySessionHash(ctx, req)
					return selection, err
				}
				var releases []func()
				defer func() {
					for _, release := range releases {
						release()
					}
				}()
				for i := 0; i < 10; i++ {
					selection, err := selectSticky()
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.True(t, selection.Acquired, "slot %d must not queue", i+1)
					require.Nil(t, selection.WaitPlan)
					require.Equal(t, i >= 5, selection.StickyBurstBypass)
					require.Equal(t, 5, selection.Account.Concurrency)
					releases = append(releases, selection.ReleaseFunc)
				}
				selection, err := selectSticky()
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.False(t, selection.Acquired)
				require.NotNil(t, selection.WaitPlan)
				require.Equal(t, 10, selection.WaitPlan.MaxConcurrency)
				require.Len(t, slots.active, 10)
				// One completed burst slot is reusable immediately, without draining to 4.
				releases[9]()
				releases = releases[:9]
				selection, err = selectSticky()
				require.NoError(t, err)
				require.True(t, selection.Acquired)
				require.True(t, selection.StickyBurstBypass)
				releases = append(releases, selection.ReleaseFunc)
			})
		}
	}
}

func TestOpenAIStickyBurstRequiresMatchingSuccessfulSession(t *testing.T) {
	for _, tc := range []struct {
		name          string
		markerGroup   int64
		markerAccount int64
		succeeded     bool
		extra         int
		platform      string
		accountType   string
	}{
		{"cold", 3, 121, false, 5, PlatformOpenAI, AccountTypeOAuth},
		{"other_group", 2, 121, true, 5, PlatformOpenAI, AccountTypeOAuth},
		{"other_account", 3, 999, true, 5, PlatformOpenAI, AccountTypeOAuth},
		{"disabled", 3, 121, true, 0, PlatformOpenAI, AccountTypeOAuth},
		{"api_key", 3, 121, true, 5, PlatformOpenAI, AccountTypeAPIKey},
		{"other_platform", 3, 121, true, 5, PlatformGrok, AccountTypeOAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(3)
			account := &Account{ID: 121, Platform: tc.platform, Type: tc.accountType, Concurrency: 5,
				Extra: map[string]any{OpenAIStickyBurstExtraKey: tc.extra}}
			slots := &stickyBurstSlots{active: map[string]struct{}{"1": {}, "2": {}, "3": {}, "4": {}, "5": {}}}
			svc := &OpenAIGatewayService{cache: &stubGatewayCache{}, concurrencyService: NewConcurrencyService(slots)}
			svc.ObserveOpenAIStickyBurstResult(context.Background(), &tc.markerGroup, "session", tc.markerAccount, "gpt-5.6-sol", false, tc.succeeded, nil)
			result, burst, limit, err := svc.tryAcquireOpenAIStickySlot(context.Background(), &groupID, "session", "gpt-5.6-sol", account)
			require.NoError(t, err)
			require.False(t, result.Acquired)
			require.False(t, burst)
			require.Equal(t, 5, limit)
			require.Len(t, slots.active, 5)
		})
	}
}
