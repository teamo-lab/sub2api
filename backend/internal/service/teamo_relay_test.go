package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/relay"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTeamoRelayRequiresServerSwitchAndAuthenticatedGroup(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, groupID := range []int64{0, 3, 80} {
			for _, contract := range []bool{false, true} {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: enabled, TeamoRelayGroupIDs: []int64{3}}}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				if contract {
					c.Request.Header.Set(RouterContractHeader, RouterContractV2)
				}
				if groupID != 0 {
					c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
				}
				stage := svc.newOpenAIStreamStage(c)
				_, isRelay := stage.(*relay.CommitGate)
				require.Equal(t, enabled && groupID == 3, isRelay)
				stage.Close()
			}
		}
	}
}

func enableTeamoRelayTestGroup(c *gin.Context) {
	groupID := int64(3)
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
}

// Test vectors from New API Client / the retired Rust prototype, through Sub's
// existing parser and adapter. No remote Relay process or database is involved.
func TestTeamoRelayNativeResponsesCommitBoundary(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_relay"}}`
	failure := `{"type":"response.failed","response":{"error":{"code":"server_error","message":"upstream temporarily unavailable"}}}`
	text := `{"type":"response.output_text.delta","delta":"hello"}`
	complete := `{"type":"response.completed","response":{"id":"resp_relay","usage":{"input_tokens":10,"output_tokens":1}}}`
	tests := []struct {
		name     string
		events   []string
		failover bool
		failure  bool
		contains string
	}{
		{"preamble_failure", []string{created, failure, text, complete}, true, true, ""},
		{"partial_tool_failure", []string{created,
			`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","name":"lookup","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{"}`, failure}, true, true, ""},
		{"output_failure", []string{created, text, failure}, false, true, "hello"},
		{"reasoning_failure", []string{created, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, failure}, false, true, "thinking"},
		{"reasoning_truncated", []string{created, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`}, false, true, "thinking"},
		{"preamble_truncated", []string{created}, true, true, ""},
		{"success", []string{created, text, complete}, false, false, "hello"},
	}
	for _, adapter := range []string{"native", "passthrough", "responses_to_chat"} {
		for _, tc := range tests {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set(RouterContractHeader, RouterContractV2)
				enableTeamoRelayTestGroup(c)
				var wire strings.Builder
				for _, event := range tc.events {
					wire.WriteString("data: " + event + "\n\n")
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire.String()))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				var err error
				var gotUsage *OpenAIUsage
				switch adapter {
				case "native":
					var result *openaiStreamingResult
					result, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					if result != nil {
						gotUsage = result.usage
					}
				case "passthrough":
					var result *openaiStreamingResultPassthrough
					result, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					if result != nil {
						gotUsage = result.usage
					}
				case "responses_to_chat":
					var result *OpenAIForwardResult
					result, err = svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", time.Now(), 20, "")
					if result != nil {
						gotUsage = &result.Usage
					}
				}
				require.Equal(t, tc.failure, err != nil, "error=%v", err)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.failover, errors.As(err, &failover), "error=%v", err)
				if tc.contains == "" {
					require.Empty(t, rec.Body.String())
				} else {
					require.Contains(t, rec.Body.String(), tc.contains)
				}
				if tc.name == "success" {
					require.NotNil(t, gotUsage)
					require.Equal(t, 10, gotUsage.InputTokens)
				}
			})
		}
	}
}

func TestTeamoRelayChatAcrossAdapters(t *testing.T) {
	role := `{"id":"chat_1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`
	text := `{"id":"chat_1","choices":[{"index":0,"delta":{"content":"hello"}}]}`
	done := `{"id":"chat_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usage := `{"id":"chat_1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}`
	failure := `{"error":{"code":"server_error","message":"temporarily unavailable"}}`
	policy := `{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates content policy"}}`
	partialTool := `{"id":"chat_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{"}}]}}]}`
	tests := []struct {
		name                     string
		events                   []string
		failover, failure, empty bool
		wantUsage                int
	}{
		{"role_error", []string{role, failure, text, done, "[DONE]"}, true, true, true, 0},
		{"partial_tool_error", []string{role, partialTool, failure}, true, true, true, 0},
		{"partial_tool_done", []string{role, partialTool, "[DONE]"}, true, true, true, 0},
		{"role_eof", []string{role}, true, true, true, 0},
		{"empty_done", []string{role, "[DONE]"}, true, true, true, 0},
		{"empty_finish_done", []string{role, done, "[DONE]"}, true, true, true, 0},
		{"role_usage_error", []string{role, usage, failure}, true, true, true, 12},
		{"output_error", []string{role, text, failure}, false, true, false, 0},
		{"output_usage_eof", []string{role, text, usage}, false, true, false, 12},
		{"policy", []string{role, policy}, false, true, false, 0},
		{"success", []string{role, text, done, usage, "[DONE]"}, false, false, false, 12},
	}
	for _, adapter := range []string{"raw", "responses", "messages"} {
		for _, tc := range tests {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{TeamoRelayEnabled: true, TeamoRelayGroupIDs: []int64{3}}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set(RouterContractHeader, RouterContractV2)
				enableTeamoRelayTestGroup(c)
				var wire strings.Builder
				for _, event := range tc.events {
					wire.WriteString("data: " + event + "\n\n")
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire.String()))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				var result *OpenAIForwardResult
				var err error
				switch adapter {
				case "raw":
					result, err = svc.streamRawChatCompletions(c, resp, account, "model", "model", "model", nil, nil, time.Now(), 20)
				case "responses":
					result, err = svc.streamChatCompletionsAsResponses(c, resp, account, "model", nil, nil, false, nil, "model", "model", nil, nil, time.Now())
				case "messages":
					result, err = svc.streamChatCompletionsAsAnthropic(c, resp, account, "model", "model", "model", nil, nil, time.Now())
				}
				require.NotNil(t, result)
				require.Equal(t, tc.failure, err != nil, "error=%v", err)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.failover, errors.As(err, &failover), "error=%v", err)
				require.Equal(t, tc.wantUsage, result.Usage.InputTokens)
				if tc.wantUsage > 0 {
					require.Equal(t, 8, result.Usage.CacheReadInputTokens)
				}
				if tc.empty {
					require.Empty(t, rec.Body.String())
				}
				if tc.failure {
					require.NotContains(t, rec.Body.String(), "response.completed")
					require.NotContains(t, rec.Body.String(), "message_stop")
					require.NotContains(t, rec.Body.String(), "[DONE]")
				}
				if tc.name == "success" {
					require.Contains(t, rec.Body.String(), "hello")
				}
				if adapter == "responses" && tc.name == "output_error" {
					ids := map[string]struct{}{}
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if data, ok := strings.CutPrefix(line, "data: "); ok {
							if id := gjson.Get(data, "response.id").String(); id != "" {
								ids[id] = struct{}{}
							}
						}
					}
					require.Len(t, ids, 1, "failure must terminate the same response ID")
					require.True(t, IsResponseCommitted(c), "outer handler must not append another terminal")
				}
				if tc.name == "policy" {
					require.Equal(t, http.StatusBadRequest, rec.Code)
					require.True(t, IsResponseCommitted(c))
				}

			})
		}
	}
}
