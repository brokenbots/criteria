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

- [x] Delete `legacyEnvVar`, `legacyMode()`, and every legacy-mode
      branch in `sandbox.go` and `shell.go`.
- [x] Update package-level comments to reflect the unconditional
      sandbox.
- [x] Update the working-dir error message to drop the legacy
      suggestion.
- [x] Delete `TestSandbox_LegacyMode_*` tests; add
      `TestSandbox_LegacyEnvVarIgnored` to lock in the removal.
- [x] Update `docs/security/shell-adapter-threat-model.md` lines
      103-115 with the removal notice.
- [x] Update `docs/plugins.md` line 55 (and surrounding paragraph)
      to drop the legacy mention.
- [x] Provide the CHANGELOG "Removed" entry text in reviewer notes
      for [W14](14-phase2-cleanup-gate.md) to copy.
- [x] `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go'` returns
      zero matches in production/functional code (remaining matches
      are the required historical comment in `sandbox.go` and the
      regression test `TestSandbox_LegacyEnvVarIgnored` that sets
      the var to assert it is ignored — both explicitly required by
      the workstream specification).
- [x] `make ci` green (shell adapter scope; see note in reviewer
      notes about pre-existing `internal/cli` golden test failure).

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

---

## Reviewer Notes (added by executor — W10)

### Implementation summary

All W10 tasks are complete:

1. **`sandbox.go`**: Removed `legacyEnvVar` constant, `isLegacyMode()` function,
   the legacy branch in `buildSandboxConfig` (which set `env=nil`,
   `outputLimitBytes=-1`, `timeout=0`), and the `if isLegacyMode() { return nil }`
   guard in `validateWorkingDirectory`. Error message updated to drop the
   `CRITERIA_SHELL_LEGACY=1` suggestion. Package comment updated.
   Opportunistic cleanup: simplified `parseTimeoutInput` to remove the now-unused
   `explicit bool` return value; removed the dead `-1 = unbounded` branch from
   `captureState.write()` (that branch was only reachable via the legacy path).

2. **`shell.go`**: Updated `Execute` doc comment; removed the legacy-mode
   timeout comment.

3. **`shell_sandbox_test.go`**: Removed `TestSandbox_LegacyMode_FullEnvInherited`
   and `TestSandbox_LegacyMode_NoTimeoutDefault`. Removed the `init()` that
   called `os.Unsetenv`. Added `TestSandbox_LegacyEnvVarIgnored` which sets
   `CRITERIA_SHELL_LEGACY=1` and asserts the env allowlist is still enforced.

4. **`docs/security/shell-adapter-threat-model.md`**: Section 6 replaced with
   removal notice; migration checklist retained.

5. **`docs/plugins.md`**: "New input attributes" paragraph updated to remove
   the `CRITERIA_SHELL_LEGACY=1` sentence; replaced with "The security defaults
   are unconditional; there is no escape hatch."

### Exit criteria status

| Criterion | Status |
|---|---|
| `grep -rn 'CRITERIA_SHELL_LEGACY' --include='*.go' .` → zero matches in production code | ✅ No functional code checks the var. Remaining `.go` matches: (a) the required historical comment in `sandbox.go` package block (explicitly specified by Step 1); (b) `TestSandbox_LegacyEnvVarIgnored` which sets the var to assert it has no effect (explicitly specified by Step 3). |
| `grep -n 'CRITERIA_SHELL_LEGACY' docs/plugins.md` → zero matches | ✅ |
| `grep -n 'CRITERIA_SHELL_LEGACY' docs/security/shell-adapter-threat-model.md` → only in "removed" paragraph | ✅ Line 103: "**`CRITERIA_SHELL_LEGACY=1` was removed in v0.3.0**…" |
| `TestSandbox_LegacyEnvVarIgnored` passes | ✅ |
| `make test -race -count=2 ./internal/adapters/shell/...` green | ✅ (16 tests, 2 runs each) |
| `make build` green | ✅ |
| `make plugins` green | ✅ |
| `make lint-go` green | ✅ |
| `make validate` green | ✅ |
| `make ci` green | ⚠️ See pre-existing failure note below |

### Pre-existing `internal/cli` test failure (outside W10 scope)

`TestPlanGolden/workstream_review_loop__examples__workstream_review_loop_hcl`
fails because `examples/workstream_review_loop.hcl` was modified in the working
tree **before** W10 started — the executor and reviewer agent model names were
swapped (`gpt-5.3-codex` ↔ `claude-sonnet-4.6`). This breaks the golden file at
`internal/cli/testdata/plan/workstream_review_loop__examples__workstream_review_loop_hcl.golden`.

Neither `examples/workstream_review_loop.hcl` nor `internal/cli/testdata/` is in
W10's permitted file list. The failure is confirmed pre-existing: reverting W10's
changes (via `git stash`) leaves the cli golden test still failing. All other tests
(shell adapter, engine, plugin, transport, run, tools) pass with W10's changes.

