# engine: emit adapter image_reference on provision_wanted

> Linear filing blocked 2026-09-23: workspace at the free-plan active-issue cap
> (USAGE_LIMIT_EXCEEDED, metric activeIssueCount). File this when a slot frees.
> Everything below is the ready-to-paste ticket.

Repository: https://github.com/brokenbots/criteria
Plan: CRI-214 M14 (operator routing companion). Blocks the workflow-example
operator change that removes the hardcoded adapter-image map (same window as
CRI-259's develop scope).

## Context

The k8s operator (workflow-example criteria-k8s) currently invents adapter
images: jobbuilder `adapterImage(kind)` returns hardcoded
`localhost:5000/criteria-adapter-<kind>:<tag>` strings, discarding what the
workflow actually pinned. The `provision_wanted` lifecycle event already
carries the lockfile digest but NOT the image reference.

## Change

Extend the adapter lifecycle `provision_wanted` payload to carry the
workflow-declared adapter image reference (the lockfile entry's `reference`
field, e.g. `ghcr.io/brokenbots/criteria-adapter-shell:0.5.3`) verbatim,
alongside the existing digest.

- Emission site: the runner's adapter lifecycle event publisher, where
  `digest` is set today.
- Wire: additive field on the event payload data (no field numbers move).
- Companion operator change (workflow-example side, direct edit per standing
  rule): registry host becomes operator config; the k8s fat-image tag mapping
  (`k8s-<ver>`) becomes operator config or a per-workflow setting; the
  hardcoded per-kind map is deleted. Per-workflow override rides the routes
  ConfigMap's workflow object (same surface as volumes/secrets/env), matching
  the job-arch precedent.
- Trust posture unchanged: the operator never trusts the reference for
  policy — it still verifies the digest before any session opens.

## Acceptance

- `provision_wanted` payload includes `image_reference` for engines whose
  lockfile adapter pins carry one.
- Regression test: the emitter includes the reference exactly as the lockfile
  declares it.
- Older engines (no field) keep working: the operator falls back to its
  configured default mapping (companion operator ticket).