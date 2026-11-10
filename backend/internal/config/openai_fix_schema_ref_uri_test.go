package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIFixSchemaRefURIEnvironmentAllowlist(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_FIX_SCHEMA_REF_URI_GROUP_IDS", "2,5,6")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []int64{2, 5, 6}, cfg.Gateway.OpenAIFixSchemaRefURIGroupIDs)
}

func TestOpenAIFixSchemaRefURIDefaultsEmpty(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Gateway.OpenAIFixSchemaRefURIGroupIDs)
}

func TestOpenAIFixSchemaRefURIRejectsInvalidGroup(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_FIX_SCHEMA_REF_URI_GROUP_IDS", "6,-1")
	_, err := Load()
	require.ErrorContains(t, err, "openai_fix_schema_ref_uri_group_ids")
}
