package dto

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// userFacingGroupJSONKeys is the full set of group keys a regular user may see
// on /groups/available and on the groups nested in their keys, subscriptions
// and redeem codes. Adding a key must be a deliberate decision: description
// holds operator notes, and routing/limit/reasoning config is admin-only.
var userFacingGroupJSONKeys = []string{
	"allow_batch_image_generation", "allow_messages_dispatch", "daily_limit_usd", "id",
	"monthly_limit_usd", "name", "peak_end", "peak_rate_enabled", "peak_rate_multiplier",
	"peak_start", "platform", "rate_multiplier", "subscription_type", "weekly_limit_usd",
}

// adminOnlyGroupJSONKeys are group keys that must stay on admin responses only.
var adminOnlyGroupJSONKeys = []string{
	"description", "fallback_group_id", "fallback_group_id_on_invalid_request",
	"rpm_limit", "max_reasoning_effort", "max_reasoning_effort_over_limit",
	"reasoning_effort_mappings", "require_oauth_only", "require_privacy_set",
	"claude_code_only", "allow_live", "image_price_1k", "video_model_prices",
}

const internalGroupNote = "按最终服务账号的售价倍率动态扣费"

func sensitiveGroupFixture() *service.Group {
	fallbackID := int64(9)
	invalidFallbackID := int64(10)
	daily := 5.0
	imagePrice := 0.04
	return &service.Group{
		ID:                              2,
		Name:                            "gpt",
		Description:                     internalGroupNote,
		Platform:                        service.PlatformOpenAI,
		RateMultiplier:                  1.2,
		Status:                          service.StatusActive,
		SubscriptionType:                "subscription",
		DailyLimitUSD:                   &daily,
		PeakRateEnabled:                 true,
		PeakStart:                       "09:00",
		PeakEnd:                         "18:00",
		PeakRateMultiplier:              1.5,
		AllowMessagesDispatch:           true,
		AllowBatchImageGeneration:       true,
		AllowLive:                       true,
		ClaudeCodeOnly:                  true,
		ImagePrice1K:                    &imagePrice,
		VideoModelPrices:                map[string]map[string]float64{"grok": {"720p": 0.1}},
		FallbackGroupID:                 &fallbackID,
		FallbackGroupIDOnInvalidRequest: &invalidFallbackID,
		RequireOAuthOnly:                true,
		RequirePrivacySet:               true,
		RPMLimit:                        600,
		MaxReasoningEffort:              "high",
		MaxReasoningEffortOverLimit:     "deny",
		ReasoningEffortMappings:         []service.ReasoningEffortMapping{{From: "xhigh", To: "high"}},
	}
}

func marshalJSONObject(t *testing.T, v any) (map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out, string(raw)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func requireUserFacingGroup(t *testing.T, label string, group any) {
	t.Helper()
	obj, ok := group.(map[string]any)
	require.True(t, ok, "%s: group should be a JSON object, got %T", label, group)
	require.Equal(t, userFacingGroupJSONKeys, sortedKeys(obj), "%s: group keys", label)
	require.Equal(t, "gpt", obj["name"])
	require.Equal(t, 1.2, obj["rate_multiplier"])
	require.Equal(t, true, obj["peak_rate_enabled"])
	require.Equal(t, true, obj["allow_messages_dispatch"])
}

func requireFullGroup(t *testing.T, label string, group any) {
	t.Helper()
	obj, ok := group.(map[string]any)
	require.True(t, ok, "%s: group should be a JSON object, got %T", label, group)
	for _, k := range adminOnlyGroupJSONKeys {
		require.Contains(t, obj, k, "%s: admin group should keep %q", label, k)
	}
	require.Equal(t, internalGroupNote, obj["description"])
}

func TestUserGroupFromService_IsWhitelist(t *testing.T) {
	got, raw := marshalJSONObject(t, UserGroupFromService(sensitiveGroupFixture()))
	requireUserFacingGroup(t, "UserGroupFromService", got)
	require.NotContains(t, raw, internalGroupNote)
	for _, k := range adminOnlyGroupJSONKeys {
		require.NotContains(t, got, k)
	}
	require.Nil(t, UserGroupFromService(nil))
}

func TestUserFacingDTOs_RedactNestedGroup(t *testing.T) {
	group := sensitiveGroupFixture()
	groupID := group.ID

	key := &service.APIKey{ID: 20, UserID: 10, Name: "prod", Key: "sk-user", GroupID: &groupID, Status: service.StatusActive, Group: group}
	sub := &service.UserSubscription{ID: 30, UserID: 10, GroupID: groupID, Status: service.SubscriptionStatusActive, Group: group}
	code := &service.RedeemCode{ID: 40, Code: "R-1", Type: service.RedeemTypeSubscription, GroupID: &groupID, ValidityDays: 30, Group: group}

	cases := map[string]struct {
		dto      any
		keepKeys map[string]any
	}{
		"UserAPIKeyFromService":           {UserAPIKeyFromService(key), map[string]any{"key": "sk-user", "name": "prod", "group_id": float64(groupID)}},
		"UserSubscriptionFromServiceUser": {UserSubscriptionFromServiceUser(sub), map[string]any{"group_id": float64(groupID), "status": service.SubscriptionStatusActive}},
		"RedeemCodeFromService":           {RedeemCodeFromService(code), map[string]any{"code": "R-1", "validity_days": float64(30)}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, raw := marshalJSONObject(t, tc.dto)
			requireUserFacingGroup(t, name, got["group"])
			require.NotContains(t, raw, internalGroupNote)
			require.NotContains(t, raw, "fallback_group_id")
			for k, v := range tc.keepKeys {
				require.Equal(t, v, got[k], "%s should still return %q", name, k)
			}
		})
	}

	require.Nil(t, UserAPIKeyFromService(nil))
	require.Nil(t, UserSubscriptionFromServiceUser(nil))
	require.Nil(t, RedeemCodeFromService(nil))

	// A key without a loaded group must not serialize an empty group object.
	got, _ := marshalJSONObject(t, UserAPIKeyFromService(&service.APIKey{ID: 21, Name: "no-group"}))
	require.NotContains(t, got, "group")
}

func TestAdminGroupDTOs_StayFull(t *testing.T) {
	group := sensitiveGroupFixture()
	groupID := group.ID

	adminGroup, _ := marshalJSONObject(t, GroupFromServiceAdmin(group))
	requireFullGroup(t, "GroupFromServiceAdmin", adminGroup)

	adminKey, _ := marshalJSONObject(t, APIKeyFromService(&service.APIKey{ID: 20, GroupID: &groupID, Group: group}))
	requireFullGroup(t, "APIKeyFromService", adminKey["group"])

	adminSub, _ := marshalJSONObject(t, UserSubscriptionFromServiceAdmin(&service.UserSubscription{ID: 30, GroupID: groupID, Group: group}))
	requireFullGroup(t, "UserSubscriptionFromServiceAdmin", adminSub["group"])

	adminCode, _ := marshalJSONObject(t, RedeemCodeFromServiceAdmin(&service.RedeemCode{ID: 40, GroupID: &groupID, Group: group}))
	requireFullGroup(t, "RedeemCodeFromServiceAdmin", adminCode["group"])
}
