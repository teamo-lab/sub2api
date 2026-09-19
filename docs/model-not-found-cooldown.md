# Model-not-found cooldown exemption

HTTP 404 with `error.code=model_not_found` (including the supported nested
response error envelope) is a request-local upstream failure. Without a code,
explicit model-not-found / unknown-model / model-does-not-exist messages also
qualify. Arbitrary echoed request fields are not used for classification.

- Do not write model rate limits, temporary unschedulability, account errors,
  or the OpenAI runtime block for this failure, even when a custom 404 rule
  would otherwise match.
- Keep the error and the existing bounded next-account failover. Disable
  same-account pool retries for this failure; the request's failed-account set
  excludes an attempted account and `MaxAccountSwitches` remains the limit.
- Ordinary endpoint 404s, 401s, 429s, 5xx errors, and plan-gated Codex 400s
  retain their existing policy.
- This does not make an unavailable upstream model available. New client
  requests may try the same account again; watch upstream 404 volume.

## Rollout and existing state

This code change does not automatically erase stored cooldowns. Deploy and
verify the new version before any approved cleanup; otherwise older instances
can recreate the state. Existing 30-minute model cooldowns can expire naturally.

For an immediate cleanup, back up affected account/model entries and remove
only entries whose reason is exactly `upstream_404_model_not_found`. Preserve
other models and other reasons, and refresh scheduler snapshots through the
existing repository/cache invalidation path. Do not use the account-wide
`ClearModelRateLimits` operation: it would also remove unrelated cooldowns.
Restoration must not overwrite newer state or extend the original expiry.

No production state is changed by the unit tests.

## Verification

```sh
cd backend
go test -tags unit ./internal/service ./internal/handler \
  -run 'ModelNotFound|ModelTemp|CodexPlanGated|HandleTempUnschedulable|CustomPolicyExclusion|OpenAIOAuth429|Pool5xx|PoolAuthFailure|APIKey5xx' -count=1
```
