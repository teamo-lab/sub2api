package config

import (
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func validDeploymentDigest() string {
	return "sha256:" + strings.Repeat("a", 64)
}

func TestDeploymentConfigRoleCapabilities(t *testing.T) {
	tests := []struct {
		role          string
		servesAPI     bool
		runsSingleton bool
	}{
		{role: ProcessRoleAll, servesAPI: true, runsSingleton: true},
		{role: ProcessRoleAPI, servesAPI: true, runsSingleton: false},
		{role: ProcessRoleWorker, servesAPI: false, runsSingleton: true},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			cfg := DeploymentConfig{ProcessRole: tt.role}
			require.Equal(t, tt.servesAPI, cfg.ServesAPI())
			require.Equal(t, tt.runsSingleton, cfg.RunsSingletonJobs())
		})
	}
}

func TestDeploymentConfigValidate(t *testing.T) {
	validAPI := DeploymentConfig{
		ProcessRole: ProcessRoleAPI,
		Slot:        "green",
		ReleaseID:   "release-20260811-001",
		Version:     "0.1.172+bluegreen.1",
		Digest:      validDeploymentDigest(),
	}
	require.NoError(t, validAPI.Validate())
	require.NoError(t, (DeploymentConfig{ProcessRole: ProcessRoleAll}).Validate())

	tests := []struct {
		name string
		cfg  DeploymentConfig
		want string
	}{
		{name: "unknown role", cfg: DeploymentConfig{ProcessRole: "both"}, want: "process_role"},
		{name: "api missing identity", cfg: DeploymentConfig{ProcessRole: ProcessRoleAPI, Slot: "blue"}, want: "required"},
		{name: "api invalid slot", cfg: DeploymentConfig{ProcessRole: ProcessRoleAPI, Slot: "worker", ReleaseID: "r1", Version: "v1", Digest: validDeploymentDigest()}, want: "blue or green"},
		{name: "worker invalid slot", cfg: DeploymentConfig{ProcessRole: ProcessRoleWorker, Slot: "green", ReleaseID: "r1", Version: "v1", Digest: validDeploymentDigest()}, want: "slot must be worker"},
		{name: "mutable digest", cfg: DeploymentConfig{ProcessRole: ProcessRoleAPI, Slot: "blue", ReleaseID: "r1", Version: "v1", Digest: "latest"}, want: "immutable sha256"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorContains(t, tt.cfg.Validate(), tt.want)
		})
	}
}

func TestLoadDeploymentFromEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SUB2API_PROCESS_ROLE", "API")
	t.Setenv("SUB2API_DEPLOYMENT_SLOT", "GREEN")
	t.Setenv("SUB2API_RELEASE_ID", "release-20260811-001")
	t.Setenv("SUB2API_DEPLOYMENT_VERSION", "0.1.172+bluegreen.1")
	t.Setenv("SUB2API_DEPLOYMENT_DIGEST", validDeploymentDigest())

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, ProcessRoleAPI, cfg.Deployment.ProcessRole)
	require.Equal(t, "green", cfg.Deployment.Slot)
	require.Equal(t, "release-20260811-001", cfg.Deployment.ReleaseID)
	require.False(t, cfg.Deployment.RunsSingletonJobs())
}

func TestDatabaseDSNIncludesDeploymentSessionIdentity(t *testing.T) {
	db := DatabaseConfig{
		Host: "postgres", Port: 5432, User: "sub2api", Password: "secret",
		DBName: "sub2api", SSLMode: "disable",
	}
	deployment := DeploymentConfig{
		ProcessRole: ProcessRoleAPI,
		Slot:        "green",
		ReleaseID:   "release-20260811-001",
		Version:     "0.1.172+bluegreen.1",
		Digest:      validDeploymentDigest(),
	}

	dsn := db.DSNWithTimezoneAndDeployment("UTC", deployment)
	require.Contains(t, dsn, "application_name=sub2api-green")
	require.Contains(t, dsn, "sub2api.deployment_slot=green")
	require.Contains(t, dsn, "sub2api.release_id=release-20260811-001")
	require.Contains(t, dsn, "sub2api.deployment_version=0.1.172+bluegreen.1")
	require.Contains(t, dsn, "sub2api.deployment_digest="+validDeploymentDigest())
	_, err := pq.NewConnector(dsn)
	require.NoError(t, err)

	legacyDSN := db.DSNWithTimezone("UTC")
	require.NotContains(t, legacyDSN, "options=")
}
