package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 全部测试数据均为合成数据。密文模拟 Fernet 形态（gAAAA 开头），首尾片段用于
// 复现上游错误里的 "gAAA...aSJ9" 定位。
const (
	encTestForeignCipher = "gAAAAforeign-cipher-from-other-vendor-aSJ9"
	encTestOwnCipher     = "gAAAAown-cipher-from-this-account-7jA="
	encTestOtherCipher   = "gAAAAanother-foreign-cipher-oAOd"
)

func encTestFailedSSE(message string) string {
	return "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"" + message + "\"}}}\n\n"
}

const encTestFragmentMessage = "The encrypted content gAAA...aSJ9 could not be verified. Reason: Encrypted content could not be decrypted or parsed."

func encTestOKSSE(outputCipher string) string {
	out := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"
	if outputCipher != "" {
		out += "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_new\",\"summary\":[],\"encrypted_content\":\"" + outputCipher + "\"}}\n\n"
	}
	return out + "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
}

func encTestSSE(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func encTestJSON(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

// KA 形态：store=false，include 密文，reasoning 的 summary 为空，没有
// previous_response_id / compaction。
func encTestKABody(session string, ciphers ...string) []byte {
	items := ""
	for i, cipher := range ciphers {
		items += `{"type":"reasoning","id":"rs_` + string(rune('a'+i)) + `","summary":[],"encrypted_content":"` + cipher + `"},`
	}
	return []byte(`{"model":"gpt-6-astra","stream":true,"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"` + session + `","input":[` + items + `{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic followup"}]}],"nonce":9007199254740993}`)
}

func encTestAccount(id int64, mode string, extra map[string]any) *Account {
	e := map[string]any{"use_responses_api": true}
	if mode != "" {
		e[accountExtraOpenAIEncryptedRecoveryMode] = mode
	}
	for k, v := range extra {
		e[k] = v
	}
	return &Account{ID: id, Name: "synthetic", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3,
		Credentials: map[string]any{"api_key": "synthetic-test-only", "base_url": "https://zero.example.com"}, Extra: e}
}

type encTestHarness struct {
	store *memoryOpenAIEncryptedRecoveryStore
	svc   *OpenAIGatewayService
}

func newEncTestHarness(store *memoryOpenAIEncryptedRecoveryStore, responses ...*http.Response) (*encTestHarness, *httpUpstreamRecorder) {
	if store == nil {
		store = newMemoryOpenAIEncryptedRecoveryStore()
	}
	upstream := &httpUpstreamRecorder{responses: responses}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, openaiEncryptedRecoveryStore: store}
	return &encTestHarness{store: store, svc: svc}, upstream
}

func encTestContext(groupID int64) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
	return c, recorder
}

func encTestInputCiphers(body []byte) []string {
	var out []string
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if v := item.Get("encrypted_content"); v.Exists() {
			out = append(out, v.String())
		}
		return true
	})
	return out
}

func encTestMetric(store *memoryOpenAIEncryptedRecoveryStore, name string, accountID int64, mode string, labels ...string) int64 {
	field := name + ":acct=" + strconv.FormatInt(accountID, 10) + ":mode=" + mode
	for _, l := range labels {
		field += ":" + l
	}
	return store.metric(field)
}

