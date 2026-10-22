package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStripSchemaRefSiblingsEnvironmentAllowlist(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_STRIP_SCHEMA_REF_SIBLINGS_GROUP_IDS", "2,5")
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []int64{2, 5}, cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs)
}

func TestOpenAIStripSchemaRefSiblingsDefaultsEmpty(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Gateway.OpenAIStripSchemaRefSiblingsGroupIDs)
}

func TestOpenAIStripSchemaRefSiblingsRejectsInvalidGroup(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_OPENAI_STRIP_SCHEMA_REF_SIBLINGS_GROUP_IDS", "2,0")
	_, err := Load()
	require.ErrorContains(t, err, "openai_strip_schema_ref_siblings_group_ids")
}
