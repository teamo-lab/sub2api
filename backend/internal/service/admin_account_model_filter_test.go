//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectAccountMappedModels_DedupSortsAndSkipsWildcards(t *testing.T) {
	accounts := []Account{
		{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{
			"gemini-3.8-flash": "gemini-3.8-flash-preview",
			"gemini-3.1-*":     "gemini-3.1-pro",
		}}},
		{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{
			"gemini-3.8-flash": "gemini-3.8-flash",
			"gemini-3.1-pro":   "gemini-3.1-pro",
		}}},
		{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "k"}},
	}

	require.Equal(t, []string{"gemini-3.1-pro", "gemini-3.8-flash"}, collectAccountMappedModels(accounts))
}

func TestAccountSupportsModel_FollowsMapping(t *testing.T) {
	mapped := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{
		"gemini-3.8-*": "gemini-3.8-pro",
	}}}
	require.True(t, AccountSupportsModel(mapped, "gemini-3.8-flash"))
	require.False(t, AccountSupportsModel(mapped, "gemini-3.1-pro"))

	unmapped := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "k"}}
	require.True(t, AccountSupportsModel(unmapped, "gemini-3.1-pro"), "没配映射的号接所有模型，和调度一致")

	passthrough := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}},
		Extra:       map[string]any{"openai_passthrough": true}}
	require.True(t, AccountSupportsModel(passthrough, "gpt-6"), "透传号放行所有模型，和调度一致")

	require.False(t, AccountSupportsModel(nil, "gpt-5"))
}
