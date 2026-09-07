# 43 production runbook

## Read-only preflight

```bash
ssh -o BatchMode=yes -o ConnectTimeout=10 root@43.159.0.162 \
  '/opt/sub2api/deploy/bin/sub2api-rollout status --json'
```

Inspect `/opt/sub2api/deploy/rollout/state.json`, blue/green/worker health, image IDs, restart counts, and embedded application commit. Never print secrets from environment files, Docker environment variables, database URLs, or API keys.

## Source and build provenance

Create the build context from the freshly fetched `origin/main` object, not the current worktree. Use an isolated detached worktree or `git archive <main-sha>`; never copy modified local files into it.

Pass the full main SHA to the existing build as its commit value. After building, query the binary or application version endpoint and require the reported commit to equal the captured SHA. Retain an audit record containing release ID, SHA, version, digest, and timestamp without credentials.

A retained server build directory may also be compared to the exact Git tree. File equality is useful evidence but does not replace correct embedded commit metadata.

## Managed rollout

The controller is `/opt/sub2api/deploy/bin/sub2api-rollout`; managed scripts are under `/opt/sub2api/deploy/rollout/`. Inspect command `--help` and the current repository implementation before invoking them because arguments can evolve.

Invariants:

- Stable begins healthy at 100%; candidate begins healthy at 0% with zero active streams.
- Stage only an immutable image digest and validate at 0%.
- Weight changes, completion, and rollback go through the controller.
- Never edit HAProxy state or Compose image variables manually.
- Never restart/remove the active slot or kill long-lived streams.
- API slots do not run singleton jobs; exactly one managed worker owns them.
- Keep database, Redis, data, and runtime configuration local to 43.

Use `docs/HK_SUB2API_BLUE_GREEN_RUNBOOK.md` and the installed controller as the operational interface. If they disagree, stop and report the discrepancy.

## Acceptance and rollback

Verify health, restart count, exact digest and commit, affected API behavior, error rate, and streaming/tool-call behavior when relevant. Use the configured gate and observation window.

Before promotion, run a controller rollback dry-run when supported. On failure after cutover, roll back to `rollback_target_slot` through the controller, then verify traffic, health, worker ownership, and stream drainage.