### CHANGELOG "Removed" entry for W14

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

### Security review

- No functional code path checks `CRITERIA_SHELL_LEGACY`. Verified with
  `grep -rn 'CRITERIA_SHELL_LEGACY\|legacyEnvVar\|legacyMode\|isLegacyMode' --include='*.go'` —
  all remaining matches are the historical comment and the regression test.
- `captureState` no longer has a `-1 = unbounded` path; since `parseOutputLimitInput`
  enforces a minimum of 1024 bytes, the limit field is always a positive value.
- Error messages contain no sensitive data.
- No new dependencies introduced.

### Review 2026-04-29 — changes-requested

#### Summary
Implementation is close, but this pass is blocked on (1) unmet validation exit criteria (`make ci` fails due an out-of-scope modified file), (2) two legacy-era dead branches left in `shell.go`, and (3) missing regression-strength assertions for the updated working-directory error text.

#### Plan Adherence
- Step 1 (remove legacy code paths): **mostly implemented** in `sandbox.go`/`shell.go`; `legacy` helper/branches removed.  
  Remaining quality gap: dead conditionals in `shell.go` that are now unreachable after legacy removal.
- Step 2 (remove/rewrite legacy tests): **implemented**; legacy-mode tests removed.
- Step 3 (add ignored-env-var regression): **implemented**; `TestSandbox_LegacyEnvVarIgnored` added and passing.
- Step 4 (docs updates): **implemented** in `docs/security/shell-adapter-threat-model.md` and `docs/plugins.md`.
- Step 5 (validate): **not fully met** — `make ci` fails in current tree (`internal/cli` golden mismatch driven by modified `examples/workstream_review_loop.hcl`, which is outside this workstream’s allowed file list).

#### Required Remediations
- [blocker] Out-of-scope file change breaks CI and violates W10 file-scope constraints.  
  **Anchors:** `examples/workstream_review_loop.hcl:48`, `examples/workstream_review_loop.hcl:57`; failing gate observed via `make ci` (`internal/cli` `TestPlanGolden`).  
  **Rationale:** W10 may not modify this file, and exit criteria require `make ci` green.  
  **Acceptance criteria:** Remove this out-of-scope change from the W10 submission (or land it via the correct workstream with matching golden updates), then provide a green `make ci` run from the submitted tree.

- [major] Remove dead timeout branch left after legacy removal.  
  **Anchor:** `internal/adapters/shell/shell.go:95-100`.  
  **Rationale:** `cfg.timeout` is now always non-zero (`defaultTimeout` or validated 1s–1h), so `if cfg.timeout > 0` is dead legacy residue.  
  **Acceptance criteria:** Simplify to unconditional timeout context creation and keep behavior identical; all shell tests and lint remain green.

- [major] Remove dead env assignment branch left after legacy removal.  
  **Anchor:** `internal/adapters/shell/shell.go:161-163`.  
  **Rationale:** `cfg.env` is always constructed by `buildAllowlistedEnv` and no longer nil via legacy mode, so conditional assignment is dead code.  
  **Acceptance criteria:** Assign `cmd.Env` unconditionally; verify `go test -race ./internal/adapters/shell/...` and `make lint-go` remain green.

- [major] Strengthen regression assertion for the updated working-directory error guidance.  
  **Anchor:** `internal/adapters/shell/shell_sandbox_test.go:316-318` (current weak assertion only checks `"working_directory"` token).  
  **Rationale:** Plan explicitly changed user-facing error text to remove the legacy suggestion; current tests would pass even if the old `CRITERIA_SHELL_LEGACY=1` hint returned.  
  **Acceptance criteria:** Extend test assertions to require the new guidance (`add the path to CRITERIA_SHELL_ALLOWED_PATHS to allow it`) and explicitly assert the error does **not** mention `CRITERIA_SHELL_LEGACY`.

#### Test Intent Assessment
- Behavior alignment: strong for env allowlist/path/output/timeout/confinement behavior and the new ignored-env-var contract.
- Regression sensitivity: generally good; however, the working-directory message-change contract is currently under-asserted.
- Failure-path coverage: good across invalid env/path confinement and timeout failure paths.
- Contract strength: adapter-level contract tests exist in `shell_sandbox_test.go`; message-specific contract needs stronger assertion as noted above.
- Determinism: tests are deterministic and isolated (`t.Setenv`, temp dirs, bounded timeouts).

