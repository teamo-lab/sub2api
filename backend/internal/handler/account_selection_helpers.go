package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// maxBusyAccountReselects bounds how many full accounts one request steps past
// before it settles for waiting on the first one.
const maxBusyAccountReselects = 8

// installSellRateRange reads X-Teamo-Sell-Rate-Min/Max from the request and
// puts the range on the request context. It returns the range (nil when the
// caller sent none).
func installSellRateRange(c *gin.Context) *service.SellRateRange {
	r := service.ParseSellRateRange(c.GetHeader(service.SellRateMinHeader), c.GetHeader(service.SellRateMaxHeader))
	if r != nil {
		c.Request = c.Request.WithContext(service.WithSellRateRange(c.Request.Context(), r))
	}
	return r
}

// preferAccountWithFreeSlot is called when selection only offers a wait plan:
// the chosen account (often the sticky one) is at its concurrency limit. It
// looks for another eligible account in the group with a free slot before the
// request queues on the full one, so a busy account no longer turns into a
// 429 while its siblings sit idle. Full accounts are excluded only for this
// lookup; the failover state is untouched. When no other account has a free
// slot, the original wait plan is returned unchanged.
func preferAccountWithFreeSlot(
	ctx context.Context,
	first *service.AccountSelectionResult,
	failed map[int64]struct{},
	reselect func(excluded map[int64]struct{}) (*service.AccountSelectionResult, error),
) *service.AccountSelectionResult {
	if first == nil || first.Acquired || first.WaitPlan == nil || first.Account == nil || reselect == nil {
		return first
	}
	excluded := make(map[int64]struct{}, len(failed)+1)
	for id := range failed {
		excluded[id] = struct{}{}
	}
	excluded[first.Account.ID] = struct{}{}
	for i := 0; i < maxBusyAccountReselects; i++ {
		if ctx.Err() != nil {
			return first
		}
		next, err := reselect(excluded)
		if err != nil || next == nil || next.Account == nil {
			return first
		}
		if next.Acquired {
			return next
		}
		excluded[next.Account.ID] = struct{}{}
	}
	return first
}
