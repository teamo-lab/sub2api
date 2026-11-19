package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetOpsSelectedServiceAccountPublishesAccountAndCostRate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:streamGenerateContent", nil)
	c.Request.Header.Set(service.RouterContractHeader, service.RouterContractV2)

	rate := 0.45
	setOpsSelectedServiceAccount(c, &service.Account{ID: 1118, Platform: service.PlatformGemini, RateMultiplier: &rate})
	setOpsSelectedServiceAccount(c, nil)

	var attempts []service.RouterUpstreamAttempt
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(service.RouterAttemptsHeader)), &attempts))
	require.Len(t, attempts, 1)
	require.Equal(t, int64(1118), attempts[0].AccountID)
	var rates map[string]float64
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(service.RouterAccountRatesHeader)), &rates))
	require.Equal(t, map[string]float64{"1118": 0.45}, rates)
	accountID, ok := c.Get(opsAccountIDKey)
	require.True(t, ok)
	require.Equal(t, int64(1118), accountID)
}

func TestSetOpsSelectedServiceAccountPublishesSellRateAcrossFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:streamGenerateContent", nil)
	c.Request.Header.Set(service.RouterContractHeader, service.RouterContractV2)

	low, premium, cost := 0.4, 4.727, 0.45
	setOpsSelectedServiceAccount(c, &service.Account{ID: 1124, Platform: service.PlatformGemini, SellRateMultiplier: &low})
	setOpsSelectedServiceAccount(c, &service.Account{ID: 1132, Platform: service.PlatformGemini, SellRateMultiplier: &premium, RateMultiplier: &cost})
	setOpsSelectedServiceAccount(c, &service.Account{ID: 1128, Platform: service.PlatformGemini})

	var attempts []service.RouterUpstreamAttempt
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(service.RouterAttemptsHeader)), &attempts))
	require.Len(t, attempts, 3)
	require.Equal(t, int64(1128), attempts[2].AccountID)
	var sell map[string]float64
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(service.RouterAccountSellRatesHeader)), &sell))
	require.Equal(t, map[string]float64{"1124": 0.4, "1132": 4.727}, sell)
	var costRates map[string]float64
	require.NoError(t, json.Unmarshal([]byte(recorder.Header().Get(service.RouterAccountRatesHeader)), &costRates))
	require.Equal(t, map[string]float64{"1132": 0.45}, costRates)
}

func TestSetOpsSelectedServiceAccountOmitsSellRateForPublicCallers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	premium := 1.6
	setOpsSelectedServiceAccount(c, &service.Account{ID: 1119, Platform: service.PlatformGemini, SellRateMultiplier: &premium})
	require.Empty(t, recorder.Header().Get(service.RouterAccountSellRatesHeader))
}
