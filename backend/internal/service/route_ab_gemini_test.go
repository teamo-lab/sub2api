package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestParseGeminiNativeRouteRequest(t *testing.T) {
	req := ParseGeminiNativeRouteRequest([]byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingLevel":"HIGH"},"responseMimeType":"application/json","maxOutputTokens":512}}`), true)
	require.True(t, req.Stream)
	require.Equal(t, "high", req.Thinking)
	require.ElementsMatch(t, []string{RouteCapResponseSchema, RouteCapOutputLimits}, req.Needs)

	req = ParseGeminiNativeRouteRequest([]byte(`{"generation_config":{"thinking_config":{"thinking_budget":0},"response_schema":{"type":"OBJECT"}}}`), false)
	require.Equal(t, "off", req.Thinking)
	require.Equal(t, []string{RouteCapResponseSchema}, req.Needs)

	req = ParseGeminiNativeRouteRequest([]byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":24576},"temperature":0.2,"responseMimeType":"text/plain"}}`), true)
	require.Equal(t, "high", req.Thinking)
	require.Empty(t, req.Needs, "temperature / text mime alone do not require anything")

	req = ParseGeminiNativeRouteRequest([]byte(`{"contents":[]}`), true)
	require.Empty(t, req.Thinking)
	require.Empty(t, req.Needs)
	require.Equal(t, len(`{"contents":[]}`), req.BodyBytes)
}

func TestParseChatCompletionsRouteRequest(t *testing.T) {
	req := ParseChatCompletionsRouteRequest([]byte(`{"model":"gemini-4-neon","reasoning_effort":"Low","response_format":{"type":"json_schema"},"max_tokens":100}`), true)
	require.Equal(t, "low", req.Thinking)
	require.ElementsMatch(t, []string{RouteCapResponseSchema, RouteCapOutputLimits}, req.Needs)

	req = ParseChatCompletionsRouteRequest([]byte(`{"model":"gemini-4-neon","response_format":{"type":"text"},"temperature":1,"stop":null}`), false)
	require.Empty(t, req.Needs)
}

func TestAccountRouteMissingCapabilityAndPreferFitting(t *testing.T) {
	plain := &Account{ID: 1}
	listed := &Account{ID: 2, Extra: map[string]any{AccountRouteMissingCapsExtraKey: []any{"response_schema", " output_limits "}}}
	csv := &Account{ID: 3, Extra: map[string]any{AccountRouteMissingCapsExtraKey: "output_limits"}}
	require.False(t, plain.RouteMissingCapability(RouteCapResponseSchema))
	require.True(t, listed.RouteMissingCapability(RouteCapResponseSchema))
	require.True(t, listed.RouteMissingCapability(RouteCapOutputLimits))
	require.True(t, csv.RouteMissingCapability(RouteCapOutputLimits))
	require.False(t, csv.RouteMissingCapability(RouteCapResponseSchema))

	all := []*Account{plain, listed, csv}
	require.Equal(t, all, preferRouteFittingAccounts(context.Background(), all), "no request needs: keep everyone")

	ctx := WithGeminiRouteRequest(context.Background(), GeminiRouteRequest{Needs: []string{RouteCapResponseSchema}})
	require.Equal(t, []*Account{plain, csv}, preferRouteFittingAccounts(ctx, all))
	require.False(t, routeAccountFitsRequest(ctx, listed))

	// 没有号满足时不排除（宁可降级也不无号可用）。
	require.Equal(t, []*Account{listed}, preferRouteFittingAccounts(ctx, []*Account{listed}))
}

func TestRouteABGroupOverridesAndSellPriceBasis(t *testing.T) {
	cfg, ok := parseOpenAIRouteABConfig(`{"groups":{"82":{"mode":"shadow","price_basis":"SELL","cost_ms_per_rate":10000,"max_priority":5,"error_penalty_ms":60000,"stall_min_ms":30000},"6":{"mode":"shadow","cost_ms_per_rate":-1}}}`)
	require.True(t, ok)

	gid := int64(82)
	g, ok := cfg.groupConfig(&gid)
	require.True(t, ok)
	eff := cfg.forGroup(g, 4.727)
	require.Equal(t, routePriceBasisSell, eff.priceBasis)
	require.Equal(t, 10000.0, eff.CostMsPerRate)
	require.Equal(t, 5, eff.MaxPriority)
	require.Equal(t, 60000.0, eff.ErrorPenaltyMs)
	require.Equal(t, 30000.0, eff.StallMinMs)

	gid6 := int64(6)
	g6, ok := cfg.groupConfig(&gid6)
	require.True(t, ok)
	eff6 := cfg.forGroup(g6, 0)
	require.Equal(t, routePriceBasisCost, eff6.priceBasis)
	require.Equal(t, cfg.CostMsPerRate, eff6.CostMsPerRate, "invalid override keeps the global value")
	require.Equal(t, cfg.MaxPriority, eff6.MaxPriority)

	cost, sell := 2.907, 4.727
	withSell := &Account{ID: 1, RateMultiplier: &cost, SellRateMultiplier: &sell}
	noSell := &Account{ID: 2, RateMultiplier: &cost}
	require.InDelta(t, 4.727, eff.priceRate(withSell), 1e-9)
	require.InDelta(t, 4.727, eff.priceRate(noSell), 1e-9, "no account sell price: falls back to the group rate")
	require.InDelta(t, 2.907, eff6.priceRate(withSell), 1e-9, "cost basis ignores sell price")
	require.InDelta(t, 2.907, cfg.forGroup(g, 0).priceRate(noSell), 1e-9, "sell basis without group rate falls back to cost")
}

func TestRouteEstimateUsesSellPriceUnderSellBasis(t *testing.T) {
	st := newOpenAIRouteStats()
	now := time.Now()
	shape := openAIRouteShape("gemini-4-neon", "-", "s")
	feedRouteSamples(st, 1132, "gemini-4-neon", shape, 20000, 20, now)
	feedRouteSamples(st, 1136, "gemini-4-neon", shape, 20000, 20, now)
	cost1132, sell1132 := 2.907, 4.727
	cost1136, sell1136 := 1.0, 2.0
	a := &Account{ID: 1132, RateMultiplier: &cost1132, SellRateMultiplier: &sell1132}
	b := &Account{ID: 1136, RateMultiplier: &cost1136, SellRateMultiplier: &sell1136}

	cfg, _ := parseOpenAIRouteABConfig(`{"groups":{"82":{"mode":"shadow","price_basis":"sell","cost_ms_per_rate":10000}}}`)
	gid := int64(82)
	g, _ := cfg.groupConfig(&gid)
	ests := st.estimateAll([]*Account{a, b}, "gemini-4-neon", shape, cfg.forGroup(g, 4.727), now)
	require.InDelta(t, 47270, ests[0].CostMs, 1)
	require.InDelta(t, 20000, ests[1].CostMs, 1)
	require.Less(t, ests[1].Score, ests[0].Score, "same speed: the cheaper sell price wins")
}

func TestGeminiPayloadHasAnswer(t *testing.T) {
	require.False(t, geminiPayloadHasAnswer([]byte(`{"candidates":[{"content":{"parts":[{"text":"thinking...","thought":true}]}}]}`)))
	require.False(t, geminiPayloadHasAnswer([]byte(`{"usageMetadata":{"promptTokenCount":3}}`)))
	require.True(t, geminiPayloadHasAnswer([]byte(`{"candidates":[{"content":{"parts":[{"text":"x","thought":true},{"text":"hi"}]}}]}`)))
	require.True(t, geminiPayloadHasAnswer([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}]}`)))
}

func TestGatewayRouteAttemptLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &GatewayService{}
	setOpenAIRouteABConfigForTest(t, `{"groups":{"82":{"mode":"shadow"}}}`)
	shape := openAIRouteShape("gemini-4-neon", "-", "s")
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-neon:streamGenerateContent", nil)
		return c
	}
	decision := func() *OpenAIRouteDecision {
		return &OpenAIRouteDecision{Arm: OpenAIRouteArmShadow, Layer: openAIRouteLayerLoad, model: "gemini-4-neon", shape: shape}
	}

	// 流式：thought 片段不算首字，第一个回答片段才算。
	c := newCtx()
	svc.StartRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 1136}, RouteDecision: decision()}, true)
	tk := openAIRouteTicketFromGin(c)
	require.NotNil(t, tk)
	markGeminiRouteFirstTokenIfAnswer(c, []byte(`{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}`))
	require.True(t, tk.wantsFirstToken())
	markGeminiRouteFirstTokenIfAnswer(c, []byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`))
	require.False(t, tk.wantsFirstToken())
	svc.FinishRouteAttempt(c, true, true)
	require.NotEmpty(t, RouteDecisionJSON(c))

	st := svc.routeStats()
	st.mu.Lock()
	_, hasCell := st.cells["gemini-4-neon"][openAIRouteCellKey{accountID: 1136, shape: shape}]
	st.mu.Unlock()
	require.True(t, hasCell)

	// 非流式成功：整条响应耗时记作本档首字样本；失败不记首字。
	c = newCtx()
	nsShape := shape + routeShapeNonStreamSuffix
	d := decision()
	d.shape = nsShape
	svc.StartRouteAttempt(c, &AccountSelectionResult{Account: &Account{ID: 1132}, RouteDecision: d}, false)
	svc.FinishRouteAttempt(c, false, true)
	c2 := newCtx()
	d2 := decision()
	d2.shape = nsShape
	svc.StartRouteAttempt(c2, &AccountSelectionResult{Account: &Account{ID: 1137}, RouteDecision: d2}, false)
	svc.FinishRouteAttempt(c2, false, false)
	st.mu.Lock()
	_, okSuccess := st.cells["gemini-4-neon"][openAIRouteCellKey{accountID: 1132, shape: nsShape}]
	_, okFailed := st.cells["gemini-4-neon"][openAIRouteCellKey{accountID: 1137, shape: nsShape}]
	errMean, _ := st.errs[1137].value(time.Now(), 180)
	st.mu.Unlock()
	require.True(t, okSuccess)
	require.False(t, okFailed)
	require.Equal(t, 1.0, errMean)

	// 不在实验里（没有决策）时什么都不登记。
	c3 := newCtx()
	svc.StartRouteAttempt(c3, &AccountSelectionResult{Account: &Account{ID: 1}}, true)
	require.Nil(t, openAIRouteTicketFromGin(c3))
	svc.FinishRouteAttempt(c3, true, true)
	require.Nil(t, RouteDecisionJSON(c3))
}
