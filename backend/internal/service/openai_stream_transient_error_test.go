package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamBareStructuredTransientError(t *testing.T) {
	for _, tc := range []struct {
		name, payload, message string
		want                   bool
	}{
		{"server_code", `{"type":"error","error":{"code":"server_error","message":"Internal server error"}}`, "Internal server error", true},
		{"upstream_code", `{"type":"error","error":{"code":"upstream_error","message":"Internal server error"}}`, "Internal server error", true},
		{"server_type", `{"type":"error","error":{"type":"server_error","message":"Internal server error"}}`, "Internal server error", true},
		{"nested_server_type", `{"type":"error","response":{"error":{"type":"server_error","message":"Internal server error"}}}`, "Internal server error", true},
		{"unknown", `{"type":"error","error":{"code":"unknown_error","message":"Internal server error"}}`, "Internal server error", false},
		{"unstructured", `{"type":"error","error":{"message":"Internal server error"}}`, "Internal server error", false},
		{"event_metadata", `{"type":"server_error","message":"Internal server error"}`, "Internal server error", false},
		{"echoed_request", `{"type":"error","error":{"message":"Internal server error"},"request":{"error":{"code":"server_error"}}}`, "Internal server error", false},
		{"invalid_code_wins", `{"type":"error","error":{"code":"invalid_request_error","type":"server_error","message":"Bad request"}}`, "Bad request", false},
		{"invalid_type", `{"type":"error","error":{"code":"server_error","type":"invalid_request_error","message":"Bad request"}}`, "Bad request", false},
		{"policy_type", `{"type":"error","error":{"code":"server_error","type":"content_policy_violation","message":"Blocked by content policy"}}`, "Blocked by content policy", false},
		{"context_window", `{"type":"error","error":{"code":"context_length_exceeded","type":"server_error","message":"Your input exceeds the context window of this model"}}`, "Your input exceeds the context window of this model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIStreamErrorEventShouldFailover([]byte(tc.payload), tc.message))
		})
	}
}
