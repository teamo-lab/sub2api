//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// hoyorouter 的内容过滤拒绝原文（11-24 生产 ops_error_logs）。
const hoyoContentRejectBody = `{"error":{"code":400,"message":"Request was rejected by upstream policy; modify the prompt or context and try again. (ref: req_5edb238c8134)","status":"INVALID_ARGUMENT"}}`

func withContentRejectFailover(acc *Account) *Account {
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra[AccountContentRejectFailoverExtraKey] = true
	return acc
}

func TestClassifyGeminiAccountUpstreamError_ContentReject(t *testing.T) {
	flagged := withContentRejectFailover(geminiNonPoolAPIKeyAccount())
	plain := geminiNonPoolAPIKeyAccount()
	body := []byte(hoyoContentRejectBody)

	require.Equal(t, geminiErrorCapabilityMiss, classifyGeminiAccountUpstreamError(flagged, http.StatusBadRequest, body))
	require.Equal(t, geminiErrorDefault, classifyGeminiAccountUpstreamError(plain, http.StatusBadRequest, body), "only opted-in accounts fail over")
	require.Equal(t, geminiErrorDefault, classifyGeminiAccountUpstreamError(nil, http.StatusBadRequest, body))
	// 打开开关的号，别的 400（请求本身的错）照旧透传
	require.Equal(t, geminiErrorDefault, classifyGeminiAccountUpstreamError(flagged, http.StatusBadRequest,
		[]byte(`{"error":{"code":400,"message":"Invalid value at 'generation_config.temperature'"}}`)))
	// 同样文案但不是 400，不走这条
	require.Equal(t, geminiErrorDefault, classifyGeminiAccountUpstreamError(flagged, http.StatusForbidden, body))
	// 字符串 "true" 不算打开（extra 里要存 JSON 布尔）
	str := geminiNonPoolAPIKeyAccount()
	str.Extra = map[string]any{AccountContentRejectFailoverExtraKey: "true"}
	require.Equal(t, geminiErrorDefault, classifyGeminiAccountUpstreamError(str, http.StatusBadRequest, body))
	// 原有分类不受影响
	require.Equal(t, geminiErrorCapabilityMiss, classifyGeminiAccountUpstreamError(plain, http.StatusNotFound, []byte(`{}`)))
}

func TestGeminiForwardNative_ContentRejectFailsOverOnOptedInAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *Account
	}{
		{"pool mode", withContentRejectFailover(geminiPoolModeAPIKeyAccount())},
		{"non pool", withContentRejectFailover(geminiNonPoolAPIKeyAccount())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, stub := newGeminiSkippedWriteService(http.StatusBadRequest, hoyoContentRejectBody)
			c, _ := newGeminiNativeTestContext(t)
			_, err := svc.ForwardNative(context.Background(), c, tc.acc, "gemini-3.8-flash", "generateContent", false, []byte(geminiUsageTestRequest))
			fe := requireGeminiImmediateFailover(t, c, err, http.StatusBadRequest, GeminiCapabilityMissReason)
			require.Equal(t, 1, stub.calls, "no same-account retry")
			// 所有号都拒时，客户端拿到的是这条原始 400
			require.JSONEq(t, hoyoContentRejectBody, string(fe.ResponseBody))
		})
	}
}

func TestGeminiForwardNative_ContentRejectPassesThroughWithoutOptIn(t *testing.T) {
	svc, _ := newGeminiSkippedWriteService(http.StatusBadRequest, hoyoContentRejectBody)
	c, rec := newGeminiNativeTestContext(t)
	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-3.8-flash", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
