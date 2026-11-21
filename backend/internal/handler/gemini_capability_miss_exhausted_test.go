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

// exhaustThroughFailoverState 按顺序把每个账号的失败喂给 FailoverState（每次都应继续换号），
// 然后模拟候选池里没有账号了（选号耗尽），返回此时的 LastFailoverErr。
func exhaustThroughFailoverState(t *testing.T, maxSwitches int, errs ...*service.UpstreamFailoverError) *service.UpstreamFailoverError {
	t.Helper()
	return exhaustWithPoolCheck(t, maxSwitches, func(context.Context, map[int64]struct{}) bool { return true }, errs...)
}

// exhaustWithPoolCheck 同上，但用给定的「全池都试过」证明函数（nil 表示没有设置）。
func exhaustWithPoolCheck(t *testing.T, maxSwitches int, check func(context.Context, map[int64]struct{}) bool, errs ...*service.UpstreamFailoverError) *service.UpstreamFailoverError {
	t.Helper()
	fs := NewFailoverState(maxSwitches, false)
	if check != nil {
		fs.SetCapabilityMissPoolCheck(check)
	}
	for i, fe := range errs {
		action := fs.HandleFailoverError(context.Background(), nil, int64(i+1), service.PlatformGemini, 0, fe)
		require.Equal(t, FailoverContinue, action, "failure %d should switch to the next account", i+1)
	}
	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(context.Background()))
	return fs.LastFailoverErr
}

func upstream500Err() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusInternalServerError,
		ResponseBody: []byte(`{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`),
	}
}

func requireMappedToUpstreamFailure(t *testing.T, last *service.UpstreamFailoverError) {
	t.Helper()
	require.True(t, service.IsGeminiCapabilityMissInconclusive(last))
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

func TestGeminiFailoverExhausted_AllCapabilityMissKeepsUpstream4xx(t *testing.T) {
	// 6 个账号都不支持（超过默认换号上限 3）：capability_miss 不占普通换号预算，
	// 一直换到候选池没有账号，才证明「所有账号都不支持」，透传上游 4xx。
	errs := make([]*service.UpstreamFailoverError, 6)
	for i := range errs {
		errs[i] = capabilityMissErr()
	}
	last := exhaustThroughFailoverState(t, 3, errs...)
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, last)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, capabilityMissBody, rec.Body.String())
}

func TestGeminiFailoverExhausted_503ThenCapabilityMissMapsToUpstreamFailure(t *testing.T) {
	// 账号 A 500、账号 B 不支持：不能告诉客户端「所有账号都不支持」，要按上游故障返回，让客户端可以重试。
	last := exhaustThroughFailoverState(t, 3, upstream500Err(), capabilityMissErr())
	require.Equal(t, service.GeminiCapabilityMissMixedReason, last.Reason)
	requireMappedToUpstreamFailure(t, last)
}

func TestGeminiFailoverExhausted_CapabilityMissThen503MapsToUpstreamFailure(t *testing.T) {
	last := exhaustThroughFailoverState(t, 3, capabilityMissErr(), upstream500Err())
	c, rec := newExhaustedTestContext("/v1beta/models/gemini-4-carbon:generateContent")
	(&GatewayHandler{}).handleGeminiFailoverExhausted(c, last)
	require.NotEqual(t, http.StatusBadRequest, rec.Code)
}

func TestFailoverState_MixedCapabilityMissDoesNotMutateOriginalError(t *testing.T) {
	original := capabilityMissErr()
	last := exhaustThroughFailoverState(t, 3, upstream500Err(), original)
	require.Equal(t, service.GeminiCapabilityMissReason, original.Reason)
	require.NotSame(t, original, last)
}

func TestFailoverState_CapabilityMissSwitchLimitIsNotProof(t *testing.T) {
	// 能力类换号次数也用完了，但池里可能还有没试过的账号：不能透传 4xx，按上游故障（可重试）返回。
	fs := NewFailoverState(3, false)
	var action FailoverAction
	for i := 0; i <= maxGeminiCapabilityMissSwitches; i++ {
		action = fs.HandleFailoverError(context.Background(), nil, int64(i+1), service.PlatformGemini, 0, capabilityMissErr())
		if i < maxGeminiCapabilityMissSwitches {
			require.Equal(t, FailoverContinue, action)
		}
	}
	require.Equal(t, FailoverExhausted, action)
	require.Equal(t, service.GeminiCapabilityMissSwitchLimitReason, fs.LastFailoverErr.Reason)
	require.Zero(t, fs.SwitchCount, "capability_miss switches do not use the normal switch budget")
	requireMappedToUpstreamFailure(t, fs.LastFailoverErr)
}

func TestFailoverState_ZeroMaxSwitchesCapabilityMissIsNotProof(t *testing.T) {
	// 配置成不换号时只试了一个账号，同样不能证明所有账号都不支持。
	fs := NewFailoverState(0, false)
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), nil, 1, service.PlatformGemini, 0, capabilityMissErr()))
	requireMappedToUpstreamFailure(t, fs.LastFailoverErr)
}

func TestFailoverState_NormalSwitchBudgetUnchangedAfterCapabilityMisses(t *testing.T) {
	// 能力类换号不挤占普通换号预算：之后的 500 仍有完整的 3 次换号。
	fs := NewFailoverState(3, false)
	for i := 0; i < 2; i++ {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), nil, int64(i+1), service.PlatformGemini, 0, capabilityMissErr()))
	}
	for i := 0; i < 3; i++ {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), nil, int64(10+i), service.PlatformGemini, 0, upstream500Err()))
	}
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), nil, 20, service.PlatformGemini, 0, upstream500Err()))
}

func TestFailoverState_SelectionExhaustedWithUntriedAccountIsNotProof(t *testing.T) {
	// A 不支持；B 还在池里但暂时限流/额度不够，选号失败。B 没验证过：不能透传 4xx。
	var gotTried map[int64]struct{}
	last := exhaustWithPoolCheck(t, 3, func(_ context.Context, tried map[int64]struct{}) bool {
		gotTried = tried
		return false
	}, capabilityMissErr())
	require.Contains(t, gotTried, int64(1))
	require.Equal(t, service.GeminiCapabilityMissUnverifiedReason, last.Reason)
	requireMappedToUpstreamFailure(t, last)
}

func TestFailoverState_SelectionExhaustedWithoutPoolCheckIsNotProof(t *testing.T) {
	last := exhaustWithPoolCheck(t, 3, nil, capabilityMissErr(), capabilityMissErr())
	requireMappedToUpstreamFailure(t, last)
}
