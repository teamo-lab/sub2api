---
name: deploy-prod
description: Deploy Sub2API to the 43 production server through its managed blue/green rollout, strictly from the latest teamo-lab/sub2api main commit. Use for production deploy, release, rollout, promotion, rollback, or checking whether production matches main; do not use for development environments or ordinary code changes.
---

# Deploy Sub2API Production

Target only `root@43.159.0.162` and `/opt/sub2api/deploy`. Another host is outside this skill.

Read [references/runbook.md](references/runbook.md) before any live deployment, promotion, completion, or rollback. Status/audit requests may use read-only checks without deployment authorization.

## Mandatory source gate

```bash
git remote get-url origin
git fetch origin main
main_sha="$(git rev-parse origin/main)"
git status --short --branch
```

Proceed only when `origin` is `https://github.com/teamo-lab/sub2api.git` (or its SSH equivalent). Build from an isolated checkout/archive of exactly `$main_sha`, without local overlays. Do not deploy `HEAD` merely because a local branch is named `main`.

Before staging, require:

- Build source SHA equals the freshly fetched `origin/main` SHA.
- Source contains no uncommitted or untracked overlay.
- Image/binary embeds that same full SHA.
- Candidate image is addressed by immutable `sha256:` digest.
- Any emergency patch running in production has first been merged to `main` through a PR.

Stop before changing production if any check fails. Never repair provenance by relabeling an old image.

## Authorization boundary

Read-only status, health, image, and commit checks are allowed for audit requests. Building, transferring, staging, shifting weights, updating the worker, completing, or rolling back requires an explicit user request to deploy or roll back production.

## Required workflow

Use the installed rollout controller and scripts; never manipulate HAProxy or the active slot directly.

1. Confirm stable is healthy at 100%, while candidate is healthy at 0% with no active streams.
2. Build once from the exact `origin/main` SHA and embed it. Use the same immutable image for all processes in the release.
3. Stage into the idle candidate at 0%.
4. Verify health, version, digest, embedded commit, database compatibility, and affected application paths.
5. Shift traffic through controller-managed gates. Bypass a failed gate only when the user explicitly authorizes that specific bypass with an auditable reason.
6. Update the singleton worker through the managed workflow; verify exactly one singleton owner.
7. Complete only after the requested acceptance window, retaining the former stable slot as rollback target until then.

On validation failure before promotion, keep stable traffic unchanged. After traffic has shifted, use the controller rollback target and let existing streams drain naturally.

## Report

Report host, exact `main` SHA, version, digest, slots and weights, worker image, validations, rollout result, and rollback target. Never claim “synced with main” from a version label alone.
