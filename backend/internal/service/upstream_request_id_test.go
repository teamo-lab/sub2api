package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamRequestIDFromHeaders_UnconfiguredAccountRecordsNothing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Client-Request-ID", "sub2api-client")
	h.Set("X-Request-ID", "sub2api-local")
	h.Set("X-Oneapi-Request-Id", "oneapi-1")
	h.Set("Request-Id", "req_official")
	h.Set("xai-request-id", "xai-1")
	h.Set("x-goog-request-id", "goog-1")

	require.Equal(t, "", UpstreamRequestIDFromHeaders(nil, h))
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok} {
		require.Equal(t, "", UpstreamRequestIDFromHeaders(&Account{Platform: platform}, h), platform)
	}
	blank := &Account{Platform: PlatformOpenAI, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "   "}}
	require.Equal(t, "", UpstreamRequestIDFromHeaders(blank, h))
}

func TestUpstreamRequestIDFromHeaders_ReadsOnlyConfiguredHeader(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Extra:    map[string]any{AccountExtraUpstreamRequestIDHeader: " x-oneapi-request-id "},
	}
	h := http.Header{}
	h.Set("X-Request-ID", "passthrough-from-real-upstream")
	require.Equal(t, "", UpstreamRequestIDFromHeaders(account, h))

	h.Set("X-Oneapi-Request-Id", " oneapi-2 ")
	require.Equal(t, "oneapi-2", UpstreamRequestIDFromHeaders(account, h))
	require.Equal(t, "", UpstreamRequestIDFromHeaders(account, nil))

	official := &Account{Platform: PlatformAnthropic, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "request-id"}}
	only := http.Header{}
	only.Set("Request-Id", "req_official")
	require.Equal(t, "req_official", UpstreamRequestIDFromHeaders(official, only))
}

func TestUsageUpstreamRequestIDPtr(t *testing.T) {
	account := &Account{Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Request-ID"}}
	h := http.Header{}
	h.Set("X-Request-ID", strings.Repeat("a", 200))
	require.Nil(t, usageUpstreamRequestIDPtr(account, h, true))
	require.Nil(t, usageUpstreamRequestIDPtr(account, http.Header{}, false))
	require.Nil(t, usageUpstreamRequestIDPtr(nil, h, false))
	require.Nil(t, usageUpstreamRequestIDPtr(&Account{}, h, false))

	got := usageUpstreamRequestIDPtr(account, h, false)
	require.NotNil(t, got)
	require.Len(t, *got, maxUsageUpstreamRequestIDLen)
}

func TestValidateUpstreamRequestIDHeaderExtra(t *testing.T) {
	require.NoError(t, ValidateUpstreamRequestIDHeaderExtra(nil))
	require.NoError(t, ValidateUpstreamRequestIDHeaderExtra(map[string]any{}))

	blank := map[string]any{AccountExtraUpstreamRequestIDHeader: "   "}
	require.NoError(t, ValidateUpstreamRequestIDHeaderExtra(blank))
	_, present := blank[AccountExtraUpstreamRequestIDHeader]
	require.False(t, present, "blank header name must be removed")

	valid := map[string]any{AccountExtraUpstreamRequestIDHeader: " X-Oneapi-Request-Id "}
	require.NoError(t, ValidateUpstreamRequestIDHeaderExtra(valid))
	require.Equal(t, "X-Oneapi-Request-Id", valid[AccountExtraUpstreamRequestIDHeader])

	require.Error(t, ValidateUpstreamRequestIDHeaderExtra(map[string]any{AccountExtraUpstreamRequestIDHeader: 1}))
	require.Error(t, ValidateUpstreamRequestIDHeaderExtra(map[string]any{AccountExtraUpstreamRequestIDHeader: "X Request Id"}))
	require.Error(t, ValidateUpstreamRequestIDHeaderExtra(map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Request-Id:"}))
	require.Error(t, ValidateUpstreamRequestIDHeaderExtra(map[string]any{AccountExtraUpstreamRequestIDHeader: strings.Repeat("x", 65)}))
}

func TestOpsUpstreamRequestID_PrefersAccountHeaderThenFallsBack(t *testing.T) {
	sub2apiStation := &Account{Platform: PlatformOpenAI, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Client-Request-Id"}}
	h := http.Header{}
	h.Set("X-Request-Id", "chain-local-id")
	h.Set("X-Client-Request-Id", " station-business-id ")
	h.Set("CF-Ray", "8f1e2d3c4b5a6978-LAX")
	require.Equal(t, "station-business-id", opsUpstreamRequestID(sub2apiStation, h))
	require.Equal(t, "", opsUpstreamCFRay(sub2apiStation, h), "business ID present: CF-Ray is not needed")

	// 账户配了头但这次响应没带（例如上游报错时不回业务 ID）：退回 x-request-id，并保留 CF-Ray。
	noBusinessID := http.Header{}
	noBusinessID.Set("X-Request-Id", "chain-local-id")
	noBusinessID.Set("CF-Ray", "8f1e2d3c4b5a6978-LAX")
	require.Equal(t, "chain-local-id", opsUpstreamRequestID(sub2apiStation, noBusinessID))
	require.Equal(t, "8f1e2d3c4b5a6978-LAX", opsUpstreamCFRay(sub2apiStation, noBusinessID))

	// 未配置头的账户（HK 现状）：行为与改动前一致，读 x-request-id。
	require.Equal(t, "chain-local-id", opsUpstreamRequestID(&Account{Platform: PlatformOpenAI}, noBusinessID))
	require.Equal(t, "chain-local-id", opsUpstreamRequestID(nil, noBusinessID))

	// Grok 等额外的退回头。
	xai := http.Header{}
	xai.Set("xai-request-id", "xai-1")
	require.Equal(t, "xai-1", opsUpstreamRequestID(&Account{Platform: PlatformGrok}, xai, "xai-request-id"))
	require.Equal(t, "", opsUpstreamRequestID(&Account{Platform: PlatformGrok}, xai))

	require.Equal(t, "", opsUpstreamRequestID(sub2apiStation, nil))
	require.Equal(t, "", opsUpstreamCFRay(sub2apiStation, nil))
}

func TestOpsUpstreamCFRay_CloudflareEdgeErrorWithoutBusinessID(t *testing.T) {
	zero := &Account{Platform: PlatformOpenAI, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Oneapi-Request-Id"}}
	edge := http.Header{}
	edge.Set("CF-Ray", " 8f1e2d3c4b5a6978-SJC ")
	edge.Set("Server", "cloudflare")
	require.Equal(t, "", opsUpstreamRequestID(zero, edge))
	require.Equal(t, "8f1e2d3c4b5a6978-SJC", opsUpstreamCFRay(zero, edge))
}
