package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestprofile"
)

func profileAutomationBootstrap(ctx context.Context, body []byte) ([]byte, bool) {
	defer requestprofile.Start(ctx, "bootstrap_automation")()
	return normalizeCodexAutomationBootstrap(body)
}
func profileDelegationBootstrap(ctx context.Context, body []byte) ([]byte, bool) {
	defer requestprofile.Start(ctx, "bootstrap_delegation")()
	return normalizeCodexDelegationBootstrap(body)
}