func TestParseOpenAIEncryptedRejection(t *testing.T) {
	rej, ok := parseOpenAIEncryptedRejection([]byte(`{"error":{"code":"invalid_encrypted_content","message":"` + encTestFragmentMessage + `"}}`))
	require.True(t, ok)
	require.Equal(t, "gAAA", rej.fragHead)
	require.Equal(t, "aSJ9", rej.fragTail)
	require.Empty(t, rej.itemID)

	rej, ok = parseOpenAIEncryptedRejection([]byte(encTestFailedSSE("The encrypted content for item amsg_01a130c6-bf36-74b2-837f-eeaa23c47257 could not be verified. Reason: encrypted content hydration failed")))
	require.True(t, ok)
	require.Equal(t, "amsg_01a130c6-bf36-74b2-837f-eeaa23c47257", rej.itemID)

	rej, ok = parseOpenAIEncryptedRejection([]byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}`))
	require.True(t, ok)
	require.True(t, rej.functionOutput)

	_, ok = parseOpenAIEncryptedRejection([]byte(`{"error":{"code":"thinking_signature_invalid","message":"Invalid signature in thinking block"}}`))
	require.False(t, ok)
	_, ok = parseOpenAIEncryptedRejection([]byte(`{"error":{"code":"thinking_signature_invalid","message":"encrypted reasoning signature is invalid"}}`))
	require.True(t, ok)

	for _, payload := range []string{
		`{"error":{"code":"rate_limit_exceeded","message":"Too many requests"}}`,
		`{"error":{"code":"server_is_overloaded","message":"Service unavailable"}}`,
		`{"error":{"type":"invalid_request_error","message":"Invalid task protocol request"}}`,
		`{"request":{"error":{"code":"invalid_encrypted_content"}}}`,
	} {
		_, ok := parseOpenAIEncryptedRejection([]byte(payload))
		require.False(t, ok, payload)
	}
}

func TestPlanOpenAIEncryptedRecoveryItemTypes(t *testing.T) {
	fragment := openAIEncryptedRejection{code: "invalid_encrypted_content", fragHead: "gAAA", fragTail: "aSJ9"}
	unlocated := openAIEncryptedRejection{code: "invalid_encrypted_content"}
	cases := []struct {
		name     string
		body     string
		rej      openAIEncryptedRejection
		failFast bool
		reason   string
		targets  []int
	}{
		{"fragment_locates_one_reasoning", string(encTestKABody("s", encTestOwnCipher, encTestForeignCipher, encTestOtherCipher)), fragment, false, "located_fragment", []int{1}},
		{"item_id_reasoning", `{"store":false,"input":[{"type":"reasoning","id":"amsg_x","summary":[],"encrypted_content":"gAAAAx"},{"type":"reasoning","id":"rs_y","summary":[],"encrypted_content":"gAAAAy"}]}`,
			openAIEncryptedRejection{code: "invalid_encrypted_content", itemID: "amsg_x"}, false, "located_item", []int{0}},
		{"item_id_agent_message_missing", string(encTestKABody("s", encTestForeignCipher)), openAIEncryptedRejection{code: "invalid_encrypted_content", itemID: "amsg_missing"}, true, "non_reasoning_item", nil},
		{"item_id_message_nested_cipher", `{"input":[{"type":"message","id":"amsg_m","role":"assistant","content":[{"type":"output_text","text":"x"},{"type":"encrypted","encrypted_content":"gAAAAm"}]},{"type":"reasoning","id":"rs_r","summary":[],"encrypted_content":"gAAAAr"}]}`,
			openAIEncryptedRejection{code: "invalid_encrypted_content", itemID: "amsg_m"}, true, "non_reasoning_item", nil},
		{"fragment_hits_compaction_only", `{"input":[{"type":"compaction","id":"cmp_1","encrypted_content":"gAAAAcompaction-aSJ9"},{"type":"reasoning","summary":[],"encrypted_content":"gAAAAr-zzzz"}]}`, fragment, true, "non_reasoning_item", nil},
		{"function_output", string(encTestKABody("s", encTestForeignCipher)), openAIEncryptedRejection{code: "invalid_encrypted_content", functionOutput: true}, true, "function_output", nil},
		{"unlocated_with_compaction", `{"input":[{"type":"compaction","encrypted_content":"gAAAAc"},{"type":"reasoning","summary":[],"encrypted_content":"gAAAAr"}]}`, unlocated, true, "unlocated_with_opaque_items", nil},
		{"unlocated_reasoning_only", string(encTestKABody("s", encTestOwnCipher, encTestForeignCipher)), unlocated, false, "whole_request", []int{0, 1}},
		{"no_reasoning_cipher", `{"input":[{"type":"message","role":"user","content":"hi"}]}`, unlocated, true, "no_reasoning_cipher", nil},
		{"previous_response_id", `{"previous_response_id":"resp_1","input":[{"type":"reasoning","summary":[],"encrypted_content":"` + encTestForeignCipher + `"}]}`, fragment, true, "previous_response_id", []int{0}},
		{"fragment_unmatched_falls_back", string(encTestKABody("s", encTestOwnCipher)), fragment, false, "whole_request", []int{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planOpenAIEncryptedRecovery(tc.rej, scanOpenAIEncryptedInput([]byte(tc.body)))
			require.Equal(t, tc.failFast, plan.failFast)
			require.Equal(t, tc.reason, plan.reason)
			var got []int
			for _, it := range plan.targets {
				got = append(got, it.index)
			}
			require.Equal(t, tc.targets, got)
		})
	}
}

func TestStripOpenAIEncryptedRecoveryTargetsStoreModes(t *testing.T) {
	withSummary := `{"type":"reasoning","id":"rs_s","summary":[{"type":"summary_text","text":"keep me"}],"encrypted_content":"` + encTestForeignCipher + `"}`
	bodyFor := func(store string) []byte {
		return []byte(`{"model":"m",` + store + `"input":[{"type":"reasoning","id":"rs_e","summary":[],"encrypted_content":"` + encTestOtherCipher + `"},` + withSummary + `,{"type":"reasoning","id":"rs_own","summary":[],"encrypted_content":"` + encTestOwnCipher + `"}],"nonce":9007199254740993}`)
	}
	pick := func(body []byte) []openAIEncryptedInputItem {
		scan := scanOpenAIEncryptedInput(body)
		return []openAIEncryptedInputItem{scan.items[0], scan.items[1]}
	}

	// store=false：整项删除，不留孤立 rs_ id；summary 非空的计数。
	body := bodyFor(`"store":false,`)
	out, stripped, dropped, err := stripOpenAIEncryptedRecoveryTargets(body, pick(body), true)
	require.NoError(t, err)
	require.Equal(t, 2, stripped)
	require.Equal(t, 1, dropped)
	require.Equal(t, int64(1), gjson.GetBytes(out, "input.#").Int())
	require.Equal(t, "rs_own", gjson.GetBytes(out, "input.0.id").String())
	require.Equal(t, encTestOwnCipher, gjson.GetBytes(out, "input.0.encrypted_content").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "nonce").Raw)

	// store=true 与缺省：只删 encrypted_content，保留 id 与 summary。
	for _, store := range []string{`"store":true,`, ``} {
		body := bodyFor(store)
		scan := scanOpenAIEncryptedInput(body)
		require.False(t, scan.storeFalse)
		out, stripped, dropped, err := stripOpenAIEncryptedRecoveryTargets(body, pick(body), scan.storeFalse)
		require.NoError(t, err)
		require.Equal(t, 2, stripped)
		require.Zero(t, dropped)
		require.Equal(t, int64(3), gjson.GetBytes(out, "input.#").Int())
		require.Equal(t, "rs_s", gjson.GetBytes(out, "input.1.id").String())
		require.Equal(t, "keep me", gjson.GetBytes(out, "input.1.summary.0.text").String())
		require.False(t, gjson.GetBytes(out, "input.0.encrypted_content").Exists())
		require.False(t, gjson.GetBytes(out, "input.1.encrypted_content").Exists())
		require.Equal(t, encTestOwnCipher, gjson.GetBytes(out, "input.2.encrypted_content").String())
	}
}

func TestAccountOpenAIEncryptedRecoveryMode(t *testing.T) {
	require.Equal(t, OpenAIEncryptedRecoveryModeOff, encTestAccount(1, "", nil).OpenAIEncryptedRecoveryMode(62))
	require.Equal(t, OpenAIEncryptedRecoveryModeOff, encTestAccount(1, "bogus", nil).OpenAIEncryptedRecoveryMode(62))
	require.Equal(t, OpenAIEncryptedRecoveryModeShadow, encTestAccount(1, "shadow", nil).OpenAIEncryptedRecoveryMode(62))
	scoped := encTestAccount(1, "on", map[string]any{accountExtraOpenAIEncryptedRecoveryGroupIDs: []any{float64(62)}})
	require.Equal(t, OpenAIEncryptedRecoveryModeOn, scoped.OpenAIEncryptedRecoveryMode(62))
	require.Equal(t, OpenAIEncryptedRecoveryModeOff, scoped.OpenAIEncryptedRecoveryMode(7))
	malformed := encTestAccount(1, "on", map[string]any{accountExtraOpenAIEncryptedRecoveryGroupIDs: "62"})
	require.Equal(t, OpenAIEncryptedRecoveryModeOff, malformed.OpenAIEncryptedRecoveryMode(62))
	require.Equal(t, 60, encTestAccount(1, "on", nil).openAIEncryptedRecoveryRetryPerMinute())
}

// KA 复现：HTTP 200 + SSE response.failed，reasoning summary 为空。on 模式下
// 剥掉被点名的项（以及其余来历不明的项），本账号生成的密文保留；重试一次成功后，
// 下一轮同一会话直接预剥，不再被拒。
func TestOpenAIEncryptedRecoveryOnSSEStripRetryAndPreStrip(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestOKSSE(encTestOwnCipher)),
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	account := encTestAccount(1094, "on", map[string]any{accountExtraOpenAIEncryptedRecoveryGroupIDs: []any{float64(62)}})

	// 第 0 轮：本账号输出了一段 reasoning 密文，记下来历。
	c0, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c0, account, encTestKABody("ka-session"))
	require.NoError(t, err)

	// 第 1 轮：客户端代理把别家生成的密文混进了历史。
	c, recorder := encTestContext(62)
	result, err := h.svc.Forward(context.Background(), c, account, encTestKABody("ka-session", encTestOwnCipher, encTestForeignCipher))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 3)
	require.Equal(t, []string{encTestOwnCipher}, encTestInputCiphers(upstream.bodies[2]))
	require.False(t, strings.Contains(string(upstream.bodies[2]), `"rs_b"`), "store=false must not leave the orphan reasoning id")
	require.Equal(t, "synthetic followup", gjson.GetBytes(upstream.bodies[2], "input.1.content.0.text").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.bodies[2], "nonce").Raw)
	require.NotContains(t, recorder.Body.String(), "invalid_encrypted_content")
	require.Empty(t, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Equal(t, int64(1), encTestMetric(h.store, "strip_retry", 1094, "on", "result=success"))
	require.Equal(t, int64(1), encTestMetric(h.store, "invalid_encrypted_content", 1094, "on"))

	// 第 2 轮：历史里仍带着同一段坏密文。
	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, encTestKABody("ka-session", encTestOwnCipher, encTestForeignCipher))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 4)
	require.Equal(t, []string{encTestOwnCipher}, encTestInputCiphers(upstream.bodies[3]))
	require.Equal(t, int64(1), encTestMetric(h.store, "pre_strip", 1094, "on"))
}

// 上游一次只点名一段坏密文：重试时一并剥掉其余来历不明的密文，成功后隔离，
// 下一轮全部预剥、一次就过。
func TestOpenAIEncryptedRecoveryQuarantinesUnprovenCiphers(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	account := encTestAccount(1094, "on", nil)
	body := encTestKABody("multi-foreign", encTestOtherCipher, encTestForeignCipher, encTestOwnCipher)
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Empty(t, encTestInputCiphers(upstream.bodies[1]))
	require.Equal(t, int64(1), gjson.GetBytes(upstream.bodies[1], "input.#").Int())

	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Empty(t, encTestInputCiphers(upstream.bodies[2]))
}

// 多实例：蓝绿两个槽共享同一存储（生产为 Redis），A 记下的 lineage B 立即可用。
func TestOpenAIEncryptedRecoveryLineageSharedAcrossInstances(t *testing.T) {
	shared := newMemoryOpenAIEncryptedRecoveryStore()
	blue, blueUpstream := newEncTestHarness(shared,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	green, greenUpstream := newEncTestHarness(shared, encTestSSE(http.StatusOK, encTestOKSSE("")))
	account := encTestAccount(1094, "on", nil)

	c, _ := encTestContext(62)
	_, err := blue.svc.Forward(context.Background(), c, account, encTestKABody("shared-session", encTestForeignCipher))
	require.NoError(t, err)
	require.Len(t, blueUpstream.bodies, 2)

	c2, _ := encTestContext(62)
	_, err = green.svc.Forward(context.Background(), c2, account, encTestKABody("shared-session", encTestForeignCipher))
	require.NoError(t, err)
	require.Len(t, greenUpstream.bodies, 1)
	require.Empty(t, encTestInputCiphers(greenUpstream.bodies[0]))
}

// lineage 按账号隔离：A 拒绝过的密文，发往 B 时原样保留。
func TestOpenAIEncryptedRecoveryLineageIsPerAccount(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), encTestKABody("per-account", encTestForeignCipher))
	require.NoError(t, err)

	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, encTestAccount(2000, "on", nil), encTestKABody("per-account", encTestForeignCipher))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Equal(t, []string{encTestForeignCipher}, encTestInputCiphers(upstream.bodies[2]))
}

// 来历：账号 X 生成的密文，发往不同密文域的 Y 时提前剥掉；同域账号原样发送。
func TestOpenAIEncryptedRecoveryProvenancePreStrip(t *testing.T) {
	const generated = "gAAAAgenerated-by-zero-account-QQ=="
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestOKSSE(generated)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	zero := encTestAccount(1094, "on", nil)
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, zero, encTestKABody("prov-session"))
	require.NoError(t, err)

	other := encTestAccount(3000, "on", nil)
	other.Credentials["base_url"] = "https://other-vendor.example.net/v1"
	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, other, encTestKABody("prov-session", generated))
	require.NoError(t, err)
	require.Empty(t, encTestInputCiphers(upstream.bodies[1]))

	sameHost := encTestAccount(1095, "on", nil)
	c3, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c3, sameHost, encTestKABody("prov-session", generated))
	require.NoError(t, err)
	require.Equal(t, []string{generated}, encTestInputCiphers(upstream.bodies[2]))
}

// 熔断：剥离重试后仍被拒 → 标记不可恢复、带 exhausted 头；同会话下一次直接失败。
func TestOpenAIEncryptedRecoveryCircuitBreaker(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestFailedSSE("The encrypted content gAAA...7jA= could not be verified.")),
		encTestSSE(http.StatusOK, encTestFailedSSE("The encrypted content gAAA...oAOd could not be verified.")),
	)
	account := encTestAccount(1094, "on", nil)
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, account, encTestKABody("dead-session", encTestOwnCipher, encTestForeignCipher))
	require.Error(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, OpenAIEncryptedRecoveryHeaderExhausted, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Contains(t, recorder.Body.String(), "invalid_encrypted_content")
	require.Equal(t, int64(1), encTestMetric(h.store, "circuit_open", 1094, "on"))
	require.Equal(t, int64(1), encTestMetric(h.store, "strip_retry", 1094, "on", "result=rejected_again"))

	c2, recorder2 := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, encTestKABody("dead-session", encTestOtherCipher))
	require.Error(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Equal(t, OpenAIEncryptedRecoveryHeaderExhausted, recorder2.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Equal(t, int64(1), encTestMetric(h.store, "fail_fast", 1094, "on", "reason=circuit_open"))
}

// 令牌桶：超出后不重试，直接 failover。
func TestOpenAIEncryptedRecoveryTokenBucketFailsOver(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
	)
	account := encTestAccount(1094, "on", map[string]any{accountExtraOpenAIEncryptedRecoveryRetryPerMinute: float64(1)})
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, account, encTestKABody("bucket-a", encTestForeignCipher))
	require.NoError(t, err)

	c2, recorder2 := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, encTestKABody("bucket-b", encTestForeignCipher))
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover), "got %v", err)
	require.Len(t, upstream.bodies, 3)
	require.Empty(t, recorder2.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Equal(t, int64(1), encTestMetric(h.store, "rate_limited", 1094, "on"))
}

func TestMemoryOpenAIEncryptedRecoveryTokenBucketRefills(t *testing.T) {
	store := newMemoryOpenAIEncryptedRecoveryStore()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 2; i++ {
		ok, _ := store.AllowOpenAIEncryptedRetry(context.Background(), 1, 2, now)
		require.True(t, ok)
	}
	ok, _ := store.AllowOpenAIEncryptedRetry(context.Background(), 1, 2, now)
	require.False(t, ok)
	ok, _ = store.AllowOpenAIEncryptedRetry(context.Background(), 1, 2, now.Add(30*time.Second))
	require.True(t, ok)
	ok, _ = store.AllowOpenAIEncryptedRetry(context.Background(), 2, 2, now)
	require.True(t, ok, "buckets are per account")
}

// agent message / compaction / function output 里的密文：不剥、不重试，尽快失败。
func TestOpenAIEncryptedRecoveryFailFastOnOpaqueItems(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
	}{
		{"agent_message", `{"model":"gpt-6-astra","stream":true,"store":false,"prompt_cache_key":"ff","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAAr"}]}`,
			"The encrypted content for item amsg_01a1333b-c3dd-7363-842e-4b5385443c73 could not be verified. Reason: encrypted content hydration failed"},
		{"compaction", `{"model":"gpt-6-astra","stream":true,"store":false,"prompt_cache_key":"ff","input":[{"type":"compaction","id":"cmp_1","encrypted_content":"gAAAAc-aSJ9"},{"type":"reasoning","summary":[],"encrypted_content":"gAAAAr"}]}`,
			encTestFragmentMessage},
		{"function_output", string(encTestKABody("ff", encTestForeignCipher)), "Encrypted function output content could not be decrypted or decoded."},
		{"previous_response_id", `{"model":"gpt-6-astra","stream":true,"store":false,"prompt_cache_key":"ff","previous_response_id":"resp_1","input":[{"type":"reasoning","summary":[],"encrypted_content":"` + encTestForeignCipher + `"}]}`,
			encTestFragmentMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, upstream := newEncTestHarness(nil, encTestSSE(http.StatusOK, encTestFailedSSE(tc.message)), encTestSSE(http.StatusOK, encTestOKSSE("")))
			c, recorder := encTestContext(62)
			_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), []byte(tc.body))
			require.Error(t, err)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, OpenAIEncryptedRecoveryHeaderExhausted, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
			require.Contains(t, recorder.Body.String(), "invalid_encrypted_content")
		})
	}
}

// HTTP 400 JSON 路径在 on 模式下同样按新规则剥离（store=false 整项删除）。
func TestOpenAIEncryptedRecoveryOnHTTP400(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestJSON(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"`+encTestFragmentMessage+`"}}`),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	account := encTestAccount(1094, "on", nil)
	c, _ := encTestContext(62)
	sessionHash := h.svc.GenerateSessionHash(c, encTestKABody("http400"))
	require.NoError(t, h.store.RecordOpenAIEncryptedProvenance(context.Background(), 62, sessionHash,
		map[string]string{openAIEncryptedContentDigest(encTestOwnCipher): "host:zero.example.com#1094"}, 512, time.Hour))
	_, err := h.svc.Forward(context.Background(), c, account, encTestKABody("http400", encTestOwnCipher, encTestForeignCipher))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, []string{encTestOwnCipher}, encTestInputCiphers(upstream.bodies[1]))
	require.Equal(t, int64(2), gjson.GetBytes(upstream.bodies[1], "input.#").Int())
}

