//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const capabilityMissBody = `{"error":{"code":400,"message":"Requests ending with a model turn are not supported.","status":"INVALID_ARGUMENT"}}`

func capabilityMissErr() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusBadRequest,
		ResponseBody: []byte(capabilityMissBody),
		Reason:       service.GeminiCapabilityMissReason,
	}
}

func newExhaustedTestContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, rec
}

func TestGeminiFailoverExhausted_CapabilityMissKeepsUpstream4xx(t *testing.T) {
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, capabilityMissErr())
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, capabilityMissBody, rec.Body.String())
}

func TestGeminiFailoverExhausted_OtherErrorsStillMapped(t *testing.T) {
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:   http.StatusPaymentRequired,
		ResponseBody: []byte(`{"error":{"code":402,"message":"Your prepayment credits are depleted."}}`),
		Reason:       service.GeminiPaymentRequiredReason,
	})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "prepayment")
}

func TestCCFailoverExhausted_CapabilityMissKeepsUpstream4xx(t *testing.T) {
	c, rec := newExhaustedTestContext("/v1/chat/completions")
	(&GatewayHandler{}).handleCCFailoverExhausted(c, capabilityMissErr(), false)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Requests ending with a model turn are not supported.")
	require.Contains(t, rec.Body.String(), "invalid_request_error")
}

func TestMessagesFailoverExhausted_CapabilityMissKeepsUpstream4xx(t *testing.T) {
	c, rec := newExhaustedTestContext("/v1/messages")
	(&GatewayHandler{}).handleFailoverExhausted(c, capabilityMissErr(), service.PlatformGemini, false)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Requests ending with a model turn are not supported.")
}
