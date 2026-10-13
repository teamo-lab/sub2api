package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestEncryptedContentFailureWithoutTypeIsNotTransient(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}}`,
		`{"type":"error","error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}`,
	} {
		require.Equal(t, http.StatusBadRequest, openAIStreamFailedEventSemanticStatus([]byte(payload), ""))
		require.False(t, openAIStreamFailedEventShouldFailover([]byte(payload), ""))
		require.False(t, openAIStreamErrorEventShouldFailover([]byte(payload), ""))
	}
}

func TestOpaqueEncryptedToolOutputDoesNotFailOverOrLoseContent(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"encrypted_content","encrypted_content":"opaque-original"}]}]}`)
	failure := `{"type":"response.failed","response":{"id":"resp_err","status":"failed","error":{"code":"invalid_encrypted_content","message":"Encrypted function output content could not be decrypted or decoded."}}}`
	for _, passthrough := range []bool{false, true} {
		response := newOpenAIRejectedFieldTestResponse(200, "data: "+failure+"\n\n")
		response.Header.Set("Content-Type", "text/event-stream")
		upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
		c := newOpenAIRejectedFieldTestContext(body)
		account := newOpenAIRejectedFieldTestAccount()
		account.Extra["openai_passthrough"] = passthrough
		_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, account, body)
		require.Error(t, err)
		var failover *UpstreamFailoverError
		require.False(t, errors.As(err, &failover))
		require.Len(t, upstream.bodies, 1)
		require.Contains(t, string(upstream.bodies[0]), "opaque-original")
	}
}

func TestEncryptedFunctionOutputNeverTriggersUnrelatedReasoningRepair(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"keep-reasoning","summary":[{"type":"summary_text","text":"keep"}]},{"type":"message","role":"user","content":"hello"}]}`)
	failure := `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."}}`
	for _, passthrough := range []bool{false, true} {
		upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(400, failure)}}
		c := newOpenAIRejectedFieldTestContext(body)
		account := newOpenAIRejectedFieldTestAccount()
		account.Extra["openai_passthrough"] = passthrough
		_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, account, body)
		require.Error(t, err)
		require.Len(t, upstream.bodies, 1)
		require.Contains(t, string(upstream.bodies[0]), "keep-reasoning")
		initializeOpenAIEncryptedSemanticRetry(c, account, body)
		require.Nil(t, newOpenAIEncryptedSemanticRetrySignal(c, []byte(failure)))
	}
}
