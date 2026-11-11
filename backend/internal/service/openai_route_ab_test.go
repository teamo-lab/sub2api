package service

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setOpenAIRouteABConfigForTest(t *testing.T, raw string) {
	t.Helper()
	cfg, enabled := parseOpenAIRouteABConfig(raw)
	openAIRouteABConfigCache.Store(&cachedOpenAIRouteABConfig{
		cfg:       cfg,
		enabled:   enabled,
		expiresAt: time.Now().Add(time.Hour).UnixNano(),
	})
	t.Cleanup(func() { openAIRouteABConfigCache.Store(&cachedOpenAIRouteABConfig{}) })
}

func setOpenAIRouteRandForTest(t *testing.T, v float64) {
	t.Helper()
	prev := openAIRouteRandFloat
	openAIRouteRandFloat = func() float64 { return v }
	t.Cleanup(func() { openAIRouteRandFloat = prev })
}

func routeTestAccount(id int64, priority int, rate float64) *Account {
	r := rate
	return &Account{ID: id, Priority: priority, RateMultiplier: &r}
}

// feed 模拟 n 次首字为 ms 的成功请求。
func feedRouteSamples(st *openAIRouteStats, accountID int64, model, shape string, ms int, n int, now time.Time) {
	for i := 0; i < n; i++ {
		start := now.Add(-time.Duration(ms) * time.Millisecond)
		tk := st.startTicket(accountID, model, shape, nil, 180, start)
		tk.markFirstToken(now)
		tk.finish(true, now)
	}
}

func TestParseOpenAIRouteABConfig(t *testing.T) {
	_, ok := parseOpenAIRouteABConfig("")
	require.False(t, ok)
	_, ok = parseOpenAIRouteABConfig("{not json")
	require.False(t, ok)

	cfg, ok := parseOpenAIRouteABConfig(`{"groups":{"6":{"mode":"AB","percent":150},"7":{"mode":"off"}},"explore":0.9,"half_life_sec":-1}`)
	require.True(t, ok)
	def := defaultOpenAIRouteABConfig()
	require.Equal(t, def.HalfLifeSec, cfg.HalfLifeSec, "invalid half life falls back to default")
	require.Equal(t, def.Explore, cfg.Explore, "explore above 0.5 falls back to default")

	g6 := int64(6)
	g, ok := cfg.groupConfig(&g6)
	require.True(t, ok)
	require.Equal(t, openAIRouteModeAB, g.Mode)
	require.Equal(t, 100, g.Percent, "percent is clamped")

	g7 := int64(7)
	_, ok = cfg.groupConfig(&g7)
	require.False(t, ok, "unknown mode means not in experiment")
	g8 := int64(8)
	_, ok = cfg.groupConfig(&g8)
	require.False(t, ok)
	_, ok = cfg.groupConfig(nil)
	require.False(t, ok)
}

func TestOpenAIRouteSessionBucketIsStablePerSession(t *testing.T) {
	b1, s1 := openAIRouteSessionBucket("salt", "session-a")
	b2, s2 := openAIRouteSessionBucket("salt", "session-a")
	require.Equal(t, b1, b2)
	require.Equal(t, s1, s2)
	require.NotEmpty(t, s1)
	require.NotContains(t, s1, "session-a")

	_, s3 := openAIRouteSessionBucket("other-salt", "session-a")
	require.NotEqual(t, s1, s3, "salt changes the assignment")

	b, s := openAIRouteSessionBucket("salt", "")
	require.GreaterOrEqual(t, b, 0)
	require.Less(t, b, 100)
	require.Empty(t, s)

	// 分桶大致均匀：1000 个会话里落在 <50 的应在 400–600 之间。
	low := 0
	for i := 0; i < 1000; i++ {
		bucket, _ := openAIRouteSessionBucket("salt", "s-"+time.Duration(i).String())
		if bucket < 50 {
			low++
		}
	}
	require.InDelta(t, 500, low, 100)
}

