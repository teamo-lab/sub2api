package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	openAIStickyBurstCanaryGroupID = int64(80)
	openAIStickyBurstRecentTTL     = 10 * time.Minute
	openAIStickyBurstCacheTimeout  = 200 * time.Millisecond
)

var ErrOpenAIStickyBurstRecentNotFound = errors.New("openai sticky burst recent session not found")

// OpenAIStickyBurstStore is an optional extension implemented by the Redis
// gateway cache. Keeping it separate from GatewayCache makes the canary fail
// closed when another cache implementation has not opted in.
type OpenAIStickyBurstStore interface {
	GetOpenAIStickyBurstRecent(ctx context.Context, groupID int64, sessionKey string) (int64, error)
	SetOpenAIStickyBurstRecent(ctx context.Context, groupID int64, sessionKey string, accountID int64, ttl time.Duration) error
	IncrementOpenAIStickyBurstMetric(ctx context.Context, accountID int64, event string, ttl time.Duration) error
}

func (s *OpenAIGatewayService) openAIStickyBurstEligible(groupID *int64, requestedModel string, account *Account) bool {
	if s == nil || s.cache == nil || account == nil || derefGroupID(groupID) != openAIStickyBurstCanaryGroupID {
		return false
	}
	if account.Type != AccountTypeOAuth {
		return false
	}
	// Burst protects an already successful sticky conversation, not cold
	// admission. The account-level value defaults to +1, may be disabled with
	// zero, and is bounded by OpenAIStickyBurstMax.
	return account.ID > 0 && account.Concurrency > 0 && account.OpenAIStickyBurstExtraSlots() > 0
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
// deliberately ignored: the canary must never fail or delay a customer request
// merely because its telemetry is unavailable.
func (s *OpenAIGatewayService) ObserveOpenAIStickyBurstResult(ctx context.Context, groupID *int64, sessionHash string, accountID int64, requestedModel string, bypass bool, succeeded bool, requestErr error) {
	if s == nil || s.cache == nil || derefGroupID(groupID) != openAIStickyBurstCanaryGroupID || accountID <= 0 {
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