// 已向客户端输出后不再重试。
func TestOpenAIEncryptedRecoveryNoRetryAfterOutput(t *testing.T) {
	prefix := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"already delivered\"}\n\n"
	h, upstream := newEncTestHarness(nil, encTestSSE(http.StatusOK, prefix+encTestFailedSSE(encTestFragmentMessage)), encTestSSE(http.StatusOK, encTestOKSSE("")))
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), encTestKABody("committed", encTestForeignCipher))
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Contains(t, recorder.Body.String(), "already delivered")
}

// shadow：只记录，不改请求；原有逻辑（summary 为空时不重试）保持不变。
func TestOpenAIEncryptedRecoveryShadowDoesNotChangeRequests(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	account := encTestAccount(1094, "shadow", nil)
	body := encTestKABody("shadow-session", encTestOwnCipher, encTestForeignCipher)
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Empty(t, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Equal(t, int64(1), encTestMetric(h.store, "invalid_encrypted_content", 1094, "shadow"))

	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, []string{encTestOwnCipher, encTestForeignCipher}, encTestInputCiphers(upstream.bodies[1]))
	require.Equal(t, int64(1), encTestMetric(h.store, "pre_strip", 1094, "shadow"))
}

// 默认关闭：不触碰 Redis，也不改变原有行为。
func TestOpenAIEncryptedRecoveryOffKeepsLegacyBehavior(t *testing.T) {
	h, upstream := newEncTestHarness(nil, encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)), encTestSSE(http.StatusOK, encTestOKSSE("")))
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "", nil), encTestKABody("off", encTestForeignCipher))
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Empty(t, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Zero(t, encTestMetric(h.store, "invalid_encrypted_content", 1094, "off"))
	require.Nil(t, openAIEncryptedRecoveryFor(c))
}

