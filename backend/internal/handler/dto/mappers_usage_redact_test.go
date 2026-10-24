package dto

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// userUsageLogJSONKeys is the full set of JSON keys a regular user may see on a
// usage record. Adding a field to dto.UsageLog must be a deliberate decision:
// upstream account identity and group configuration are admin-only.
var userUsageLogJSONKeys = []string{
	"actual_cost", "api_key", "api_key_id", "billing_mode", "billing_type",
	"cache_creation_1h_tokens", "cache_creation_5m_tokens", "cache_creation_cost",
	"cache_creation_tokens", "cache_read_cost", "cache_read_tokens", "cache_ttl_overridden",
	"created_at", "duration_ms", "first_token_ms", "group", "group_id", "id",
	"image_count", "image_input_cost", "image_input_size", "image_input_tokens",
	"image_output_cost", "image_output_size", "image_output_tokens", "image_size",
	"image_size_breakdown", "image_size_source", "inbound_endpoint", "input_cost",
	"input_tokens", "ip_address", "long_context_billing_applied", "media_type", "model",
	"native_compaction_v2", "openai_ws_mode", "output_cost", "output_tokens",
	"rate_multiplier", "reasoning_effort", "request_id", "request_type", "service_tier",
	"session_id", "stream", "subscription", "subscription_id", "total_cost",
	"upstream_endpoint", "user", "user_agent", "user_id",
}

func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sensitiveUsageLogFixture() *service.UsageLog {
	fallbackID := int64(9)
	groupID := int64(2)
	return &service.UsageLog{
		ID:        1,
		UserID:    10,
		APIKeyID:  20,
		AccountID: 2,
		GroupID:   &groupID,
		RequestID: "req_redact",
		Model:     "gpt-5.6-sol",
		Account:   &service.Account{ID: 2, Name: "avemujica 0.5x router-1112"},
		APIKey:    &service.APIKey{ID: 20, Name: "prod", Key: "sk-secret", GroupID: &groupID},
		Group: &service.Group{
			ID:                      2,
			Name:                    "gpt",
			Description:             "按最终服务账号的售价倍率动态扣费",
			Platform:                "openai",
			RateMultiplier:          1.2,
			FallbackGroupID:         &fallbackID,
			RPMLimit:                600,
			ReasoningEffortMappings: []service.ReasoningEffortMapping{{}},
		},
	}
}

func TestUsageLogDTO_UserJSONKeysAreWhitelisted(t *testing.T) {
	t.Parallel()

	want := append([]string(nil), userUsageLogJSONKeys...)
	sort.Strings(want)
	require.Equal(t, want, jsonFieldNames(reflect.TypeOf(UsageLog{})),
		"dto.UsageLog JSON fields changed; review whether the new field is safe for regular users")
	require.Equal(t, []string{"id", "name"}, jsonFieldNames(reflect.TypeOf(UsageLogGroup{})))
	require.Equal(t, []string{"id", "name"}, jsonFieldNames(reflect.TypeOf(UsageLogAPIKey{})))
}

func TestUsageLogFromService_UserJSONOmitsAccountAndGroupConfig(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(UsageLogFromService(sensitiveUsageLogFixture()))
	require.NoError(t, err)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &payload))
	for key := range payload {
		require.Contains(t, userUsageLogJSONKeys, key)
	}
	for _, key := range []string{"account_id", "account", "account_rate_multiplier", "upstream_model", "channel_id"} {
		require.NotContains(t, payload, key)
	}

	var group map[string]any
	require.NoError(t, json.Unmarshal(payload["group"], &group))
	require.Equal(t, map[string]any{"id": float64(2), "name": "gpt"}, group)

	var apiKey map[string]any
	require.NoError(t, json.Unmarshal(payload["api_key"], &apiKey))
	require.Equal(t, map[string]any{"id": float64(20), "name": "prod"}, apiKey)

	require.NotContains(t, string(raw), "按最终服务账号")
	require.NotContains(t, string(raw), "avemujica")
	require.NotContains(t, string(raw), "sk-secret")
}

func TestUsageLogFromServiceAdmin_KeepsAccountAndFullGroup(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(UsageLogFromServiceAdmin(sensitiveUsageLogFixture()))
	require.NoError(t, err)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.JSONEq(t, `2`, string(payload["account_id"]))
	require.JSONEq(t, `{"id":2,"name":"avemujica 0.5x router-1112"}`, string(payload["account"]))

	var group map[string]any
	require.NoError(t, json.Unmarshal(payload["group"], &group))
	require.Equal(t, "按最终服务账号的售价倍率动态扣费", group["description"])
	require.Equal(t, float64(9), group["fallback_group_id"])
	require.Equal(t, float64(600), group["rpm_limit"])
	require.Contains(t, group, "reasoning_effort_mappings")

	var apiKey map[string]any
	require.NoError(t, json.Unmarshal(payload["api_key"], &apiKey))
	require.Equal(t, "sk-secret", apiKey["key"])
	require.Equal(t, float64(2), apiKey["group_id"])
}
