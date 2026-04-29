# Workstream 10 — Remove `CRITERIA_SHELL_LEGACY=1` escape hatch

**Owner:** Workstream executor · **Depends on:** none.

## Context

Phase 1 [W05](archived/v1/05-shell-adapter-sandbox.md) shipped the
shell-adapter sandbox with a time-boxed opt-out:
`CRITERIA_SHELL_LEGACY=1` disables the entire sandbox (env
allowlist, PATH sanitization, working-dir confinement, hard timeout,
output cap). The threat model
([docs/security/shell-adapter-threat-model.md:103-115](../docs/security/shell-adapter-threat-model.md#L103-L115))
explicitly commits to removing this in **v0.3.0**.

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
sections 4 and "What would move it back to MARGINAL") flags
**slipping the v0.3.0 removal** as a credibility risk:

> A regression on the `-race -count=1` test contract (any reintroduced flake).
> Shell sandbox legacy mode (CRITERIA_SHELL_LEGACY=1) is **not** removed in v0.3.0 as promised — that would establish a pattern of slipping security commitments.

This workstream honors the commitment. The legacy code path is
deleted; the env var is no longer recognized; tests that depended on
it are removed or rewritten; the threat model and `docs/plugins.md`
are updated; the `CHANGELOG.md` notes the breaking change (the
CHANGELOG itself is W14's territory; this workstream provides the
text in reviewer notes).

## Prerequisites

- `make ci` green on `main`.
- Familiarity with
  [internal/adapters/shell/sandbox.go](../internal/adapters/shell/sandbox.go)
  and
  [internal/adapters/shell/shell.go](../internal/adapters/shell/shell.go).
- Familiarity with the existing tests in
  [internal/adapters/shell/shell_sandbox_test.go](../internal/adapters/shell/shell_sandbox_test.go).

## In scope

### Step 1 — Delete legacy code paths

In [internal/adapters/shell/sandbox.go](../internal/adapters/shell/sandbox.go):

- Remove the `legacyEnvVar` constant
  ([line 21](../internal/adapters/shell/sandbox.go#L21)).
- Remove the `legacyMode()` (or equivalently named) helper
  ([around line 46](../internal/adapters/shell/sandbox.go#L46)).
- Remove every `if legacyMode() { ... }` branch. The sandbox defaults
  become unconditional.
- Remove the legacy-mode branch from working-directory validation
  ([around line 244 onward](../internal/adapters/shell/sandbox.go#L244)).
  The `add the path to CRITERIA_SHELL_ALLOWED_PATHS or set
  CRITERIA_SHELL_LEGACY=1 to disable confinement` error message
  drops the legacy-mode suggestion. New text:
  `add the path to CRITERIA_SHELL_ALLOWED_PATHS to allow it`.
- Update the package comment block at the top of the file (lines
  1-10) to remove the "All sandbox defaults are disabled when
  CRITERIA_SHELL_LEGACY=1" line. Replace with a one-line note that
  the legacy opt-out was removed in v0.3.0.

In [internal/adapters/shell/shell.go](../internal/adapters/shell/shell.go):

- Remove the package-comment lines 77-79 (the legacy-mode
  description).
- Remove the comment at line 97 about "In legacy mode without an
  explicit timeout attribute".
- Remove any `if legacyMode() { ... }` branches in this file.

If the `legacyMode()` helper is the only consumer of `os/exec`'s
`Getenv` for `CRITERIA_SHELL_LEGACY`, that import line cleans up
automatically. Run `goimports -w` after the deletions.

### Step 2 — Remove or rewrite legacy-mode tests

In [internal/adapters/shell/shell_sandbox_test.go](../internal/adapters/shell/shell_sandbox_test.go):

- Delete `TestSandbox_LegacyMode_*` tests (lines 357 onward;
  multiple tests use `t.Setenv("CRITERIA_SHELL_LEGACY", "1")`).
- Delete the `os.Unsetenv("CRITERIA_SHELL_LEGACY")` call at line 63
  (no longer needed since the env var is unrecognized).
- If any *non-legacy* test relied on a side effect of the legacy
  branch (unlikely but possible), rewrite to use the sandbox
  defaults.

After the deletion, run the test file in isolation to confirm no
references remain: `go test ./internal/adapters/shell/...`.

### Step 3 — Add a regression test asserting the env var is unrecognized

Add a new test:

```go
// TestSandbox_LegacyEnvVarIgnored asserts that CRITERIA_SHELL_LEGACY
// is no longer recognized after v0.3.0 removal (W10). Setting it has
// no effect on sandbox enforcement.
func TestSandbox_LegacyEnvVarIgnored(t *testing.T) {
    t.Setenv("CRITERIA_SHELL_LEGACY", "1")
    // Run a workflow that would have escaped sandboxing under the
    // legacy mode; assert it is still enforced.
    // For example: assert env allowlist is applied, PATH is
    // sanitized, working-dir confinement is enforced.
}
```

This test is the durable signal that the removal is real and stays
real. Pick a single observable check (env allowlist is the simplest)
and assert it under `CRITERIA_SHELL_LEGACY=1`.

### Step 4 — Update documentation

[docs/security/shell-adapter-threat-model.md](../docs/security/shell-adapter-threat-model.md):

- Lines 103-115 describe the legacy opt-out. Replace the section
  with:
  > **`CRITERIA_SHELL_LEGACY=1` was removed in v0.3.0** as committed
  > in the v0.2.0 threat model. Setting the env var has no effect.
  > The Phase 1 sandbox defaults are unconditional.
- Update the threat-mitigation table if any row references the
  legacy mode as an "operator escape hatch" — the row should now
  read "no escape hatch; always enforced".

[docs/plugins.md](../docs/plugins.md):

- Line 55 documents the env var. Remove that mention.
- Update the surrounding paragraph to make clear the security
  defaults are unconditional.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.
W14 handles the CHANGELOG entry; this workstream provides the
exact text in reviewer notes:

> ### Removed
>
> - **W10 — `CRITERIA_SHELL_LEGACY=1` removed.** The shell-adapter
>   legacy escape hatch is no longer recognized. Workflows that
>   previously set this env var to disable the v0.2.0 hardening must
>   migrate to explicit configuration (`CRITERIA_SHELL_ALLOWED_PATHS`
>   for working-directory confinement, the `env` and `command_path`
>   step inputs for environment passthrough, etc.). See
>   [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md)
>   for the unconditional sandbox semantics. This was committed as a
>   time-boxed removal in the v0.2.0 threat model.

### Step 5 — Validate

- `make build` succeeds.
- `make plugins` succeeds.
- `make test -race -count=2 ./internal/adapters/shell/...` green
  (with the legacy tests removed).
- `make test -race -count=2 ./...` green across all three modules.
- `make lint-go` green (no orphan imports left).
- `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go' .` returns zero
  matches in `internal/`, `cmd/`, `workflow/`, `sdk/`, `events/`.
  Matches in `tests/` are also zero. Matches in `docs/security/`
  remain only as historical references in the "removed in v0.3.0"
  paragraph.
- `make validate` green (no example workflow depends on legacy
  mode).
- `make ci` green.

## Behavior change

**Yes — breaking.**

- `CRITERIA_SHELL_LEGACY=1` no longer disables the sandbox. Any
  workflow that depends on the legacy mode breaks immediately and
  must migrate.
- The working-dir-not-allowed error message drops the legacy
  fallback suggestion.
- `goleak` should still be clean. The flake-watch lane stays green.

This is a **deliberate breaking change** committed in the v0.2.0
threat model. The CHANGELOG entry (provided by this workstream's
reviewer notes; written by [W14](14-phase2-cleanup-gate.md)) calls
this out under "Removed".

## Reuse

- Existing sandbox defaults — they were the production behavior all
  along; this workstream just removes the alternative path.
- Existing test harness in `shell_sandbox_test.go` — keep the
  non-legacy tests; remove the legacy ones.

## Out of scope

- Tightening the sandbox further (e.g. seccomp, sandbox-exec). That
  is Phase 4.
- Adding new sandbox configuration. The v0.2.0 sandbox API is fixed.
- Changes to the shell adapter's HCL surface
  (`command`, `env`, `command_path`, `timeout`,
  `output_limit_bytes`, `working_directory`). Unchanged.
- Migration tooling (e.g. a script that converts legacy-mode workflows
  to the new shape). Operators using legacy mode are expected to
  read the threat model and migrate.

## Files this workstream may modify

- `internal/adapters/shell/sandbox.go` (delete legacy paths;
  update package comment).
- `internal/adapters/shell/shell.go` (delete legacy comments and
  branches).
- `internal/adapters/shell/shell_sandbox_test.go` (delete
  `TestSandbox_LegacyMode_*` tests; add
  `TestSandbox_LegacyEnvVarIgnored`).
- Any other shell-package file that touches `legacyEnvVar` or the
  legacy helper (locate via grep before editing).
- `docs/security/shell-adapter-threat-model.md` (replace the
  escape-hatch section with the removal notice).
- `docs/plugins.md` (remove the env-var mention; update surrounding
  paragraph).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the shell adapter's HCL surface or its
`Info()` / schema responses.

## Tasks

- [ ] Delete `legacyEnvVar`, `legacyMode()`, and every legacy-mode
      branch in `sandbox.go` and `shell.go`.
- [ ] Update package-level comments to reflect the unconditional
      sandbox.
- [ ] Update the working-dir error message to drop the legacy
      suggestion.
- [ ] Delete `TestSandbox_LegacyMode_*` tests; add
      `TestSandbox_LegacyEnvVarIgnored` to lock in the removal.
- [ ] Update `docs/security/shell-adapter-threat-model.md` lines
      103-115 with the removal notice.
- [ ] Update `docs/plugins.md` line 55 (and surrounding paragraph)
      to drop the legacy mention.
- [ ] Provide the CHANGELOG "Removed" entry text in reviewer notes
      for [W14](14-phase2-cleanup-gate.md) to copy.
- [ ] `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go'` returns
      zero matches.
- [ ] `make ci` green.

## Exit criteria

- `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go' .` → zero
  matches.
- `grep -n 'CRITERIA_SHELL_LEGACY' docs/plugins.md` → zero matches.
- `grep -n 'CRITERIA_SHELL_LEGACY' docs/security/shell-adapter-threat-model.md`
  → matches only in the "removed" historical paragraph.
- `TestSandbox_LegacyEnvVarIgnored` passes.
- `make test -race -count=2 ./internal/adapters/shell/...` green.
- `make ci` green.
- The CHANGELOG entry text is in reviewer notes for W14 to consume.

## Tests

- New: `TestSandbox_LegacyEnvVarIgnored` (Step 3).
- Removed: `TestSandbox_LegacyMode_*` (Step 2).
- All other shell-adapter tests continue to pass unchanged.
- `goleak` continues to report clean.

## Risks

| Risk | Mitigation |
|---|---|
| A consumer outside this repo (orchestrator, custom plugin) depends on the legacy mode | Document the removal loudly in the CHANGELOG and the threat model. The threat model committed to this in v0.2.0; consumers had a release cycle to migrate. If a known consumer surfaces, treat it as a separate scoped exception — but do not delay the removal beyond v0.3.0. |
| The flake-watch lane regresses because some test relied on legacy-mode looseness for timing | The flake-watch tests don't exercise legacy mode. Run `make test-flake-watch` after the removal to confirm. If a flake surfaces, treat it as a Phase 1 W01 regression and remediate per W01's contract. |
| The grep verification produces false negatives (e.g. comment-only mention in a `.go` file) | The exit criteria explicitly require `grep -rn` to return zero matches in `*.go` files. Comment-only references should also be removed (since they would mislead a future reader). The threat model is the only place a historical reference is allowed. |
| Removing the env var leaves users with workflows that fail and no clear migration path | The error message changes and the threat model documents the migration. The CHANGELOG entry names the migration knobs explicitly (`CRITERIA_SHELL_ALLOWED_PATHS`, `env`, `command_path`). |
| Reviewer notes accidentally land in the wrong file | The CHANGELOG entry is provided in reviewer notes for W14's gate agent to copy. This workstream does not edit CHANGELOG.md directly — that constraint is hard. |