// stream=false 也会收到 HTTP 200 + SSE response.failed：同样剥离重试并记 lineage。
func TestOpenAIEncryptedRecoveryNonStreamingSSEFailure(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	account := encTestAccount(1094, "on", nil)
	body := []byte(strings.Replace(string(encTestKABody("nonstream", encTestForeignCipher)), `"stream":true`, `"stream":false`, 1))
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Empty(t, encTestInputCiphers(upstream.bodies[1]))
	require.NotContains(t, recorder.Body.String(), "invalid_encrypted_content")
	require.Equal(t, int64(1), encTestMetric(h.store, "strip_retry", 1094, "on", "result=success"))

	// lineage 已写入：下一轮直接预剥。
	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Empty(t, encTestInputCiphers(upstream.bodies[2]))
}

// 账号 A 剥离重试后出错 failover 到 B：B 的首次拒绝不得写熔断。
func TestOpenAIEncryptedRecoveryCrossAccountDoesNotTripOtherCircuit(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestJSON(http.StatusServiceUnavailable, `{"error":{"type":"server_error","message":"upstream unavailable"}}`),
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
	)
	body := encTestKABody("cross-account", encTestForeignCipher)
	c, recorder := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, int64(1), encTestMetric(h.store, "strip_retry", 1094, "on", "result=error"))

	_, err = h.svc.Forward(context.Background(), c, encTestAccount(2000, "on", nil), body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Equal(t, OpenAIEncryptedRecoveryHeaderExhausted, recorder.Header().Get(OpenAIEncryptedRecoveryHeader))
	require.Equal(t, int64(1), encTestMetric(h.store, "fail_fast", 2000, "on", "reason=already_retried"))
	require.Zero(t, encTestMetric(h.store, "circuit_open", 2000, "on"))
	state, err := h.store.LoadOpenAIEncryptedRecoveryState(context.Background(),
		OpenAIEncryptedRecoveryKey{GroupID: 62, SessionHash: h.svc.GenerateSessionHash(c, body), AccountID: 2000}, nil)
	require.NoError(t, err)
	require.False(t, state.Unrecoverable)
}