func TestOpenAIRouteBodyBand(t *testing.T) {
	require.Equal(t, "?", openAIRouteBodyBand(0))
	require.Equal(t, "s", openAIRouteBodyBand(10<<10))
	require.Equal(t, "m", openAIRouteBodyBand(200<<10))
	require.Equal(t, "l", openAIRouteBodyBand(1<<20))
	require.Equal(t, "x", openAIRouteBodyBand(3<<20))
}

func TestOpenAIRouteEstimateNormalizesRequestMix(t *testing.T) {
	// 账号 1 只接重请求（xhigh、大上下文）、账号 2 只接轻请求，但两者相对各自档位都正常：
	// 慢系数都应接近 1，不能因为请求构成把 1 判成慢号。
	cfg := defaultOpenAIRouteABConfig()
	st := newOpenAIRouteStats()
	now := time.Now()
	heavy := openAIRouteShape("gpt-6.1-sol", "xhigh", "l")
	light := openAIRouteShape("gpt-6.1-sol", "low", "s")
	for i := 0; i < 20; i++ {
		feedRouteSamples(st, 3, "gpt-6.1-sol", heavy, 30000, 1, now) // 第三个号给两档都建基线
		feedRouteSamples(st, 3, "gpt-6.1-sol", light, 3000, 1, now)
		feedRouteSamples(st, 1, "gpt-6.1-sol", heavy, 30000, 1, now)
		feedRouteSamples(st, 2, "gpt-6.1-sol", light, 3000, 1, now)
	}
	e1 := st.estimate(routeTestAccount(1, 1, 0.16), "gpt-6.1-sol", light, cfg, now)
	e2 := st.estimate(routeTestAccount(2, 1, 0.16), "gpt-6.1-sol", light, cfg, now)
	require.InDelta(t, 1.0, e1.SlowFactor, 0.25)
	require.InDelta(t, 1.0, e2.SlowFactor, 0.25)
	require.InDelta(t, e1.PredMs, e2.PredMs, 1500)
}

func TestOpenAIRouteEstimatePrefersFastAccountAndShrinksSmallSamples(t *testing.T) {
	cfg := defaultOpenAIRouteABConfig()
	st := newOpenAIRouteStats()
	now := time.Now()
	shape := openAIRouteShape("gpt-6.1-sol", "high", "m")
	feedRouteSamples(st, 1, "gpt-6.1-sol", shape, 27000, 30, now) // Zero 变慢
	feedRouteSamples(st, 17, "gpt-6.1-sol", shape, 5000, 30, now)
	feedRouteSamples(st, 99, "gpt-6.1-sol", shape, 60000, 1, now) // 只有 1 个样本

	slow := st.estimate(routeTestAccount(1, 1, 0.16), "gpt-6.1-sol", shape, cfg, now)
	fast := st.estimate(routeTestAccount(17, 1, 0.15), "gpt-6.1-sol", shape, cfg, now)
	lucky := st.estimate(routeTestAccount(99, 1, 0.16), "gpt-6.1-sol", shape, cfg, now)
	require.Greater(t, slow.PredMs, fast.PredMs*3)
	require.Less(t, fast.Score, slow.Score)
	require.Less(t, lucky.SlowFactor, 60000.0/12000.0, "a single sample is shrunk toward the pool")
	require.InDelta(t, 0.15*cfg.CostMsPerRate, fast.CostMs, 1)
}

func TestOpenAIRouteEstimateDetectsSlowdownOfDominantAccount(t *testing.T) {
	// 今早的情形：Zero(1) 接八成多流量，后来变慢；codebot(17) 只接一小部分但正常。
	// 基线不能被 Zero 拖着走，Zero 必须被判慢。
	cfg := defaultOpenAIRouteABConfig()
	st := newOpenAIRouteStats()
	now := time.Now()
	shape := openAIRouteShape("gpt-6.1-sol", "high", "m")
	for i := 0; i < 10; i++ {
		feedRouteSamples(st, 1, "gpt-6.1-sol", shape, 25000, 9, now)
		feedRouteSamples(st, 17, "gpt-6.1-sol", shape, 5000, 1, now)
	}
	ests := st.estimateAll([]*Account{routeTestAccount(1, 1, 0.16), routeTestAccount(17, 1, 0.15)}, "gpt-6.1-sol", shape, cfg, now)
	require.Greater(t, ests[0].PredMs, 3*ests[1].PredMs)
	require.Greater(t, ests[0].SlowFactor, 1.5)
	require.Less(t, ests[1].SlowFactor, 0.7)
}