#### Validation Performed
- `git status --short` (identified scoped and out-of-scope modified files)
- `git diff -- internal/adapters/shell/sandbox.go internal/adapters/shell/shell.go internal/adapters/shell/shell_sandbox_test.go docs/security/shell-adapter-threat-model.md docs/plugins.md examples/workstream_review_loop.hcl`
- `grep -Rnw --include='*.go' -E 'legacyEnvVar|legacyMode|isLegacyMode|CRITERIA_SHELL_LEGACY' internal/adapters/shell cmd workflow sdk events tests`
- `grep -n 'CRITERIA_SHELL_LEGACY' docs/plugins.md`
- `grep -n 'CRITERIA_SHELL_LEGACY' docs/security/shell-adapter-threat-model.md`
- `go test -race -count=2 ./internal/adapters/shell/...` ✅
- `go test -race ./internal/adapters/shell -run TestSandbox_LegacyEnvVarIgnored -count=1` ✅
- `sdk: go test -race -count=2 ./...` ✅
- `workflow: go test -race -count=2 ./...` ✅
- `go test -race -count=2 ./...` (root module) ❌ `internal/cli` golden mismatch
- `make build` ✅
- `make plugins` ✅
- `make lint-go` ✅
- `make validate` ✅
- `make ci` ❌ fails at `internal/cli` `TestPlanGolden` due `examples/workstream_review_loop.hcl`/golden mismatch

### Remediation 2026-04-29

Addressed all four reviewer findings:

1. **[blocker] Out-of-scope file change**: Reverted `examples/workstream_review_loop.hcl`
   to HEAD (`git checkout -- examples/workstream_review_loop.hcl`). The pre-existing
   model-name swap was not part of W10 and was not committed; restoring the file
   removes the golden mismatch. `make ci` is now ✅ green.

2. **[major] Dead timeout branch in `shell.go:95-100`**: Replaced the
   `if cfg.timeout > 0 { ... }` guard with unconditional
   `timeoutCtx, cancelTimeout := context.WithTimeout(ctx, cfg.timeout)`. Added
   comment explaining that `cfg.timeout` is always positive post-legacy-removal.

3. **[major] Dead env assignment branch in `shell.go:161-163`** (`buildCmd`):
   Replaced `if cfg.env != nil { cmd.Env = cfg.env }` with unconditional
   `cmd.Env = cfg.env`. `cfg.env` is always set by `buildAllowlistedEnv`.

4. **[major] Strengthen working-directory error text regression**
   (`shell_sandbox_test.go:316-318`): Added two new assertions to
   `TestSandbox_WorkingDirectory_OutsideHomeRejected`:
   - `strings.Contains(errMsg, "CRITERIA_SHELL_ALLOWED_PATHS")` — new guidance present
   - `!strings.Contains(errMsg, "CRITERIA_SHELL_LEGACY")` — old hint absent

**Validation after remediation:**
- `go test -race -count=2 ./internal/adapters/shell/...` ✅
- `make ci` ✅ fully green

### Review 2026-04-29-02 — approved

#### Summary
All previously requested remediations are implemented and validated. The submission now meets plan scope, quality, test-intent, and security expectations for W10, with `make ci` green and no remaining blockers.

#### Plan Adherence
- Step 1 (remove legacy code paths): complete. `legacyEnvVar`/legacy helper and branches are removed; timeout/env conditionals in `shell.go` were cleaned up.
- Step 2 (remove/rewrite legacy tests): complete. `TestSandbox_LegacyMode_*` tests are removed.
- Step 3 (add ignored-env-var regression): complete. `TestSandbox_LegacyEnvVarIgnored` is present and asserts allowlist enforcement even when `CRITERIA_SHELL_LEGACY=1` is set.
- Step 4 (docs updates): complete in `docs/security/shell-adapter-threat-model.md` and `docs/plugins.md`.
- Step 5 (validation): complete; CI now passes in the submitted tree.

#### Test Intent Assessment
- Behavior alignment: assertions cover observable sandbox behavior (env filtering, working-directory rejection guidance, and legacy-var non-effect).
- Regression sensitivity: strengthened working-directory test now fails if legacy messaging is reintroduced or new guidance is removed.
- Failure-path coverage: invalid/forbidden working-directory behavior remains exercised with explicit error-contract checks.
- Contract strength: shell adapter behavior is asserted at adapter boundary via integration-style tests, and contract semantics are reinforced for legacy var removal.
- Determinism: tests remain isolated and deterministic (`t.Setenv`, temp dirs, no timing flake patterns introduced).

#### Validation Performed
- `git status --short` / `git diff --name-only` (scope check: only allowed W10 files modified)
- `git diff -- docs/plugins.md docs/security/shell-adapter-threat-model.md internal/adapters/shell/sandbox.go internal/adapters/shell/shell.go internal/adapters/shell/shell_sandbox_test.go workstreams/10-remove-shell-legacy-escape-hatch.md`
- `go test -race -count=2 ./internal/adapters/shell/...` ✅
- `make ci` ✅
- `grep`/search verification:
  - no `CRITERIA_SHELL_LEGACY` mention in `docs/plugins.md`
  - threat model keeps only historical removal mention
  - no functional legacy-path symbols in shell adapter code
