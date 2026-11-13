package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChatImageURLBecomesAnthropicURLSource(t *testing.T) {
	var cc ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"看图"},`+
		`{"type":"image_url","image_url":{"url":"https://img.alicdn.com/imgextra/i1/O1CN01_!!6000-0-tps-1024-406.jpg"}},`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`), &cc))

	resp, err := ChatCompletionsToResponses(&cc)
	require.NoError(t, err)
	anth, err := ResponsesToAnthropicRequest(resp)
	require.NoError(t, err)

	raw, err := json.Marshal(anth)
	require.NoError(t, err)
	var decoded struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded.Messages, 1)
	blocks := decoded.Messages[0].Content
	require.Len(t, blocks, 3, "image url must not be dropped")

	urlSource := blocks[1]["source"].(map[string]any)
	require.Equal(t, map[string]any{"type": "url", "url": "https://img.alicdn.com/imgextra/i1/O1CN01_!!6000-0-tps-1024-406.jpg"}, urlSource)

	b64Source := blocks[2]["source"].(map[string]any)
	require.Equal(t, map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}, b64Source)
}

func TestAnthropicURLImageBecomesResponsesImageURL(t *testing.T) {
	require.Equal(t, "https://example.com/a.png", anthropicImageToDataURI(&AnthropicImageSource{Type: "url", URL: "https://example.com/a.png"}))
	require.Equal(t, "data:image/png;base64,AAAA", anthropicImageToDataURI(&AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}))
	require.Equal(t, "", anthropicImageToDataURI(nil))
	require.Equal(t, "", anthropicImageToDataURI(&AnthropicImageSource{Type: "base64"}))
}

func TestImageURLToAnthropicImageSourceRejectsOtherSchemes(t *testing.T) {
	require.Nil(t, imageURLToAnthropicImageSource("ftp://example.com/a.png"))
	require.Nil(t, imageURLToAnthropicImageSource("file-abc123"))
}