func TestOpenAIRouteEstimateCountsStalledInflightImmediately(t *testing.T) {
	cfg := defaultOpenAIRouteABConfig()
	st := newOpenAIRouteStats()
	now := time.Now()
	shape := openAIRouteShape("gpt-6.1-terra", "-", "s")
	feedRouteSamples(st, 1, "gpt-6.1-terra", shape, 3000, 20, now)
	feedRouteSamples(st, 4, "gpt-6.1-terra", shape, 3000, 20, now)

	// 账号 1 有 4 条在途：2 条已等 90 秒没首字（挂住），2 条刚开始。
	st.startTicket(1, "gpt-6.1-terra", shape, nil, 180, now.Add(-90*time.Second))
	st.startTicket(1, "gpt-6.1-terra", shape, nil, 180, now.Add(-90*time.Second))
	st.startTicket(1, "gpt-6.1-terra", shape, nil, 180, now.Add(-time.Second))
	st.startTicket(1, "gpt-6.1-terra", shape, nil, 180, now.Add(-time.Second))
	// 已出首字的长输出请求不算挂住。
	long := st.startTicket(4, "gpt-6.1-terra", shape, nil, 180, now.Add(-200*time.Second))
	long.markFirstToken(now.Add(-197 * time.Second))

	hung := st.estimate(routeTestAccount(1, 1, 0.16), "gpt-6.1-terra", shape, cfg, now)
	ok := st.estimate(routeTestAccount(4, 1, 0.16), "gpt-6.1-terra", shape, cfg, now)
	require.Equal(t, 2, hung.Stalled)
	require.Equal(t, 4, hung.Inflight)
	require.InDelta(t, 45000, hung.StallMs, 1000, "half of in-flight stuck at 90s adds ~45s")
	require.Zero(t, ok.Stalled)
	require.Greater(t, hung.Score, ok.Score+30000)
}

func TestOpenAIRouteEstimateErrorPenaltyAndDecay(t *testing.T) {
	cfg := defaultOpenAIRouteABConfig()
	st := newOpenAIRouteStats()
	now := time.Now()
	shape := openAIRouteShape("gpt-6-astra", "high", "m")
	for i := 0; i < 20; i++ {
		tk := st.startTicket(3, "gpt-6-astra", shape, nil, cfg.HalfLifeSec, now)
		tk.finish(false, now) // 429/502
	}
	bad := st.estimate(routeTestAccount(3, 1, 0.1), "gpt-6-astra", shape, cfg, now)
	require.Greater(t, bad.ErrorMs, 0.7*cfg.ErrorPenaltyMs)

	// 半衰期 180 秒，20 分钟后几乎没有影响。
	later := st.estimate(routeTestAccount(3, 1, 0.1), "gpt-6-astra", shape, cfg, now.Add(20*time.Minute))
	require.Less(t, later.ErrorMs, 0.05*cfg.ErrorPenaltyMs)
}

func TestOpenAIRouteSoftOrderMostlyPicksBestButExplores(t *testing.T) {
	cfg := defaultOpenAIRouteABConfig()
	items := []accountWithLoad{
		{account: routeTestAccount(1, 1, 0.16)},
		{account: routeTestAccount(17, 1, 0.15)},
	}
	estimates := []openAIRouteEstimate{{AccountID: 1, Score: 40000}, {AccountID: 17, Score: 15000}}

	first := map[int64]int{}
	r := uint64(1)
	rng := func() float64 { // 简单 LCG，结果可复现
		r = r*6364136223846793005 + 1442695040888963407
		return float64(r>>11) / float64(1<<53)
	}
	for i := 0; i < 2000; i++ {
		order := openAIRouteSoftOrder(items, estimates, cfg, rng)
		require.Len(t, order, 2)
		first[order[0].account.ID]++
	}
	// 分差 25s / 温度 5s → 慢号权重 e^-5；加 3% 探索，慢号约 2.2% 排第一。
	require.Greater(t, first[17], 1900)
	require.Greater(t, first[1], 10, "exploration keeps sending a little traffic to the slow account")
}

