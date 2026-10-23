package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func bodyCapTestAccount(extra map[string]any) *Account {
	merged := map[string]any{"openai_responses_supported": true}
	for k, v := range extra {
		merged[k] = v
	}
	return &Account{
		ID:          9101,
		Name:        "zero",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.test"},
		Extra:       merged,
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestOpenAIMaxRequestBodyBytes_ParsesExtra(t *testing.T) {
	require.Equal(t, int64(0), (*Account)(nil).OpenAIMaxRequestBodyBytes())
	require.Equal(t, int64(0), bodyCapTestAccount(nil).OpenAIMaxRequestBodyBytes())
	require.Equal(t, int64(0), bodyCapTestAccount(map[string]any{AccountExtraMaxRequestBodyBytes: float64(-1)}).OpenAIMaxRequestBodyBytes())
	require.Equal(t, int64(33554432), bodyCapTestAccount(map[string]any{AccountExtraMaxRequestBodyBytes: float64(33554432)}).OpenAIMaxRequestBodyBytes())
	require.Equal(t, int64(1024), bodyCapTestAccount(map[string]any{AccountExtraMaxRequestBodyBytes: "1024"}).OpenAIMaxRequestBodyBytes())
}

func TestOpenAIRequestBodyCap_EligibilitySkipsAccountOnlyWhenBodyExceedsLimit(t *testing.T) {
	capped := bodyCapTestAccount(map[string]any{AccountExtraMaxRequestBodyBytes: float64(32 << 20)})
	uncapped := bodyCapTestAccount(nil)
	big := WithOpenAIInboundRequestBodyBytes(context.Background(), 40<<20)
	small := WithOpenAIInboundRequestBodyBytes(context.Background(), 1<<20)
	exact := WithOpenAIInboundRequestBodyBytes(context.Background(), 32<<20)

	require.Equal(t, openAIAccountRequestBodyTooLargeReason,
		openAICompatibleAccountEligibilityFailureReasonBeforeProfit(big, capped, PlatformOpenAI, "", false, OpenAIEndpointCapabilityResponses))
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(small, capped, PlatformOpenAI, "", false, OpenAIEndpointCapabilityResponses))
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(exact, capped, PlatformOpenAI, "", false, OpenAIEndpointCapabilityResponses))
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(big, uncapped, PlatformOpenAI, "", false, OpenAIEndpointCapabilityResponses))
	require.Empty(t, openAICompatibleAccountEligibilityFailureReasonBeforeProfit(context.Background(), capped, PlatformOpenAI, "", false, OpenAIEndpointCapabilityResponses),
		"requests without a recorded body size keep the legacy behavior")

	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{}}
	compatible, reason := scheduler.isAccountRequestCompatibleReason(big, capped, OpenAIAccountScheduleRequest{})
	require.False(t, compatible)
	require.Equal(t, openAIAccountRequestBodyTooLargeReason, reason)
	compatible, _ = scheduler.isAccountRequestCompatibleReason(big, uncapped, OpenAIAccountScheduleRequest{})
	require.True(t, compatible)
}

func TestIsNewAPIBodyParseLimitRejection(t *testing.T) {
	const msg = "Invalid task protocol request (request id: 202610230757405509340538268d9d6deLd4NtO)"
	require.True(t, isNewAPIBodyParseLimitRejection(http.StatusBadRequest, msg, 33<<20))
	require.False(t, isNewAPIBodyParseLimitRejection(http.StatusBadRequest, msg, 1<<20),
		"new-api returns the same text for small malformed JSON; that is a client error, not a size limit")
	require.False(t, isNewAPIBodyParseLimitRejection(http.StatusBadGateway, msg, 33<<20))
	require.False(t, isNewAPIBodyParseLimitRejection(http.StatusBadRequest, "Model name not specified", 33<<20))
}

func newAPIBodyLimitUpstream() (*httpUpstreamRecorder, *passthroughCloseTrackingReadCloser) {
	const upstreamBody = `{"error":{"code":"","message":"Invalid task protocol request (request id: 202610230757405509340538268d9d6deLd4NtO)","type":"new_api_error"}}`
	body := &passthroughCloseTrackingReadCloser{Reader: strings.NewReader(upstreamBody)}
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header: http.Header{
			"Content-Type":        []string{"application/json"},
			"X-Oneapi-Request-Id": []string{"202610230757405509340538268d9d6deLd4NtO"},
		},
		Body: body,
	}}, body
}

func TestOpenAIRequestBodyCap_NewAPIInvalidTaskProtocolOnLargeBodySwitchesAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	upstream, body := newAPIBodyLimitUpstream()
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: upstream,
	}
	ctx := WithOpenAIInboundRequestBodyBytes(context.Background(), 40<<20)

	result, err := svc.Forward(ctx, c, bodyCapTestAccount(nil), []byte(`{"model":"gpt-6.1-sol","stream":true,"input":"hello"}`))

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.IsOpenAIRequestBodyTooLarge())
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, http.StatusRequestEntityTooLarge, failoverErr.ClientStatusCode)
	require.False(t, c.Writer.Written(), "account failover must happen before downstream output is committed")
	require.True(t, body.closed)
}

func TestOpenAIRequestBodyCap_NewAPIInvalidTaskProtocolOnSmallBodyIsPassedThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	upstream, _ := newAPIBodyLimitUpstream()
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: upstream,
	}
	ctx := WithOpenAIInboundRequestBodyBytes(context.Background(), 2<<20)

	result, err := svc.Forward(ctx, c, bodyCapTestAccount(nil), []byte(`{"model":"gpt-6.1-sol","stream":true,"input":"hello"}`))

	require.Nil(t, result)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, c.Writer.Written())
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestOpenAIRequestBodyCap_ChatPathNewAPIInvalidTaskProtocolOnLargeBodySwitchesAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	upstream, _ := newAPIBodyLimitUpstream()
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	respBody, upstreamMsg := svc.readOpenAIUpstreamError(upstream.resp)

	large := WithOpenAIInboundRequestBodyBytes(context.Background(), 40<<20)
	failoverErr := svc.failoverOpenAIUpstreamHTTPError(large, c, bodyCapTestAccount(nil), upstream.resp, respBody, upstreamMsg, "gpt-6.1-sol")
	require.NotNil(t, failoverErr)
	require.True(t, failoverErr.IsOpenAIRequestBodyTooLarge())

	small := WithOpenAIInboundRequestBodyBytes(context.Background(), 1<<20)
	require.Nil(t, svc.failoverOpenAIUpstreamHTTPError(small, c, bodyCapTestAccount(nil), upstream.resp, respBody, upstreamMsg, "gpt-6.1-sol"))
}
