package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIBodyLimitFailoverExhausted_ReturnsRedactedJSON413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, bodyLimitFailoverTestError(), false)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	errBody, ok := envelope["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errBody["type"])
	require.Equal(t, "Request payload is too large", errBody["message"])
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

func TestOpenAIBodyLimitFailoverExhausted_ReturnsRedactedResponsesSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, bodyLimitFailoverTestError(), true)

	body := rec.Body.String()
	require.True(t, strings.HasPrefix(body, "event: response.failed\n"))
	require.Contains(t, body, `"code":"invalid_request"`)
	require.Contains(t, body, `"message":"Request payload is too large"`)
	require.NotContains(t, body, "must-not-leak")
}

func TestOpenAIBodyLimitFailoverExhausted_AnthropicReturns413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, bodyLimitFailoverTestError(), false)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), `"invalid_request_error"`)
	require.Contains(t, rec.Body.String(), "Request payload is too large")
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

func bodyLimitFailoverTestError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        http.StatusRequestEntityTooLarge,
		ResponseBody:      []byte(`{"error":{"message":"proxy limit secret=must-not-leak"}}`),
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.GatewayFailureReason("openai_request_body_too_large"),
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusRequestEntityTooLarge,
		ClientMessage:     "Request payload is too large",
	}
}

func TestOpenAIContextWindowFailoverExhausted_ReturnsResponsesSSEWithCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, contextWindowFailoverTestError(), true)

	body := rec.Body.String()
	require.True(t, strings.HasPrefix(body, "event: response.failed\n"))
	require.Contains(t, body, `"code":"context_length_exceeded"`)
	require.Contains(t, body, "exceeds the context window")
}

func TestOpenAIContextWindowFailoverExhausted_ReturnsAnthropicInvalidRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, contextWindowFailoverTestError(), false)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"invalid_request_error"`)
	require.Contains(t, rec.Body.String(), "exceeds the context window")
}

func contextWindowFailoverTestError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		ResponseBody:      []byte(`{"error":{"type":"upstream_error","message":"Your input exceeds the context window of this model.","code":"context_length_exceeded"}}`),
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.GatewayFailureReason("openai_context_window_exceeded"),
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusBadRequest,
		ClientMessage:     "Your input exceeds the context window of this model.",
	}
}