func TestOpenAIRouteOrderOnlyReordersArmB(t *testing.T) {
	setOpenAIRouteRandForTest(t, 0.5)
	st := newOpenAIRouteStats()
	now := time.Now()
	cfg := defaultOpenAIRouteABConfig()
	shape := openAIRouteShape("gpt-6.1-sol", "high", "m")
	feedRouteSamples(st, 1, "gpt-6.1-sol", shape, 30000, 30, now)
	feedRouteSamples(st, 17, "gpt-6.1-sol", shape, 5000, 30, now)
	available := []accountWithLoad{
		{account: routeTestAccount(1, 1, 0.16)},
		{account: routeTestAccount(17, 1, 0.15)},
		{account: routeTestAccount(2, 3, 0.5)}, // 兜底档，不参与评分
	}
	for _, arm := range []string{OpenAIRouteArmShadow, OpenAIRouteArmA, OpenAIRouteArmB} {
		sel := &openAIRouteSelection{stats: st, cfg: cfg, arm: arm, model: "gpt-6.1-sol", shape: shape, now: now, rng: openAIRouteRandFloat}
		order, decision := sel.order(available)
		require.Equal(t, int64(17), decision.Best, arm)
		require.Len(t, decision.Est, 2, "fallback-tier account is not scored")
		require.Equal(t, arm, decision.Arm)
		if arm == OpenAIRouteArmB {
			require.True(t, decision.Applied)
			require.Equal(t, int64(17), order[0].account.ID)
			require.Equal(t, int64(2), order[2].account.ID, "fallback tier stays last")
		} else {
			require.False(t, decision.Applied)
			require.Equal(t, int64(1), order[0].account.ID, "non-B arms keep legacy order")
		}
	}
}

func newRouteABSelectionTestService(t *testing.T, groupID int64) (*OpenAIGatewayService, *stubGatewayCache) {
	t.Helper()
	r1, r17 := 0.16, 0.15
	repo := groupAwareStubOpenAIAccountRepo{stubOpenAIAccountRepo{accounts: []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 200, Priority: 1, RateMultiplier: &r1, GroupIDs: []int64{groupID}},
		{ID: 17, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 500, Priority: 1, RateMultiplier: &r17, GroupIDs: []int64{groupID}},
	}}}
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	concurrencyCache := stubConcurrencyCache{
		acquireResults: map[int64]bool{1: true, 17: true},
		loadMap: map[int64]*AccountLoadInfo{
			1:  {AccountID: 1, LoadRate: 10},
			17: {AccountID: 17, LoadRate: 50}, // 老逻辑按负载率会选 1
		},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}
	// 账号 1 当前明显更慢。
	now := time.Now()
	shape := openAIRouteShape("gpt-6.1-sol", "-", "?")
	feedRouteSamples(svc.openAIRouteStats(), 1, "gpt-6.1-sol", shape, 30000, 30, now)
	feedRouteSamples(svc.openAIRouteStats(), 17, "gpt-6.1-sol", shape, 5000, 30, now)
	return svc, cache
}