type encTestFailingBucketStore struct {
	*memoryOpenAIEncryptedRecoveryStore
}

func (encTestFailingBucketStore) AllowOpenAIEncryptedRetry(context.Context, int64, int, time.Time) (bool, error) {
	return false, errors.New("redis: i/o timeout")
}

// 令牌桶存储故障时 fail closed：不重试，直接 failover。
func TestOpenAIEncryptedRecoveryTokenBucketStoreErrorFailsClosed(t *testing.T) {
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestFailedSSE(encTestFragmentMessage)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	h.svc.openaiEncryptedRecoveryStore = encTestFailingBucketStore{h.store}
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), encTestKABody("bucket-down", encTestForeignCipher))
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover), "got %v", err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, int64(1), encTestMetric(h.store, "rate_limit_unavailable", 1094, "on"))
}

// stream=false 收到 SSE 且 terminal output 为空时，仍从 output_item.done 记录来历。
func TestOpenAIEncryptedRecoveryNonStreamingRecordsProvenanceFromItemEvents(t *testing.T) {
	const generated = "gAAAAgenerated-in-nonstream-sse-ZZ=="
	h, upstream := newEncTestHarness(nil,
		encTestSSE(http.StatusOK, encTestOKSSE(generated)),
		encTestSSE(http.StatusOK, encTestOKSSE("")),
	)
	nonStream := func(body []byte) []byte {
		return []byte(strings.Replace(string(body), `"stream":true`, `"stream":false`, 1))
	}
	c, _ := encTestContext(62)
	_, err := h.svc.Forward(context.Background(), c, encTestAccount(1094, "on", nil), nonStream(encTestKABody("ns-prov")))
	require.NoError(t, err)

	other := encTestAccount(3000, "on", nil)
	other.Credentials["base_url"] = "https://other-vendor.example.net/v1"
	c2, _ := encTestContext(62)
	_, err = h.svc.Forward(context.Background(), c2, other, nonStream(encTestKABody("ns-prov", generated)))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Empty(t, encTestInputCiphers(upstream.bodies[1]))
}
