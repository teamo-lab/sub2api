package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const temperatureRejection = `{"error":{"code":"unsupported_parameter","message":"Unsupported parameter: 'temperature' is not supported with this model.","param":"temperature","type":"invalid_request_error"}}`

func TestTemperatureRetryNormalization(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","temperature":0,"top_p":0.9,"input":"keep","metadata":{"temperature":"keep"}}`)
	for _, rejection := range []string{
		temperatureRejection,
		`{"error":{"message":"Unsupported parameter: 'temperature' is not supported with this model."}}`,
		`{"error":{"code":"unsupported_parameter","param":"temperature"}}`,
	} {
		retry, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(400, body, []byte(rejection))
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "temperature parameter rejection", reason)
		require.JSONEq(t, `{"model":"gpt-6-astra","top_p":0.9,"input":"keep","metadata":{"temperature":"keep"}}`, string(retry))
		require.True(t, gjson.GetBytes(body, "temperature").Exists(), "do not mutate caller's body")
	}
}

func TestTemperatureRetryIgnoresUnrelatedErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, rejection string
		status                int
	}{
		{"invalid value", `{"temperature":3}`, `{"error":{"code":"invalid_value","param":"temperature","message":"temperature must be between 0 and 2"}}`, 400},
		{"missing field", `{"input":"keep"}`, temperatureRejection, 400},
		{"server error", `{"temperature":0}`, temperatureRejection, 500},
		{"conflicting target", `{"temperature":0}`, `{"error":{"code":"unsupported_parameter","param":"temperature","message":"Unsupported parameter: truncation."}}`, 400},
		{"nested field", `{"temperature":0}`, `{"error":{"message":"Unsupported parameter: temperature.value"}}`, 400},
		{"other parameter", `{"temperature":0}`, `{"error":{"code":"unsupported_parameter","param":"top_p"}}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(tc.status, []byte(tc.body), []byte(tc.rejection))
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, retry)
		})
	}
}

func TestTemperatureRetryGateway(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "passthrough"} {
		for _, stream := range []bool{false, true} {
			for _, reject := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/reject=%t", path, stream, reject), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"temperature":0.7,"top_p":0.9,"input":"hello"}`, stream))
					if path == "/v1/chat/completions" {
						body = []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hello"}]}`, stream))
					}
					success := `{"id":"resp_ok","object":"response","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}`
					response := newOpenAIRejectedFieldTestResponse(200, success)
					if stream || path == "/v1/chat/completions" {
						response = newOpenAIRejectedFieldTestResponse(200, "data: {\"type\":\"response.completed\",\"response\":"+success+"}\n\ndata: [DONE]\n\n")
						response.Header.Set("Content-Type", "text/event-stream")
					}
					upstream := &httpUpstreamRecorder{}
					if reject {
						upstream.responses = append(upstream.responses, newOpenAIRejectedFieldTestResponse(400, temperatureRejection))
					}
					upstream.responses = append(upstream.responses, response)
					account := newOpenAIRejectedFieldTestAccount()
					c := newOpenAIRejectedFieldTestContext(body)
					c.Request.URL.Path = path
					if path == "passthrough" {
						account.Extra["openai_passthrough"] = true
						c.Request.URL.Path = "/v1/responses"
					}
					svc := newOpenAIRejectedFieldTestService(upstream)
					var result *OpenAIForwardResult
					var err error
					if path == "/v1/chat/completions" {
						result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					} else {
						result, err = svc.Forward(context.Background(), c, account, body)
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 11, result.Usage.InputTokens)
					require.Equal(t, 5, result.Usage.OutputTokens)
					wantCalls := 1
					if reject {
						wantCalls = 2
					}
					require.Len(t, upstream.bodies, wantCalls)
					require.InDelta(t, 0.7, gjson.GetBytes(upstream.bodies[0], "temperature").Float(), 0.001)
					if reject {
						require.False(t, gjson.GetBytes(upstream.bodies[1], "temperature").Exists())
					}
					require.InDelta(t, 0.9, gjson.GetBytes(upstream.bodies[wantCalls-1], "top_p").Float(), 0.001)
				})
			}
		}
	}
}

func TestTemperatureRetryStopsAfterRemoval(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","temperature":0,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(400, temperatureRejection),
		newOpenAIRejectedFieldTestResponse(400, temperatureRejection),
	}}
	_, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body, "", "")
	require.Error(t, err)
	require.Len(t, upstream.bodies, 2)
}
