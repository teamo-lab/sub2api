package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Synthetic opaque fields: these tests check preservation, not cryptographic
// verification of a fabricated Anthropic signature.
const signedThinkingFixture = `{ "signature":"sig\/opaque\u003d", "type":"thinking", "thinking":"\u4f60\n<&>", "vendor_extra":{"n":9007199254740993} }`
const redactedThinkingFixture = `{ "data":"encrypted\/opaque\u003d", "type":"redacted_thinking", "vendor_extra":"keep" }`
const thinkingToolFixture = `{"type":"tool_use","id":"toolu_test","name":"noop","input":{"n":9007199254740993,"thinking":"ordinary tool argument"}}`

func TestThinkingHistoryPreservation_AllGenerationModes(t *testing.T) {
	for _, mode := range []string{"enabled", "adaptive", "disabled", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			setting := ""
			if mode != "omitted" {
				setting = fmt.Sprintf(`"thinking":{"type":%q},`, mode)
			}
			body := []byte(`{` + setting + `"messages":[{"role":"assistant","content":[` + signedThinkingFixture + `,` + redactedThinkingFixture + `,` + thinkingToolFixture + `]}],"metadata":{"n":9007199254740993}}`)
			for turn := 0; turn < 3; turn++ {
				out := FilterThinkingBlocks(body, "claude-opus-4-6")
				require.Equal(t, body, out, "a valid opaque history must not change on replay")
			}
		})
	}
}

func TestThinkingHistoryPreservation_CleanupDoesNotReserializeSiblings(t *testing.T) {
	body := []byte(`{"thinking":{"type":"adaptive"},"metadata":{"n":9007199254740993},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"unsigned"},` + signedThinkingFixture + `,` + redactedThinkingFixture + `,` + thinkingToolFixture + `]}]}`)
	out := FilterThinkingBlocks(body, "claude-opus-4-6")
	blocks := gjson.GetBytes(out, "messages.0.content").Array()
	require.Len(t, blocks, 3)
	require.Equal(t, signedThinkingFixture, blocks[0].Raw)
	require.Equal(t, redactedThinkingFixture, blocks[1].Raw)
	require.Equal(t, thinkingToolFixture, blocks[2].Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "metadata.n").Raw)
	require.Equal(t, out, FilterThinkingBlocks(out, "claude-opus-4-6"))
	require.Contains(t, string(body), `"unsigned"`, "input buffer must not be mutated")
}

func TestThinkingHistoryPreservation_RedactedUsesDataNotSignature(t *testing.T) {
	for _, malformed := range []string{
		`{"type":"redacted_thinking"}`,
		`{"type":"redacted_thinking","data":""}`,
		`{"type":"redacted_thinking","signature":"not-a-data-field"}`,
		`{"type":"redacted_thinking","data":123}`,
	} {
		body := []byte(`{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[` + redactedThinkingFixture + `,` + malformed + `]}]}`)
		out := FilterThinkingBlocks(body, "claude-opus-4-6")
		blocks := gjson.GetBytes(out, "messages.0.content").Array()
		require.Len(t, blocks, 1)
		require.Equal(t, redactedThinkingFixture, blocks[0].Raw)
	}
}

func TestThinkingHistoryPreservation_PreFiltersKeepOpaqueBytes(t *testing.T) {
	body := []byte(`{"thinking":{"type":"adaptive"},"messages":[{"role":"assistant","content":[` + signedThinkingFixture + `,` + redactedThinkingFixture + `,` + thinkingToolFixture + `]},{"role":"user","content":[{"type":"text","text":""},{"type":"tool_result","tool_use_id":"toolu_test","content":[{"type":"text","text":""},{"type":"tool_result","content":[{"type":"text","text":""},{"type":"text","text":"ok"}]}]}]}]}`)
	out := FilterThinkingBlocks(StripEmptyTextBlocks(body), "claude-opus-4-6")
	blocks := gjson.GetBytes(out, "messages.0.content").Array()
	require.Len(t, blocks, 3)
	require.Equal(t, signedThinkingFixture, blocks[0].Raw)
	require.Equal(t, redactedThinkingFixture, blocks[1].Raw)
	require.Equal(t, thinkingToolFixture, blocks[2].Raw)
	require.Len(t, gjson.GetBytes(out, "messages.1.content").Array(), 1)
	require.Equal(t, "ok", gjson.GetBytes(out, "messages.1.content.0.content.0.content.0.text").String())
	require.Equal(t, out, StripEmptyTextBlocks(out))
}

func TestThinkingHistoryPreservation_InvalidJSONAndForeignProtocols(t *testing.T) {
	for _, body := range []string{`{"messages":[`, `{"messages":[{"content":[{"type":"text","text":""}]}]`, `{"messages":null}`} {
		require.Equal(t, []byte(body), StripEmptyTextBlocks([]byte(body)))
		require.Equal(t, []byte(body), FilterThinkingBlocks([]byte(body), "claude-opus-4-6"))
	}
	for _, model := range []string{"deepseek-v4-pro", "kimi-k3", "glm-5.3", "unknown-model"} {
		body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"unsigned"},` + redactedThinkingFixture + `]}]}`)
		require.Equal(t, body, FilterThinkingBlocks(body, model))
		require.Equal(t, body, FilterThinkingBlocksForRetry(body, model))
	}
}

func TestThinkingHistoryPreservation_DeepContentFailsWithoutPartialRewrite(t *testing.T) {
	content := `[{"type":"text","text":""}]`
	for i := 0; i < 70; i++ {
		content = `[{"type":"tool_result","content":` + content + `}]`
	}
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":""},` + signedThinkingFixture + `]},{"role":"user","content":` + content + `}]}`)
	require.Equal(t, body, StripEmptyTextBlocks(body), "a failed traversal must not apply earlier partial edits")
}

func TestThinkingHistoryPreservation_ActualAPIKeyForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"adaptive", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			setting := ""
			if mode != "omitted" {
				setting = `"thinking":{"type":"adaptive"},`
			}
			body := []byte(`{` + setting + `"model":"claude-opus-4-6","stream":false,"max_tokens":128,"messages":[{"role":"assistant","content":[` + signedThinkingFixture + `,` + redactedThinkingFixture + `,` + thinkingToolFixture + `]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_test","content":[{"type":"text","text":""},{"type":"text","text":"ok"}]}]}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
			require.NoError(t, err)
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`)),
			}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra = nil // Exercise the normal pre-filter path used by 101 accounts 53/54.
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			_, err = svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			blocks := gjson.GetBytes(upstream.lastBody, "messages.0.content").Array()
			require.Len(t, blocks, 3)
			require.Equal(t, signedThinkingFixture, blocks[0].Raw)
			require.Equal(t, redactedThinkingFixture, blocks[1].Raw)
			require.Equal(t, thinkingToolFixture, blocks[2].Raw)
			require.Len(t, gjson.GetBytes(upstream.lastBody, "messages.1.content.0.content").Array(), 1)
			require.Equal(t, gjson.GetBytes(body, "thinking").Raw, gjson.GetBytes(upstream.lastBody, "thinking").Raw)
		})
	}
}

func BenchmarkThinkingHistoryPreservation_LargeCleanup(b *testing.B) {
	content := bytes.Repeat([]byte(`{"type":"text","text":""},`), 1000)
	body := append([]byte(`{"messages":[{"role":"assistant","content":[`), content...)
	body = append(body, []byte(redactedThinkingFixture+`]}]}`)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		StripEmptyTextBlocks(body)
	}
}