func TestOpenAISelectAccountWithLoadAwareness_RouteABArms(t *testing.T) {
	const groupID = int64(6)
	setOpenAIRouteRandForTest(t, 0.5)
	cases := []struct {
		name     string
		config   string
		wantID   int64
		wantArm  string
		applied  bool
		recorded bool
	}{
		{name: "off", config: `{"groups":{}}`, wantID: 1},
		{name: "shadow", config: `{"groups":{"6":{"mode":"shadow"}}}`, wantID: 1, wantArm: OpenAIRouteArmShadow, recorded: true},
		{name: "ab arm A", config: `{"groups":{"6":{"mode":"ab","percent":0}}}`, wantID: 1, wantArm: OpenAIRouteArmA, recorded: true},
		{name: "ab arm B", config: `{"groups":{"6":{"mode":"ab","percent":100}}}`, wantID: 17, wantArm: OpenAIRouteArmB, applied: true, recorded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOpenAIRouteABConfigForTest(t, tc.config)
			svc, cache := newRouteABSelectionTestService(t, groupID)
			gid := groupID
			sessionHash := "route-ab-" + tc.name
			selection, err := svc.selectAccountWithLoadAwareness(context.Background(), &gid, PlatformOpenAI, sessionHash, "gpt-6.1-sol", nil, false, "", true)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, tc.wantID, selection.Account.ID)
			require.Equal(t, tc.wantID, cache.sessionBindings["openai:"+sessionHash], "new session binds to the chosen account")
			if !tc.recorded {
				require.Nil(t, selection.RouteDecision)
				return
			}
			require.NotNil(t, selection.RouteDecision)
			require.Equal(t, tc.wantArm, selection.RouteDecision.Arm)
			require.Equal(t, openAIRouteLayerLoad, selection.RouteDecision.Layer)
			require.Equal(t, int64(17), selection.RouteDecision.Best)
			require.Equal(t, tc.wantID, selection.RouteDecision.Picked)
			require.Equal(t, tc.applied, selection.RouteDecision.Applied)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}

			// 同一会话第二次请求命中粘性，两组都不迁移，只记 layer=sticky。
			again, err := svc.selectAccountWithLoadAwareness(context.Background(), &gid, PlatformOpenAI, sessionHash, "gpt-6.1-sol", nil, false, "", true)
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

func TestOpenAIRouteAttemptLifecycleOnGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setOpenAIRouteABConfigForTest(t, `{"groups":{"6":{"mode":"shadow"}}}`)
	svc := &OpenAIGatewayService{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

	// 不在实验里：什么都不写。
	svc.StartOpenAIRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 1}})
	require.Nil(t, OpenAIRouteDecisionJSON(c))

	shape := openAIRouteShape("gpt-6.1-sol", "high", "m")
	decision := &OpenAIRouteDecision{Arm: OpenAIRouteArmShadow, Layer: openAIRouteLayerLoad, Shape: shape, Best: 17, model: "gpt-6.1-sol", shape: shape}
	svc.StartOpenAIRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 1}, RouteDecision: decision})
	st := svc.openAIRouteStats()
	require.Len(t, st.inflight[1], 1)

	markOpenAIRouteFirstToken(c)
	svc.FinishOpenAIRouteAttempt(c, true)
	require.Empty(t, st.inflight[1])
	mean, w := st.cells["gpt-6.1-sol"][openAIRouteCellKey{accountID: 1, shape: shape}].value(time.Now(), 180)
	require.Equal(t, 1.0, math.Round(w))
	require.Less(t, math.Exp(mean), 1000.0, "ttft measured from attempt start")

	raw := OpenAIRouteDecisionJSON(c)
	require.NotNil(t, raw)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "S", got["arm"])
	require.Equal(t, float64(1), got["picked"])
	require.Equal(t, float64(17), got["best"])

	// 失败重选：上一张未结束的票据被结束，新的一张登记到新账号。
	svc.StartOpenAIRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 17}, RouteDecision: &OpenAIRouteDecision{Arm: "S", model: "gpt-6.1-sol", shape: shape}})
	svc.StartOpenAIRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 3}, RouteDecision: &OpenAIRouteDecision{Arm: "S", model: "gpt-6.1-sol", shape: shape}})
	require.Empty(t, st.inflight[17])
	require.Len(t, st.inflight[3], 1)
	svc.FinishOpenAIRouteAttempt(c, false)
	require.Empty(t, st.inflight[3])
}
