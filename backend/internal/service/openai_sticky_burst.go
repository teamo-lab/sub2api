package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	openAIStickyBurstRecentTTL    = 10 * time.Minute
	openAIStickyBurstCacheTimeout = 200 * time.Millisecond
)

var ErrOpenAIStickyBurstRecentNotFound = errors.New("openai sticky burst recent session not found")

// OpenAIStickyBurstStore is an optional extension implemented by the Redis
// gateway cache. Keeping it separate from GatewayCache makes bursting fail
// closed when another cache implementation has not opted in.
type OpenAIStickyBurstStore interface {
	GetOpenAIStickyBurstRecent(ctx context.Context, groupID int64, sessionKey string) (int64, error)
	SetOpenAIStickyBurstRecent(ctx context.Context, groupID int64, sessionKey string, accountID int64, ttl time.Duration) error
	IncrementOpenAIStickyBurstMetric(ctx context.Context, accountID int64, event string, ttl time.Duration) error
}

func (s *OpenAIGatewayService) openAIStickyBurstEligible(groupID *int64, requestedModel string, account *Account) bool {
	if s == nil || s.cache == nil || account == nil || derefGroupID(groupID) <= 0 {
		return false
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return false
	}
	// Burst protects an already successful sticky conversation, not cold
	// admission. The account-level value defaults to +1, may be disabled with
	// zero, and is bounded by OpenAIStickyBurstMax.
	return account.ID > 0 && account.Concurrency > 0 && account.OpenAIStickyBurstExtraSlots() > 0
}

// tryAcquireOpenAIStickySlot uses the same slot counter for durable and burst
// capacity. Callers must first validate the sticky binding and account. The
// returned ceiling also belongs in the wait plan: otherwise a waiter admitted
// at the burst ceiling can only wake once usage drops below durable capacity.
func (s *OpenAIGatewayService) tryAcquireOpenAIStickySlot(ctx context.Context, groupID *int64, sessionHash, requestedModel string, account *Account) (*AcquireResult, bool, int, error) {
	limit := account.Concurrency
	result, err := s.tryAcquireAccountSlot(ctx, account.ID, limit)
	if err != nil || result == nil || result.Acquired ||
		!s.openAIStickyBurstEligible(groupID, requestedModel, account) ||
		!s.hasRecentOpenAIStickyBurstSession(ctx, groupID, sessionHash, account.ID) {
		return result, false, limit, err
	}
	burstLimit := limit + account.OpenAIStickyBurstExtraSlots()
	burstResult, burstErr := s.tryAcquireAccountSlot(ctx, account.ID, burstLimit)
	if burstErr != nil || burstResult == nil {
		// Cache errors do not grant burst capacity or change the legacy wait path.
		return result, false, limit, nil
	}
	if burstResult.Acquired {
		s.recordOpenAIStickyBurstAcquired(ctx, account.ID)
	}
	return burstResult, true, burstLimit, nil
}

func (s *OpenAIGatewayService) hasRecentOpenAIStickyBurstSession(ctx context.Context, groupID *int64, sessionHash string, accountID int64) bool {
	store, ok := s.cache.(OpenAIStickyBurstStore)
	if !ok || strings.TrimSpace(sessionHash) == "" || accountID <= 0 {
		return false
	}
	key := s.openAISessionCacheKey(sessionHash)
	if key == "" {
		return false
	}
	recentAccountID, err := store.GetOpenAIStickyBurstRecent(ctx, derefGroupID(groupID), key)
	return err == nil && recentAccountID == accountID
}

// ObserveOpenAIStickyBurstResult records a successful recent-session marker
// and, for bypassed calls, a compact Redis outcome counter. Cache failures are
// deliberately ignored: telemetry must never fail a customer request
// merely because its telemetry is unavailable.
func (s *OpenAIGatewayService) ObserveOpenAIStickyBurstResult(ctx context.Context, groupID *int64, sessionHash string, accountID int64, requestedModel string, bypass bool, succeeded bool, requestErr error) {
	if s == nil || s.cache == nil || derefGroupID(groupID) <= 0 || accountID <= 0 {
		return
	}
	store, ok := s.cache.(OpenAIStickyBurstStore)
	if !ok {
		return
	}
	cacheCtx, cancel := openAIStickyBurstCacheContext(ctx)
	defer cancel()
	if succeeded {
		if key := s.openAISessionCacheKey(sessionHash); key != "" {
			_ = store.SetOpenAIStickyBurstRecent(cacheCtx, derefGroupID(groupID), key, accountID, openAIStickyBurstRecentTTL)
		}
	}
	if !bypass {
		return
	}
	event := "error"
	if succeeded {
		event = "success"
	} else {
		var failoverErr *UpstreamFailoverError
		if errors.As(requestErr, &failoverErr) {
			switch {
			case failoverErr.StatusCode == 429:
				event = "error_429"
			case failoverErr.StatusCode >= 500:
				event = "error_5xx"
			}
		}
	}
	_ = store.IncrementOpenAIStickyBurstMetric(cacheCtx, accountID, event, 72*time.Hour)
}

func (s *OpenAIGatewayService) recordOpenAIStickyBurstAcquired(ctx context.Context, accountID int64) {
	store, ok := s.cache.(OpenAIStickyBurstStore)
	if !ok {
		return
	}
	cacheCtx, cancel := openAIStickyBurstCacheContext(ctx)
	defer cancel()
	_ = store.IncrementOpenAIStickyBurstMetric(cacheCtx, accountID, "acquired", 72*time.Hour)
}

func openAIStickyBurstCacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), openAIStickyBurstCacheTimeout)
}
