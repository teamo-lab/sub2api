package service

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetRouterOutcomeWithoutRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, RouterContractV2Requested(c))
	require.NotPanics(t, func() { SetRouterOutcome(c, RouterOutcomeRetryableAbort) })
	require.Empty(t, c.Writer.Header().Get(RouterOutcomeHeader))
}

func TestSetRouterOutcomeIsOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		header     string
		wantHeader string
	}{
		{name: "ordinary caller", header: "", wantHeader: ""},
		{name: "wrong version", header: "v1", wantHeader: ""},
		{name: "contract v2", header: "v2", wantHeader: "retryable_abort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			if tc.header != "" {
				c.Request.Header.Set(RouterContractHeader, tc.header)
			}
			SetRouterOutcome(c, RouterOutcomeRetryableAbort)
			if tc.wantHeader == "" {
				require.Empty(t, w.Header().Get(RouterOutcomeHeader))
			} else {
				require.Equal(t, tc.wantHeader, w.Header().Get(RouterOutcomeHeader))
				require.Equal(t, RouterContractV2, w.Header().Get(RouterContractHeader))
			}
		})
	}
}

func TestSetRouterOutcomeDoesNotWriteAfterResponse(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	c.String(200, "already committed")
	SetRouterOutcome(c, RouterOutcomeRetryableAbort)
	require.Empty(t, w.Header().Get(RouterOutcomeHeader))
}

func TestRouterUpstreamAttemptsPreserveFallbackOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	BeginRouterUpstreamAttempts(c)
	RecordRouterUpstreamAccount(c, 12)
	MarkRouterUpstreamAttemptFailed(c, 12, http.StatusTooManyRequests, "rate_limit", time.Now().UnixMilli())
	RecordRouterUpstreamAccount(c, 57)

	var attempts []RouterUpstreamAttempt
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(RouterAttemptsHeader)), &attempts))
	require.Len(t, attempts, 2)
	require.Equal(t, int64(12), attempts[0].AccountID)
	require.Equal(t, "failed", attempts[0].Result)
	require.Equal(t, http.StatusTooManyRequests, attempts[0].Status)
	require.Equal(t, int64(57), attempts[1].AccountID)
	require.Equal(t, "pending", attempts[1].Result)
}

func TestRouterUpstreamAttemptsStayPrivate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	BeginRouterUpstreamAttempts(c)
	RecordRouterUpstreamAccount(c, 12)
	require.Empty(t, recorder.Header().Get(RouterAttemptsHeader))
}

func TestRouterUpstreamAttemptsStartUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	BeginRouterUpstreamAttempts(c)
	require.JSONEq(t, `[{"account_id":0,"result":"pending"}]`, recorder.Header().Get(RouterAttemptsHeader))
}

func TestRouterUpstreamAccountCostRatesFollowSelectedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	cheap, mid, zero := 0.45, 0.8, 0.0
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 1118, RateMultiplier: &cheap})
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 1119, RateMultiplier: &mid})
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 7, RateMultiplier: &zero})
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 1120})

	var rates map[string]float64
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(RouterAccountRatesHeader)), &rates))
	require.Equal(t, map[string]float64{"1118": 0.45, "1119": 0.8, "7": 0}, rates)
}

func TestRouterUpstreamAccountCostRatesRejectInvalidOrPublicCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	negative, nan := -1.0, math.NaN()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 1, RateMultiplier: &negative})
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 2, RateMultiplier: &nan})
	RecordRouterUpstreamAccountCostRate(c, nil)
	require.Empty(t, recorder.Header().Get(RouterAccountRatesHeader))

	cheap := 0.45
	public := httptest.NewRecorder()
	pc, _ := gin.CreateTestContext(public)
	pc.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	RecordRouterUpstreamAccountCostRate(pc, &Account{ID: 1118, RateMultiplier: &cheap})
	require.Empty(t, public.Header().Get(RouterAccountRatesHeader))
}

func TestRouterUpstreamAccountCostRatesNotWrittenAfterResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	c.String(http.StatusOK, "committed")
	cheap := 0.45
	RecordRouterUpstreamAccountCostRate(c, &Account{ID: 1118, RateMultiplier: &cheap})
	require.Empty(t, recorder.Header().Get(RouterAccountRatesHeader))
}

func TestRouterUpstreamAccountSellRatesFollowSelectedAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:generateContent", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)

	// 1124 serves first, fails over to 1132; both priced accounts stay in the map.
	low, premium, free := 0.4, 4.727, 0.0
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 1124, SellRateMultiplier: &low})
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 1132, SellRateMultiplier: &premium})
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 7, SellRateMultiplier: &free})
	// Unpriced account: omitted so Router keeps the group rate.
	cost := 0.45
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 1118, RateMultiplier: &cost})

	var rates map[string]float64
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(RouterAccountSellRatesHeader)), &rates))
	require.Equal(t, map[string]float64{"1124": 0.4, "1132": 4.727, "7": 0}, rates)
	// Sell and cost headers are independent.
	require.Empty(t, recorder.Header().Get(RouterAccountRatesHeader))
}

func TestRouterUpstreamAccountSellRatesRejectInvalidOrPublicCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	negative, nan, inf := -1.0, math.NaN(), math.Inf(1)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 1, SellRateMultiplier: &negative})
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 2, SellRateMultiplier: &nan})
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 3, SellRateMultiplier: &inf})
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 4})
	RecordRouterUpstreamAccountSellRate(c, nil)
	require.Empty(t, recorder.Header().Get(RouterAccountSellRatesHeader))

	premium := 1.6
	public := httptest.NewRecorder()
	pc, _ := gin.CreateTestContext(public)
	pc.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	RecordRouterUpstreamAccountSellRate(pc, &Account{ID: 1119, SellRateMultiplier: &premium})
	require.Empty(t, public.Header().Get(RouterAccountSellRatesHeader))
}

func TestRouterUpstreamAccountSellRatesNotWrittenAfterResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(RouterContractHeader, RouterContractV2)
	c.String(http.StatusOK, "committed")
	premium := 1.6
	RecordRouterUpstreamAccountSellRate(c, &Account{ID: 1119, SellRateMultiplier: &premium})
	require.Empty(t, recorder.Header().Get(RouterAccountSellRatesHeader))
}
