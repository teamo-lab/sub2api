package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 线上样本（Router 渠道 10011 / 43 账号 23、28、30、31、32）：账号套餐不含
// gpt-5.5，上游以 HTTP 200 开流、发完 Codex 元数据后才在流内报 model_not_found。
// 语义状态被归一成 502，因此走不到 HTTP 路径的账号+模型冷却，同一个模型被反复
// 派给同一个没权限的账号，中位 45 秒、最长 259 秒才失败。
func TestIsOpenAIStreamModelNotFoundEvent(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			"response.failed 结构化帧",
			`{"type":"response.failed","response":{"status":"failed","error":{"code":"model_not_found","message":"The model ` + "`gpt-5.5`" + ` does not exist or you do not have access to it.","type":"invalid_request_error"}}}`,
			true,
		},
		{
			"裸 error 帧，code 在顶层",
			`{"type":"error","code":"model_not_found","message":"The model ` + "`gpt-5.5`" + ` does not exist or you do not have access to it.","sequence_number":0}`,
			true,
		},
		{
			"只有文案没有 code",
			`{"type":"error","message":"The model ` + "`gpt-5.5`" + ` does not exist or you do not have access to it."}`,
			true,
		},
		{
			"Codex 套餐门控",
			`{"type":"response.failed","response":{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}}`,
			true,
		},
		// 反例：容量类与上下文超限不得被当成模型缺失，否则会把健康账号的模型冷却掉。
		{
			"overloaded 不算",
			`{"type":"error","code":"server_error","message":"Our servers are currently overloaded. Please try again later."}`,
			false,
		},
		{
			"上下文超限不算",
			`{"type":"error","code":"context_too_large","message":"Your input exceeds the context window of this model."}`,
			false,
		},
		{"空载荷", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isOpenAIStreamModelNotFoundEvent([]byte(tc.payload)))
		})
	}
}

// 关键词表补 "does not exist" 后，HTTP 404 路径也能识别现行 OpenAI 文案。
func TestUpstreamModelNotFoundMatchesDoesNotExistWording(t *testing.T) {
	body := []byte(`{"error":{"message":"The model ` + "`gpt-5.5`" + ` does not exist or you do not have access to it.","code":"model_not_found"}}`)
	require.True(t, isUpstreamModelNotFoundError(http.StatusNotFound, body))
	// 正文不含 "model" 时不判定，避免把无关 404 误伤。
	require.False(t, isUpstreamModelNotFoundError(http.StatusNotFound, []byte(`{"error":{"message":"resource does not exist"}}`)))
	// 非 404 不走这条判定。
	require.False(t, isUpstreamModelNotFoundError(http.StatusBadGateway, body))
}
