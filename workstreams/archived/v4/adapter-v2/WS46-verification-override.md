# WS46 — Verification override on every consuming command

**Phase:** Adapter v2 · **Track:** Signing completion (WS06 follow-up) · **Owner:** Workstream executor · **Depends on:** WS06, WS07, WS08. · **Unblocks:** WS47, WS48 (lets dev/CI proceed while signing is completed). · **Base branch:** `adapter-v2`

## Context

WS06 shipped a `signing.Policy` with three modes (`off` / `warn` / `strict`) and a `PullContext{ AllowUnsigned, WorkflowVerification }` resolver (`internal/adapter/signing/policy.go` `PolicyFor`). But the override is only wired into **`criteria adapter pull`** (`internal/cli/adapter_pull.go` exposes `--allow-unsigned` and populates `PullContext`). **`lock`, `compile`, and `apply` are hardwired to strict** — both `internal/cli/adapter_lock.go` and `internal/cli/adapter_autopull.go` call `signing.PolicyFor(signing.PullContext{})` with an empty context, so there is no flag/env/workflow way to relax verification for the operations that matter most during development.

Product decision (locked): **the unsigned override must always be available** — it is essential for local development and many CI flows. This WS makes the override uniform without weakening the secure default.

This is independent of *how* signatures are produced/verified (WS47 key mode, WS48 keyless bundle); it is purely the escape hatch and the per-workflow mode surface.

## Prerequisites

WS06/WS07/WS08 merged (present on `adapter-v2`).

## In scope

### Step 1 — One override resolver

Add a single helper (e.g. `internal/cli/verification.go`) that resolves the effective `signing.PullContext` from, in precedence order:

1. `--allow-unsigned` flag (highest) → `AllowUnsigned = true`.
2. `CRITERIA_ALLOW_UNSIGNED` env (`1`/`true`) → `AllowUnsigned = true`.
3. Workflow-level `verification = "off"|"warn"|"strict"` attribute (Step 3) → `WorkflowVerification`.
4. Default: `strict` (unchanged secure default).

`signing.PolicyFor` already honors both `AllowUnsigned` and `WorkflowVerification`; this WS only feeds it.

### Step 2 — Wire the flag into lock / apply / compile

- `internal/cli/adapter_lock.go` — add `--allow-unsigned`; replace `PolicyFor(PullContext{})` (currently ~line 111) with the resolver.
- `internal/cli/adapter_autopull.go` — the compile/apply auto-pull path; replace `PolicyFor(PullContext{})` (~line 62) with the resolver; thread the resolved context from the calling command.
- `internal/cli/apply*.go`, `internal/cli/compile.go` — add `--allow-unsigned` flags and pass through.
- Keep `internal/cli/adapter_pull.go` behavior; refactor it to use the shared resolver.

### Step 3 — Workflow-level `verification` attribute

- Add an optional `verification` string attribute to the workflow block in `workflow/schema.go` (`WorkflowSpec`), validated against `off|warn|strict` at compile.
- Surface it on the compiled graph so `adapter_autopull.go` can read it into `WorkflowVerification`.
- Document in `docs/adapters.md` (Environments/Signing section) and `docs/workflow.md`.

### Step 4 — Make `warn` the transition default (decision D-WS46-1)

Until WS47/WS48 land, set the *effective default* to `warn` (log, do not fail) so existing unsigned/legacy artifacts don't break `lock`/`apply`, while still surfacing the gap. Enterprise opts into `strict` via the workflow attr or a future global config. Record this as a dated decision in this file; revert to `strict` default once WS48 ships verifiable keyless.

> **Decision D-WS46-1 (2026-06-06):** The CLI transition default is `warn`,
> implemented as the single constant `transitionDefaultMode` in
> `internal/cli/verification.go`. `signing.PolicyFor`'s own secure default stays
> `strict`; the resolver injects `warn` only when no explicit override or workflow
> `verification` attribute is set. **Flip back to `strict`** by changing that one
> constant to `signing.ModeStrict`, in the WS48 Step 5 follow-up PR, once keyless
> is verifiable end-to-end and the real-OIDC CI integration job is green on
> `adapter-v2`.

## Out of scope

- Producing or verifying signatures correctly (WS47, WS48).
- Global/enterprise trust-config file (WS47).

## Behavior change

`lock`/`apply`/`compile` gain `--allow-unsigned` and honor `CRITERIA_ALLOW_UNSIGNED` and a workflow `verification` attribute. Default verification posture during the transition becomes `warn` (was effectively `strict`-but-unverifiable).

## Tests required

- `lock`/`apply`/`compile` with `--allow-unsigned` skip verification (unit, table-driven over the three commands).
- `CRITERIA_ALLOW_UNSIGNED` honored; precedence flag > env > workflow attr > default.
- Workflow `verification = "off"` parses, compiles, and disables verification; invalid value is a compile error.
- `strict` (explicit) still fails closed on an unsigned/unverifiable artifact.

## Exit criteria

- The override is reachable from `pull`, `lock`, `apply`, `compile` via flag + env + workflow attr.
- Secure default preserved (no silent downgrade beyond the documented `warn` transition default).
- Docs updated.

## Files this workstream may modify

- `internal/cli/verification.go` *(new)*, `internal/cli/adapter_lock.go`, `internal/cli/adapter_autopull.go`, `internal/cli/adapter_pull.go`, `internal/cli/apply*.go`, `internal/cli/compile.go`
- `internal/adapter/signing/policy.go` *(only if the resolver needs a new mode constant — unlikely)*
- `workflow/schema.go` *(workflow `verification` attribute)*, compiled-graph plumbing
- `docs/adapters.md`, `docs/workflow.md`

## Files this workstream may NOT edit

- `internal/adapter/signing/verify.go` (verification logic — WS47/WS48).
- `internal/adapter/publish/*` (signer — WS47/WS48).
