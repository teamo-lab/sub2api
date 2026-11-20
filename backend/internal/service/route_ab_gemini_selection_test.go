//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// newGeminiRouteSelectionFixture 照 #82 neon 的现状：1132 优先级 1、慢、售价 4.727；
// 1136 优先级 2、快、售价 2.0、不认 generationConfig。老逻辑按优先级选 1132。
func newGeminiRouteSelectionFixture(t *testing.T) (*GatewayService, context.Context, *mockGatewayCacheForPlatform) {
	t.Helper()
	const groupID = int64(82)
	cost1132, sell1132 := 2.907, 4.727
	cost1136, sell1136 := 1.0, 2.0
	neon := map[string]any{"model_mapping": map[string]any{"gemini-4-neon": "gemini-4-neon"}}
	repo := &mockAccountRepoForPlatform{accounts: []Account{
		{ID: 1132, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 200, Priority: 1,
			RateMultiplier: &cost1132, SellRateMultiplier: &sell1132, Credentials: neon},
		{ID: 1136, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 100, Priority: 2,
			RateMultiplier: &cost1136, SellRateMultiplier: &sell1136, Credentials: neon,
			Extra: map[string]any{AccountRouteMissingCapsExtraKey: []any{RouteCapResponseSchema, RouteCapOutputLimits}}},
	}, accountsByID: map[int64]*Account{}}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	group := &Group{ID: groupID, Platform: PlatformGemini, Status: StatusActive, Hydrated: true, RateMultiplier: 4.727}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{}}
	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}},
		cache:       cache,
		cfg:         cfg,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{
			acquireResults: map[int64]bool{1132: true, 1136: true},
			loadMap: map[int64]*AccountLoadInfo{
				1132: {AccountID: 1132, LoadRate: 10},
				1136: {AccountID: 1136, LoadRate: 5},
			},
		}),
	}
	now := time.Now()
	shape := openAIRouteShape("gemini-4-neon", "", "s")
	feedRouteSamples(svc.routeStats(), 1132, "gemini-4-neon", shape, 140000, 20, now)
	feedRouteSamples(svc.routeStats(), 1136, "gemini-4-neon", shape, 25000, 20, now)
	return svc, context.WithValue(context.Background(), ctxkey.Group, group), cache
}

func TestGeminiSelectAccountWithLoadAwareness_RouteABArms(t *testing.T) {
	const groupID = int64(82)
	setOpenAIRouteRandForTest(t, 0.5)
	const groupCfg = `"price_basis":"sell","cost_ms_per_rate":10000,"max_priority":5`
	cases := []struct {
		name       string
		config     string
		needs      []string
		noRouteReq bool
		wantID     int64
		wantArm    string
		applied    bool
		recorded   bool
	}{
		{name: "off", config: `{"groups":{}}`, wantID: 1132},
		{name: "no route request in ctx", config: `{"groups":{"82":{"mode":"ab","percent":100,` + groupCfg + `}}}`, noRouteReq: true, wantID: 1132},
		{name: "shadow", config: `{"groups":{"82":{"mode":"shadow",` + groupCfg + `}}}`, wantID: 1132, wantArm: OpenAIRouteArmShadow, recorded: true},
		{name: "ab arm A", config: `{"groups":{"82":{"mode":"ab","percent":0,` + groupCfg + `}}}`, wantID: 1132, wantArm: OpenAIRouteArmA, recorded: true},
		{name: "ab arm B", config: `{"groups":{"82":{"mode":"ab","percent":100,` + groupCfg + `}}}`, wantID: 1136, wantArm: OpenAIRouteArmB, applied: true, recorded: true},
		{name: "ab arm B, request needs schema", config: `{"groups":{"82":{"mode":"ab","percent":100,` + groupCfg + `}}}`,
			needs: []string{RouteCapResponseSchema}, wantID: 1132, wantArm: OpenAIRouteArmB, applied: true, recorded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOpenAIRouteABConfigForTest(t, tc.config)
			svc, ctx, cache := newGeminiRouteSelectionFixture(t)
			if !tc.noRouteReq {
				ctx = WithGeminiRouteRequest(ctx, GeminiRouteRequest{Stream: true, BodyBytes: 2048, Needs: tc.needs})
			}
			gid := groupID
			sessionHash := "gemini-route-" + tc.name
			selection, err := svc.SelectAccountWithLoadAwareness(ctx, &gid, sessionHash, "gemini-4-neon", nil, "", 0)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, tc.wantID, selection.Account.ID)
			require.Equal(t, tc.wantID, cache.sessionBindings[sessionHash], "new session binds to the chosen account")
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if !tc.recorded {
				require.Nil(t, selection.RouteDecision)
				return
			}
			require.NotNil(t, selection.RouteDecision)
			require.Equal(t, tc.wantArm, selection.RouteDecision.Arm)
			require.Equal(t, openAIRouteLayerLoad, selection.RouteDecision.Layer)
			require.Equal(t, tc.wantID, selection.RouteDecision.Picked)
			require.Equal(t, tc.applied, selection.RouteDecision.Applied)
			if len(tc.needs) == 0 {
				require.Equal(t, int64(1136), selection.RouteDecision.Best)
			} else {
				require.Equal(t, int64(1132), selection.RouteDecision.Best, "accounts missing a needed capability are not scored")
			}

			// 同一会话第二次请求命中粘性，只记 layer=sticky、不迁移。
			again, err := svc.SelectAccountWithLoadAwareness(ctx, &gid, sessionHash, "gemini-4-neon", nil, "", 0)
			require.NoError(t, err)
			require.Equal(t, tc.wantID, again.Account.ID)
			require.NotNil(t, again.RouteDecision)
			require.Equal(t, openAIRouteLayerSticky, again.RouteDecision.Layer)
			if again.ReleaseFunc != nil {
				again.ReleaseFunc()
			}
		})
	}
}

func TestGeminiStickySkipsAccountMissingNeededCapability(t *testing.T) {
	setOpenAIRouteABConfigForTest(t, `{"groups":{}}`)
	svc, ctx, cache := newGeminiRouteSelectionFixture(t)
	cache.sessionBindings["s1"] = 1136
	gid := int64(82)

	plain := WithGeminiRouteRequest(ctx, GeminiRouteRequest{Stream: true})
	sel, err := svc.SelectAccountWithLoadAwareness(plain, &gid, "s1", "gemini-4-neon", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, int64(1136), sel.Account.ID, "sticky kept when nothing special is needed")
	if sel.ReleaseFunc != nil {
		sel.ReleaseFunc()
	}

	schema := WithGeminiRouteRequest(ctx, GeminiRouteRequest{Stream: true, Needs: []string{RouteCapResponseSchema}})
	sel, err = svc.SelectAccountWithLoadAwareness(schema, &gid, "s1", "gemini-4-neon", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, int64(1132), sel.Account.ID, "schema request leaves the sticky account that drops schemas")
	if sel.ReleaseFunc != nil {
		sel.ReleaseFunc()
	}
}
