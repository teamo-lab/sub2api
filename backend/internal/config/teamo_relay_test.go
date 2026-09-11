package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTeamoRelayEnvironmentAllowlist(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_TEAMO_RELAY_ENABLED", "true")
	t.Setenv("GATEWAY_TEAMO_RELAY_GROUP_IDS", "3,29,31")
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Gateway.TeamoRelayEnabled)
	require.Equal(t, []int64{3, 29, 31}, cfg.Gateway.TeamoRelayGroupIDs)
}

func TestTeamoRelayRejectsInvalidGroup(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("GATEWAY_TEAMO_RELAY_GROUP_IDS", "3,-1")
	_, err := Load()
	require.ErrorContains(t, err, "teamo_relay_group_ids")
}
