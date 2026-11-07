//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// AnyRoute 实测回包结构：每个 chunk 只有 candidates/model，没有 usageMetadata。
const geminiStreamWithoutUsage = "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}],\"model\":\"gemini-4-argon\"}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" world, this is a test\"}]}}],\"model\":\"gemini-4-argon\"}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"model\":\"gemini-4-argon\"}\n\n"

const geminiStreamWithUsage = "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}]}\n\n" +
	"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":120,\"candidatesTokenCount\":7,\"totalTokenCount\":127}}\n\n"

const geminiUsageTestRequest = `{"systemInstruction":{"parts":[{"text":"You are a careful assistant."}]},"contents":[{"role":"user","parts":[{"text":"Please say hello to the whole world in one short sentence."}]}]}`

func sniff(t *testing.T, stream string, chunk int) *geminiUsageSniffer {
	t.Helper()
	s := newGeminiUsageSniffer(io.NopCloser(strings.NewReader(stream)), false)
	buf := make([]byte, chunk)
	var out strings.Builder
	for {
		n, err := s.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	require.Equal(t, stream, out.String(), "sniffer must not change forwarded bytes")
	return s
}

func TestGeminiUsageSniffer_EstimatesWhenStreamHasNoUsage(t *testing.T) {
	for _, chunk := range []int{1, 7, 4096} { // 行被拆成多次 Read 时也要能拼回来
		s := sniff(t, geminiStreamWithoutUsage, chunk)
		est := s.estimate([]byte(geminiUsageTestRequest))
		require.NotNil(t, est)
		require.Equal(t, estimateTokensForText("Hello world, this is a test"), est.OutputTokens)
		require.Equal(t, estimateGeminiRequestTokens([]byte(geminiUsageTestRequest)), est.InputTokens)
		require.Greater(t, est.InputTokens, 0)
	}
}

func TestGeminiUsageSniffer_NoEstimateWhenUpstreamReportsUsage(t *testing.T) {
	s := sniff(t, geminiStreamWithUsage, 5)
	require.Nil(t, s.estimate([]byte(geminiUsageTestRequest)))
}

func TestGeminiUsageSniffer_ZeroUsageMetadataCountsAsMissing(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":0,\"candidatesTokenCount\":0}}\n\n"
	s := sniff(t, stream, 64)
	require.NotNil(t, s.estimate([]byte(geminiUsageTestRequest)))
}

func TestGeminiUsageSniffer_CumulativeTextNotDoubleCounted(t *testing.T) {
	stream := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"abcd efgh\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"abcd efgh ijkl mnop\"}]}}]}\n\n"
	s := sniff(t, stream, 64)
	est := s.estimate([]byte(geminiUsageTestRequest))
	require.NotNil(t, est)
	require.Equal(t, estimateTokensForText("abcd efgh ijkl mnop"), est.OutputTokens)
}

func TestGeminiUsageSniffer_NoCandidatesNoEstimate(t *testing.T) {
	s := sniff(t, "data: {\"error\":{\"message\":\"x\"}}\n\n", 64)
	require.Nil(t, s.estimate([]byte(geminiUsageTestRequest)))
}

func TestEstimateGeminiRequestTokens_CountsToolTraffic(t *testing.T) {
	plain := estimateGeminiRequestTokens([]byte(geminiUsageTestRequest))
	withTools := `{"contents":[{"role":"user","parts":[{"text":"Please say hello to the whole world in one short sentence."}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"hello world population"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"about eight billion people live on earth today"}}}]}],` +
		`"tools":[{"functionDeclarations":[{"name":"lookup","description":"search the web","parameters":{"type":"object"}}]}]}`
	require.Greater(t, estimateGeminiRequestTokens([]byte(withTools)), plain)
}

func TestGeminiForwardNative_StreamWithoutUsageGetsEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiStreamWithoutUsage)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", strings.NewReader(geminiUsageTestRequest))

	res, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-4-argon", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.Greater(t, res.Usage.InputTokens, 0)
	require.Equal(t, estimateTokensForText("Hello world, this is a test"), res.Usage.OutputTokens)
}

func TestGeminiForwardNative_StreamWithUsageKeepsUpstreamNumbers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiStreamWithUsage)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-4-argon:streamGenerateContent?alt=sse", strings.NewReader(geminiUsageTestRequest))

	res, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(), "gemini-4-argon", "streamGenerateContent", true, []byte(geminiUsageTestRequest))
	require.NoError(t, err)
	require.Equal(t, 120, res.Usage.InputTokens)
	require.Equal(t, 7, res.Usage.OutputTokens)
}

func TestGeminiForwardAsChatCompletions_StreamWithoutUsageGetsEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newGeminiSkippedWriteService(http.StatusOK, geminiStreamWithoutUsage)
	body := []byte(`{"model":"gemini-4-argon","stream":true,"messages":[{"role":"user","content":"Please say hello to the whole world in one short sentence."}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	res, err := svc.ForwardAsChatCompletions(context.Background(), c, geminiPoolModeAPIKeyAccount(), body)
	require.NoError(t, err)
	require.Greater(t, res.Usage.InputTokens, 0)
	require.Equal(t, estimateTokensForText("Hello world, this is a test"), res.Usage.OutputTokens)
}
