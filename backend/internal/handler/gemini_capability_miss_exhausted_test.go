//go:build unit

package handler

import (
	"context"
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

// exhaustThroughFailoverState 按顺序把每个账号的失败喂给 FailoverState，返回耗尽时的 LastFailoverErr。
func exhaustThroughFailoverState(t *testing.T, errs ...*service.UpstreamFailoverError) *service.UpstreamFailoverError {
	t.Helper()
	fs := NewFailoverState(len(errs)-1, false)
	for i, fe := range errs {
		action := fs.HandleFailoverError(context.Background(), nil, int64(i+1), service.PlatformGemini, 0, fe)
		if i < len(errs)-1 {
			require.Equal(t, FailoverContinue, action)
		} else {
			require.Equal(t, FailoverExhausted, action)
		}
	}
	return fs.LastFailoverErr
}

func upstream503Err() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusServiceUnavailable,
		ResponseBody: []byte(`{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`),
	}
}

func TestGeminiFailoverExhausted_AllCapabilityMissKeepsUpstream4xx(t *testing.T) {
	last := exhaustThroughFailoverState(t, capabilityMissErr(), capabilityMissErr())
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, last)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, capabilityMissBody, rec.Body.String())
}

func TestGeminiFailoverExhausted_503ThenCapabilityMissMapsToUpstreamFailure(t *testing.T) {
	// 账号 A 503、账号 B 不支持：不能告诉客户端「所有账号都不支持」，要按上游故障返回，让客户端可以重试。
	last := exhaustThroughFailoverState(t, upstream503Err(), capabilityMissErr())
	require.Equal(t, service.GeminiCapabilityMissMixedReason, last.Reason)
	require.False(t, service.IsGeminiCapabilityMissExhausted(last))

	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, last)
	require.Equal(t, http.StatusBadGateway, rec.Code)

	c, rec = newExhaustedTestContext("/v1/chat/completions")
	(&GatewayHandler{}).handleCCFailoverExhausted(c, last, false)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "invalid_request_error")

	c, rec = newExhaustedTestContext("/v1/messages")
	(&GatewayHandler{}).handleFailoverExhausted(c, last, service.PlatformGemini, false)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "invalid_request_error")
}

func TestGeminiFailoverExhausted_CapabilityMissThen503MapsToUpstreamFailure(t *testing.T) {
	last := exhaustThroughFailoverState(t, capabilityMissErr(), upstream503Err())
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, last)
	require.NotEqual(t, http.StatusBadRequest, rec.Code)
}

func TestFailoverState_MixedCapabilityMissDoesNotMutateOriginalError(t *testing.T) {
	original := capabilityMissErr()
	last := exhaustThroughFailoverState(t, upstream503Err(), original)
	require.Equal(t, service.GeminiCapabilityMissReason, original.Reason)
	require.NotSame(t, original, last)
}
