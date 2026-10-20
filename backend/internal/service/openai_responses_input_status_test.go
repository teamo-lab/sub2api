package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 结构取自线上 Codex Desktop 回放（内容已替换）：web_search_call 不带 status，
// 严格上游返回 400 "Missing required parameter: 'input[N].status'"。
const openAIResponsesStatusReplayBody = `{"model":"gpt-5.6-sol","store":false,"stream":false,"input":[` +
	`{"type":"message","role":"developer","content":"rules"},` +
	`{"type":"message","role":"user","content":"search it"},` +
	`{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"q"}},` +
	`{"type":"web_search_call","action":{"type":"open_page","url":"https://example.com"}},` +
	`{"type":"web_search_call","id":"ws_3","status":"failed","action":{"type":"search","query":"q"}},` +
	`{"type":"web_search_call","id":"ws_4","status":null,"action":{"type":"search","query":"q"}},` +
	`{"type":"image_generation_call","id":"ig_1","result":"b64","revised_prompt":"p"},` +
	`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","arguments":"{}"},` +
	`{"type":"function_call_output","call_id":"call_1","output":"ok"},` +
	`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_2","name":"apply_patch","input":"patch"},` +
	`{"type":"custom_tool_call_output","call_id":"call_2","output":"done"},` +
	`{"type":"compaction","encrypted_content":"cipher"},` +
	`{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"answer"}]},` +
	`"plain string item"` +
	`]}`

func TestBackfillOpenAIResponsesInputItemStatus(t *testing.T) {
	body := []byte(openAIResponsesStatusReplayBody)

	normalized, changed, err := backfillOpenAIResponsesInputItemStatus(body)
	require.NoError(t, err)
	require.True(t, changed)

	require.Equal(t, int64(14), gjson.GetBytes(normalized, "input.#").Int())
	// 必填类型缺 status / status 为 null 时补 completed。
	for _, index := range []string{"2", "3", "5", "6"} {
		require.Equal(t, "completed", gjson.GetBytes(normalized, "input."+index+".status").String(), index)
	}
	// 已有的 status 不覆盖。
	require.Equal(t, "failed", gjson.GetBytes(normalized, "input.4.status").String())
	// status 可选或规范里没有 status 的类型不补。
	for _, index := range []string{"0", "1", "7", "8", "9", "10", "11", "12"} {
		require.False(t, gjson.GetBytes(normalized, "input."+index+".status").Exists(), index)
	}
	// 其它字段和顺序原样保留。
	require.Equal(t, "ws_1", gjson.GetBytes(normalized, "input.2.id").String())
	require.False(t, gjson.GetBytes(normalized, "input.3.id").Exists())
	require.Equal(t, "https://example.com", gjson.GetBytes(normalized, "input.3.action.url").String())
	require.Equal(t, "b64", gjson.GetBytes(normalized, "input.6.result").String())
	require.Equal(t, "plain string item", gjson.GetBytes(normalized, "input.13").String())
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(normalized, "model").String())
	require.False(t, gjson.GetBytes(normalized, "store").Bool())

	again, changedAgain, err := backfillOpenAIResponsesInputItemStatus(normalized)
	require.NoError(t, err)
	require.False(t, changedAgain)
	require.Equal(t, string(normalized), string(again))
}

func TestBackfillOpenAIResponsesInputItemStatusNoop(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5","input":"hi"}`,
		`{"model":"gpt-5"}`,
		`{"model":"gpt-5","input":[{"type":"function_call","call_id":"c","name":"n","arguments":"{}"},{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}}]}`,
	} {
		normalized, changed, err := backfillOpenAIResponsesInputItemStatus([]byte(body))
		require.NoError(t, err)
		require.False(t, changed, body)
		require.Equal(t, body, string(normalized))
	}
}

func TestAccountIsOpenAIResponsesInputStatusBackfillEnabled(t *testing.T) {
	enabled := map[string]any{openAIResponsesInputStatusBackfillExtraKey: true}
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: enabled}).IsOpenAIResponsesInputStatusBackfillEnabled())
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).IsOpenAIResponsesInputStatusBackfillEnabled())
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openAIResponsesInputStatusBackfillExtraKey: "true"}}).IsOpenAIResponsesInputStatusBackfillEnabled())
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: enabled}).IsOpenAIResponsesInputStatusBackfillEnabled())
	require.False(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: enabled}).IsOpenAIResponsesInputStatusBackfillEnabled())
	var nilAccount *Account
	require.False(t, nilAccount.IsOpenAIResponsesInputStatusBackfillEnabled())
}

func TestOpenAIGatewayService_Forward_APIKeyBackfillsReplayStatusOnlyWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false

	for _, tt := range []struct {
		name     string
		extra    map[string]any
		backfill bool
	}{
		{name: "enabled", extra: map[string]any{openAIResponsesInputStatusBackfillExtraKey: true}, backfill: true},
		{name: "default off", extra: map[string]any{}, backfill: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				ID:          1094,
				Name:        "strict-responses-apikey",
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{
					"api_key":  "sk-test",
					"base_url": "https://example.com",
				},
				Extra: tt.extra,
			}
			upstream := &httpUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":2}}`)),
				},
			}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

			result, err := svc.Forward(context.Background(), c, account, []byte(openAIResponsesStatusReplayBody))
			require.NoError(t, err)
			require.NotNil(t, result)

			webSearch := gjson.GetBytes(upstream.lastBody, `input.#(type=="web_search_call")#`).Array()
			require.Len(t, webSearch, 4)
			imageGeneration := gjson.GetBytes(upstream.lastBody, `input.#(type=="image_generation_call")`)
			require.True(t, imageGeneration.Exists())
			if tt.backfill {
				for _, item := range webSearch {
					require.True(t, item.Get("status").Exists(), item.Raw)
				}
				require.Equal(t, "failed", webSearch[2].Get("status").String())
				require.Equal(t, "completed", imageGeneration.Get("status").String())
			} else {
				require.False(t, webSearch[0].Get("status").Exists())
				require.False(t, webSearch[1].Get("status").Exists())
				require.False(t, imageGeneration.Get("status").Exists())
			}
			require.False(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call").status`).Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="custom_tool_call").status`).Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="compaction").status`).Exists())
		})
	}
}

func TestNormalizeOpenAIResponsesWebSocketCompatibilityBody_BackfillsReplayStatusWhenEnabled(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[` +
		`{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"q"}},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec","arguments":"{}"}]}`)

	enabled := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openAIResponsesInputStatusBackfillExtraKey: true}}
	normalized, changed, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, enabled, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "response.create", gjson.GetBytes(normalized, "type").String())
	require.Equal(t, "completed", gjson.GetBytes(normalized, "input.0.status").String())
	require.False(t, gjson.GetBytes(normalized, "input.1.status").Exists())

	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{openAIResponsesInputStatusBackfillExtraKey: true}},
	} {
		normalized, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, false)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(normalized, "input.0.status").Exists(), account.Type)
	}
}
