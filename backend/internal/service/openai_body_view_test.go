package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 这些用例覆盖大请求体快路径（预检）的边界：预检只能把「肯定不用改写」的请求挡在完整解码之外，
// 键名或取值用 \uXXXX 转义、重复键等写法都必须照常走完整逻辑。

func TestNormalizeOpenAIResponsesLegacyIngressEscapedKeyStillNormalized(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)
	normalized, changed, err := normalizeOpenAIResponsesLegacyIngress(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotContains(t, string(normalized), "messages")
}

func TestNormalizeOpenAIResponsesLegacyIngressNativeBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hi"}]}`)
	normalized, changed, err := normalizeOpenAIResponsesLegacyIngress(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(normalized))
}

func TestNormalizeOpenAIResponsesLegacyIngressNonObjectKeepsError(t *testing.T) {
	_, _, err := normalizeOpenAIResponsesLegacyIngress([]byte(`[1,2]`))
	require.Error(t, err)
}

func TestNormalizeCompactionTriggerInputOrderEscapedType(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction_trigger"},{"type":"message","role":"user","content":"hi"}]}`)
	normalized, changed, err := NormalizeCompactionTriggerInputOrder(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, strings.HasSuffix(string(normalized), `{"type":"compaction_trigger"}]}`), string(normalized))
}

func TestOpenAIStoreFalseReasoningReplayMayChange(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"plain messages":                {`{"store":false,"input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"c1"}]}`, false},
		"reasoning item":                {`{"input":[{"type":" reasoning ","encrypted_content":"x"}]}`, true},
		"escaped reasoning type":        {`{"input":[{"type":"reasoning"}]}`, true},
		"rs item reference":             {`{"input":[{"type":"item_reference","id":"rs_1"}]}`, true},
		"msg item reference":            {`{"input":[{"type":"item_reference","id":"msg_1"}]}`, false},
		"message with null call_id":     {`{"input":[{"type":"message","call_id":null}]}`, true},
		"duplicate type last wins":      {`{"input":[{"type":"message","type":"reasoning"}]}`, true},
		"non string type":               {`{"input":[{"type":7,"call_id":"c"}]}`, false},
		"input not array":               {`{"input":"hi"}`, false},
		"image generation with call id": {`{"input":[{"type":"image_generation_call","call_id":"c"}]}`, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIStoreFalseReasoningReplayMayChange([]byte(tc.body)))
			if !tc.want {
				normalized, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay([]byte(tc.body), true)
				require.NoError(t, err)
				require.False(t, changed)
				require.Equal(t, tc.body, string(normalized))
			}
		})
	}
}

func TestOpenAIRequestBodyMayContainEmptyBase64Payload(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"real image":            {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}`, false},
		"no data uri":           {`{"input":[{"type":"input_image","image_url":"https://example.com/a.png"}]}`, false},
		"empty payload":         {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`, true},
		"whitespace payload":    {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,  "}]}`, true},
		"escaped newline":       {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,\n"}]}`, true},
		"unicode space":         {"{\"input\":[{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64, \"}]}", true},
		"escaped comma":         {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`, true},
		"escaped letter":        {`{"input":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`, true},
		"unrelated cjk escapes": {`{"input":[{"type":"message","content":"中文"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`, false},
		"truncated escape":      {`{"x":"\u00`, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIRequestBodyMayContainEmptyBase64Payload([]byte(tc.body)))
		})
	}
	require.True(t, openAIRequestBodyMayContainEmptyBase64InputImage([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,"}]}]}`)))
	require.False(t, openAIRequestBodyMayContainEmptyBase64InputImage([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)))
}

func TestSanitizeOpenAIResponsesInputItemIDsPrecheckMatches(t *testing.T) {
	clean := []byte(`{"input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","id":"fc_1","call_id":"c1"}]}`)
	require.False(t, openAIResponsesInputItemIDsNeedSanitization(clean))
	out, changed, err := sanitizeOpenAIResponsesInputItemIDs(clean)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(clean), string(out))

	dirty := []byte(`{"input":[{"type":"function_call","id":"msg_1","call_id":"c1"}]}`)
	require.True(t, openAIResponsesInputItemIDsNeedSanitization(dirty))
	_, changed, err = sanitizeOpenAIResponsesInputItemIDs(dirty)
	require.NoError(t, err)
	require.True(t, changed)
}

func TestSanitizeOpenAIResponsesToolSchemasFastStringPathStillRejectsControlBytes(t *testing.T) {
	body := []byte("{\"tools\":[{\"type\":\"function\",\"name\":\"f\",\"parameters\":{\"type\":null}}],\"input\":\"a\x01b\"}")
	_, _, err := sanitizeOpenAIResponsesToolParameterTypes(body)
	require.Error(t, err)

	escaped := []byte(`{"tools":[{"type":"function","name":"f\"q","parameters":{"type":null}}],"input":[{"type":"message","content":"x\\\"y"}]}`)
	out, changed, err := sanitizeOpenAIResponsesToolParameterTypes(escaped)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(out), `"parameters":{"type":"object"}`)
}

func TestOpenAIResponsesBodyMayContainRegexLookaround(t *testing.T) {
	require.False(t, openAIResponsesBodyMayContainRegexLookaround([]byte(`{"pattern":"^(a|b)$"}`)))
	require.True(t, openAIResponsesBodyMayContainRegexLookaround([]byte(`{"pattern":"(?=a)"}`)))
	require.True(t, openAIResponsesBodyMayContainRegexLookaround([]byte(`{"pattern":"(?=a)"}`)))
	require.True(t, openAIResponsesBodyMayContainRegexLookaround([]byte(`{"pattern":"(?=a)"}`)))
}
