package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Gemini pool-mode API-key accounts front a pool of upstream keys. A request that
// still fails after the same-account retries says little about the account as a
// whole, and the legacy one-strike ban (TempUnscheduleRetryableError) takes the
// account out for every model it serves. When every account in a group hits one
// unlucky request, the group runs dry ("No available accounts") even though each
// account is serving most of its traffic fine.
//
// For these accounts the failure is counted per (account, model) across gateway
// instances, and only that pair cools down, and only once failures cluster:
// poolModelFailureThreshold failures inside poolModelFailureWindowMinutes.
const (
	poolModelFailureWindowMinutes = 1
	poolModelFailureThreshold     = 5
	poolModelFailureCooldown      = time.Minute
	poolModelFailureReason        = "pool_model_failure_cooldown"
)

// PoolModelFailureCache is an optional TempUnschedCache extension that keeps the
// rolling per (account, model) failure window shared across gateway instances.
type PoolModelFailureCache interface {
	RecordPoolModelFailure(ctx context.Context, accountID int64, modelKey string, windowMinutes, threshold int) (count int64, tripped bool, err error)
}

// WithTempUnschedulableModel attaches the requested model to ctx so failure
// handling further down can scope cooldowns to (account, model).
func WithTempUnschedulableModel(ctx context.Context, model string) context.Context {
	return withTempUnschedulableModel(ctx, []string{model})
}

// observePoolModelFailure hands a Gemini pool account's exhausted-retry failure
// to the per-model policy. The account is only looked up when the caller told us
// the model; without it the legacy behaviour applies unchanged.
func (s *GatewayService) observePoolModelFailure(ctx context.Context, accountID int64, failoverErr *UpstreamFailoverError) bool {
	if s == nil || s.rateLimitService == nil {
		return false
	}
	model := TempUnschedulableModelFromContext(ctx)
	if model == "" {
		return false
	}
	var (
		account *Account
		err     error
	)
	if s.schedulerSnapshot != nil {
		account, err = s.schedulerSnapshot.GetAccount(ctx, accountID)
	} else if s.accountRepo != nil {
		account, err = s.accountRepo.GetByID(ctx, accountID)
	}
	if err != nil || account == nil {
		return false
	}
	return s.rateLimitService.ObservePoolModelFailure(ctx, account, model, failoverErr)
}

// TempUnschedulableModelFromContext returns the model attached by WithTempUnschedulableModel.
func TempUnschedulableModelFromContext(ctx context.Context) string {
	return tempUnschedulableModel(ctx, nil)
}

func usesPoolModelFailureCooldown(account *Account) bool {
	return account != nil && account.Platform == PlatformGemini && account.IsPoolMode()
}

// ObservePoolModelFailure records one exhausted-retry failure of a Gemini pool
// account and cools down the (account, model) pair once the rolling window
// reaches the threshold. It returns true when the failure belongs to this policy,
// in which case the caller must not apply an account-wide ban, whether or not a
// cooldown was written.
func (s *RateLimitService) ObservePoolModelFailure(ctx context.Context, account *Account, requestedModel string, failoverErr *UpstreamFailoverError) bool {
	if !usesPoolModelFailureCooldown(account) || failoverErr == nil {
		return false
	}
	modelKey := strings.TrimSpace(account.GetMappedModel(strings.TrimSpace(requestedModel)))
	if modelKey == "" {
		return false
	}
	if s == nil || s.accountRepo == nil {
		return true
	}
	cache, ok := s.tempUnschedCache.(PoolModelFailureCache)
	if !ok || cache == nil {
		// Without the shared window there is no safe way to tell a cluster from
		// a single bad request; per-request failover already moved this one on.
		return true
	}

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	count, tripped, err := cache.RecordPoolModelFailure(persistCtx, account.ID, modelKey, poolModelFailureWindowMinutes, poolModelFailureThreshold)
	if err != nil {
		logger.L().Warn("gemini.pool_model_failure_record_failed", zap.Int64("account_id", account.ID), zap.String("model", modelKey), zap.Error(err))
		return true
	}
	if !tripped {
		return true
	}

	until := time.Now().Add(poolModelFailureCooldown)
	reason := poolModelFailureReason + ": " + http.StatusText(failoverErr.StatusCode)
	if err := s.accountRepo.SetModelRateLimit(persistCtx, account.ID, modelKey, until, reason); err != nil {
		logger.L().Warn("gemini.pool_model_failure_cooldown_failed", zap.Int64("account_id", account.ID), zap.String("model", modelKey), zap.Error(err))
		return true
	}
	logger.L().Warn("gemini.pool_model_failure_cooldown",
		zap.Int64("account_id", account.ID),
		zap.String("model", modelKey),
		zap.Int64("failure_count", count),
		zap.Int("failure_threshold", poolModelFailureThreshold),
		zap.Int("window_minutes", poolModelFailureWindowMinutes),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Time("until", until),
	)
	return true
}
