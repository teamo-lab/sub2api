//go:build unit

package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestGeminiModelNotSupported404(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"rootflow", 404, `{"error":{"message":"所请求的模型暂不受支持，请更换模型后重试 (request id: test)"}}`, true},
		{"google", 404, `{"error":{"code":404,"message":"models/gemini-4-carbon is not found for API version v1beta, or is not supported for generateContent"}}`, true},
		{"unsupported_code", 404, `{"error":{"code":"model_not_supported"}}`, true},
		{"unsupported_plain", 404, `model not supported`, true},
		{"code", 404, `{"error":{"code":"model_not_found"}}`, true},
		{"wrapped", 404, `{"response":{"error":{"message":"unknown model gemini-4-carbon"}}}`, true},
		{"plain", 404, `Model gemini-4-carbon does not exist`, true},
		{"route", 404, `{"error":{"message":"Cannot POST /v1beta/models/gemini-4-carbon:generateContent"}}`, false},
		{"generic", 404, `404 page not found`, false},
		{"echoed_prompt", 404, `{"error":{"message":"route not found"},"request":{"message":"model not found"}}`, false},
		{"content_refusal", 400, `{"error":{"message":"请求内容不符合使用规范，已被拒绝"}}`, false},
		{"400_model", 400, `{"error":{"code":"model_not_found"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isGeminiModelNotSupported404(tc.status, []byte(tc.body)))
		})
	}
}

func TestGeminiCapability404ForwardPaths(t *testing.T) {
	for _, path := range []string{"native", "chat", "messages"} {
		for _, status := range []int{404, 400} {
			t.Run(path+http.StatusText(status), func(t *testing.T) {
				svc, upstream := newGeminiSkippedWriteService(status, `{"error":{"message":"所请求的模型暂不受支持，请更换模型后重试"}}`)
				account := geminiPoolModeAPIKeyAccount()
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(404)}
				c, rec := newGeminiNativeTestContext(t)
				c.Request.Header.Set(RouterContractHeader, RouterContractV2)
				BeginRouterUpstreamAttempts(c)
				RecordRouterUpstreamAccount(c, account.ID)
				body := []byte(`{"model":"gemini-4-carbon","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
				var err error
				switch path {
				case "native":
					_, err = svc.ForwardNative(context.Background(), c, account, "gemini-4-carbon", "generateContent", false, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
				case "chat":
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body)
				case "messages":
					_, err = svc.Forward(context.Background(), c, account, body)
				}
				require.Error(t, err)
				require.Equal(t, 1, upstream.calls)
				var failover *UpstreamFailoverError
				if status == 404 {
					require.ErrorAs(t, err, &failover)
					require.Equal(t, 404, failover.StatusCode)
					require.False(t, failover.RetryableOnSameAccount)
					require.False(t, c.Writer.Written())
					require.Zero(t, rec.Body.Len())
					require.Contains(t, rec.Header().Get(RouterAttemptsHeader), `"error_type":"model_not_supported"`)
				} else {
					require.NotErrorAs(t, err, &failover)
					require.Equal(t, 400, rec.Code)
				}
			})
		}
	}
}
