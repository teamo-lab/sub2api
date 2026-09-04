package service

import "context"

const openAIStickyPriorityReclaimGroupID = int64(80)

type openAIStickyPriorityReclaimContextKey struct{}

// withOpenAIStickyPriorityReclaim enables the group-80 canary only for
// replayable full-input requests. previous_response_id and guardian-parent
// routes must remain on their original owner to preserve upstream state.
func withOpenAIStickyPriorityReclaim(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	guardianParentAccountID int64,
	requireImageCapability bool,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	enabled := groupID != nil && *groupID == openAIStickyPriorityReclaimGroupID &&
		previousResponseID == "" && guardianParentAccountID == 0 && !requireImageCapability
	return context.WithValue(ctx, openAIStickyPriorityReclaimContextKey{}, enabled)
}

func openAIStickyPriorityReclaimEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(openAIStickyPriorityReclaimContextKey{}).(bool)
	return enabled
}

// hasEligibleHigherPriorityOpenAIAccount reports whether a lower numerical
// priority tier can serve the request. It deliberately does not inspect a
// point-in-time free slot: Layer 2 owns the authoritative acquire loop and
// will still fall back to the sticky tier when every higher-priority slot is
// busy. This helper only decides whether Layer 1 may bypass that strict loop.
func (s *OpenAIGatewayService) hasEligibleHigherPriorityOpenAIAccount(
	ctx context.Context,
	groupID *int64,
	platform string,
	accounts []Account,
	sticky *Account,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requireCompact bool,
	requiredCapability OpenAIEndpointCapability,
	needsUpstreamCheck bool,
) bool {
	if s == nil || sticky == nil || sticky.Priority <= 1 || !openAIStickyPriorityReclaimEnabled(ctx) {
		return false
	}
	parentLookup := s.parentAccountLookup(ctx)
	for i := range accounts {
		candidate := &accounts[i]
		if candidate.ID <= 0 || candidate.ID == sticky.ID || candidate.Priority >= sticky.Priority {
			continue
		}
		if excludedIDs != nil {
			if _, excluded := excludedIDs[candidate.ID]; excluded {
				continue
			}
		}
		if reason := openAICompatibleAccountEligibilityFailureReason(
			ctx, candidate, platform, requestedModel, false, requiredCapability,
		); reason != "" {
			continue
		}
		if requireCompact && openAICompactSupportTier(candidate) == 0 {
			continue
		}
		if !parentHealthyForShadow(candidate, parentLookup) || s.isOpenAIAccountRequestRuntimeBlocked(candidate, requestedModel) {
			continue
		}
		if needsUpstreamCheck && groupID != nil &&
			s.isUpstreamModelRestrictedByChannel(ctx, *groupID, candidate, requestedModel, requireCompact) {
			continue
		}
		return true
	}
	return false
}
