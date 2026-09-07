package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStartSingletonJobHonorsDeploymentRole(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{name: "legacy nil config", cfg: nil, want: 1},
		{name: "all", cfg: &config.Config{Deployment: config.DeploymentConfig{ProcessRole: config.ProcessRoleAll}}, want: 1},
		{name: "worker", cfg: &config.Config{Deployment: config.DeploymentConfig{ProcessRole: config.ProcessRoleWorker}}, want: 1},
		{name: "api", cfg: &config.Config{Deployment: config.DeploymentConfig{ProcessRole: config.ProcessRoleAPI, Slot: "green"}}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := 0
			startSingletonJob(tt.cfg, "test", func() { started++ })
			require.Equal(t, tt.want, started)
		})
	}
}
