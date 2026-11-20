package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// Sell rate range requested by the caller (TeamoRouter), in the same unit as
// accounts.sell_rate_multiplier: RMB charged per USD of official list price.
// TeamoRouter derives it from the user's discount cap (fold / 10 * 7) and
// sends it only for channels it bills by the serving account's sell rate.
// Selection then only considers accounts whose effective sell rate falls in
// the range, so the request is billed within the user's cap without a
// router-side static pre-filter on the whole group.
const (
	SellRateMinHeader = "X-Teamo-Sell-Rate-Min"
	SellRateMaxHeader = "X-Teamo-Sell-Rate-Max"

	// SellRateOutOfRangeCode marks the response when every account that could
	// serve the model sells outside the requested range. TeamoRouter treats it
	// as "no resource within the user's cap": it moves on to the next channel
	// without counting a channel failure.
	SellRateOutOfRangeCode = "sell_rate_out_of_range"
)

// ErrNoAccountWithinSellRateRange is returned when accounts were skipped only
// because their sell rate is outside the requested range.
var ErrNoAccountWithinSellRateRange = errors.New(SellRateOutOfRangeCode)

// SellRateRange is request-scoped. rejected counts accounts the selection
// skipped for being out of range, so the handler can tell "nothing within
// range" apart from ordinary capacity exhaustion.
type SellRateRange struct {
	Min      *float64
	Max      *float64
	rejected atomic.Int64
}

type sellRateRangeCtxKey struct{}

// ParseSellRateRange reads the two header values. Missing, blank, negative or
// non-finite values are ignored; nil is returned when neither bound is usable.
func ParseSellRateRange(minRaw, maxRaw string) *SellRateRange {
	parse := func(raw string) *float64 {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil
		}
		return &v
	}
	r := &SellRateRange{Min: parse(minRaw), Max: parse(maxRaw)}
	if r.Min != nil && *r.Min <= 0 {
		r.Min = nil
	}
	if r.Min == nil && r.Max == nil {
		return nil
	}
	return r
}

// WithSellRateRange installs the range on the request context. A nil range
// leaves the context unchanged.
func WithSellRateRange(ctx context.Context, r *SellRateRange) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, sellRateRangeCtxKey{}, r)
}

// SellRateRangeFromContext returns the request's range, or nil.
func SellRateRangeFromContext(ctx context.Context) *SellRateRange {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(sellRateRangeCtxKey{}).(*SellRateRange)
	return r
}

// Contains reports whether rate is inside the range. A small tolerance keeps
// fold/10*7 rounding (e.g. 5.5 fold = 3.85) from rejecting an exact match.
func (r *SellRateRange) Contains(rate float64) bool {
	if r == nil {
		return true
	}
	const eps = 1e-6
	if r.Min != nil && rate+eps < *r.Min {
		return false
	}
	if r.Max != nil && rate > *r.Max+eps {
		return false
	}
	return true
}

// Rejected reports whether any account was skipped for its sell rate.
func (r *SellRateRange) Rejected() bool {
	return r != nil && r.rejected.Load() > 0
}

// accountEffectiveSellRate is the rate this account's requests are billed at:
// its own sell_rate_multiplier, else the group's rate multiplier (the same
// rule TeamoRouter applies to x-teamo-upstream-account-sell-rates).
func accountEffectiveSellRate(ctx context.Context, account *Account) (float64, bool) {
	if account == nil {
		return 0, false
	}
	if v, ok := account.SellRateOverride(); ok {
		return v, true
	}
	if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(group) {
		return account.ResolveSellRateMultiplier(group.RateMultiplier), true
	}
	return 0, false
}

// isAccountWithinSellRateRange is checked wherever selection checks the profit
// gate, so it covers sticky, routed and load-balanced candidates alike. An
// account whose rate cannot be resolved is allowed: selection must not fail
// closed on missing pricing data.
func (s *GatewayService) isAccountWithinSellRateRange(ctx context.Context, account *Account) bool {
	r := SellRateRangeFromContext(ctx)
	if r == nil {
		return true
	}
	rate, ok := accountEffectiveSellRate(ctx, account)
	if !ok || r.Contains(rate) {
		return true
	}
	r.rejected.Add(1)
	return false
}

// LowestSellRateForModel is the cheapest effective sell rate among the group's
// schedulable accounts that serve the model, ignoring the requested range and
// current load. TeamoRouter turns it into the recommended discount fold.
func (s *GatewayService) LowestSellRateForModel(ctx context.Context, groupID *int64, requestedModel string) (float64, bool) {
	group, err := s.resolveGroupByIDForSellRate(ctx, groupID)
	if err != nil {
		return 0, false
	}
	ctx = s.withGroupContext(ctx, group)
	platform, hasForcePlatform, err := s.resolvePlatform(ctx, groupID, group, requestedModel)
	if err != nil {
		return 0, false
	}
	accounts, useMixed, err := s.listSchedulableAccounts(ctx, groupID, platform, hasForcePlatform)
	if err != nil {
		return 0, false
	}
	lowest, found := 0.0, false
	for i := range accounts {
		acc := &accounts[i]
		if !s.isAccountSchedulableForSelection(acc) || !s.isAccountAllowedForPlatform(acc, platform, useMixed) {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		rate, ok := accountEffectiveSellRate(ctx, acc)
		if !ok {
			continue
		}
		if !found || rate < lowest {
			lowest, found = rate, true
		}
	}
	return lowest, found
}

func (s *GatewayService) resolveGroupByIDForSellRate(ctx context.Context, groupID *int64) (*Group, error) {
	if groupID == nil || *groupID <= 0 {
		return nil, errors.New("group id required")
	}
	return s.resolveGroupByID(ctx, *groupID)
}

// SellRateOutOfRangeMessage is the response message for
// ErrNoAccountWithinSellRateRange. TeamoRouter parses min_available_sell_rate.
func SellRateOutOfRangeMessage(lowest float64, found bool) string {
	msg := SellRateOutOfRangeCode + ": no account within the requested sell rate range"
	if found {
		msg += "; min_available_sell_rate=" + strconv.FormatFloat(lowest, 'f', 4, 64)
	}
	return msg
}
