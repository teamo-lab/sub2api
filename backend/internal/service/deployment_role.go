package service

import (
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// startSingletonJob keeps provider construction stable for Wire while making
// background execution explicit. nil config preserves legacy/test behavior.
func startSingletonJob(cfg *config.Config, name string, start func()) {
	if cfg != nil && !cfg.Deployment.RunsSingletonJobs() {
		slog.Info("singleton job disabled for API process", "service", name, "slot", cfg.Deployment.Slot)
		return
	}
	start()
}
