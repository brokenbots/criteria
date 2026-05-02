# Workstream 4 — State directory permissions hardening

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W16](16-phase2-cleanup-gate.md) (cleanup gate verifies the perms).

## Context

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
section 4) flags two `os.MkdirAll(filepath.Dir(p), 0o755)` calls in
[internal/cli/local_state.go:74](../internal/cli/local_state.go#L74)
and [:129](../internal/cli/local_state.go#L129) as a minor security
finding. The token files written inside `~/.criteria/` are correctly
0o600, but the *directory* is world-readable, leaking run IDs and
workflow names to other local users via directory listing.

The threat model for the local state directory is operator-only: the
directory holds run IDs, workflow names, checkpoints, and (after
[W06](06-local-mode-approval.md) lands) approval decisions. None of
that should be visible to other UIDs on a shared host. The fix is a
trivial one-line change per call site, plus a regression test, plus
a small audit to confirm no other CLI code creates dirs at 0o755.

## Prerequisites

- `make ci` green on `main`.

## In scope

### Step 1 — Tighten the two cited call sites

In [internal/cli/local_state.go](../internal/cli/local_state.go):

- Line 74 (`writeLocalRunState`): change `0o755` → `0o700`.
- Line 129 (`WriteStepCheckpoint`): change `0o755` → `0o700`.

The intent is **operator-only access**: rwx for the operator, no
permissions for group or world.

### Step 2 — Audit the rest of the CLI for similar patterns

Run the following greps from repo root:

```sh
grep -rn 'MkdirAll' internal/ cmd/ workflow/ sdk/ events/
grep -rn 'os.Mkdir(' internal/ cmd/ workflow/ sdk/ events/
```

For every match:

1. If the directory holds operator-private state (checkpoints, tokens,
   run state), tighten to `0o700`.
2. If the directory holds shared / public artifacts (e.g. an example
   output dir, a build temp under `bin/`), `0o755` may be correct —
   document the rationale with a one-line code comment if the
   distinction is non-obvious.
3. The shell adapter's working-directory confinement code in
   [internal/adapters/shell/sandbox.go](../internal/adapters/shell/sandbox.go)
   creates no directories itself; ignore it.

Record the audit findings in reviewer notes: every match, its
file:line, the chosen mode, and the reason. This audit is the
deliverable — even if every other call site is already correct, the
audit itself confirms it.

### Step 3 — Regression test

Add a test to
[internal/cli/local_state_test.go](../internal/cli/local_state_test.go)
(create the file if it doesn't exist; use `t.TempDir()` and
override the state-dir resolver if `local_state.go` exposes one,
otherwise refactor minimally to enable the test).

The test must:

1. Set up a temp `HOME` (override via env var if `stateDir()` reads
   `$HOME`; otherwise inject via a test-only seam).
2. Call `writeLocalRunState` and `WriteStepCheckpoint`.
3. `os.Stat()` the directory and assert
   `info.Mode().Perm() == 0o700`.
4. `os.Stat()` the file inside and assert
   `info.Mode().Perm() == 0o600` (existing behavior — the test
   doubles as a regression guard for the file mode too).
5. Skip on Windows (POSIX-mode-bit assertions don't apply).

### Step 4 — No migration

Existing `~/.criteria/` directories on operator machines retain their
existing perms. The change applies to *new* directories only. This is
intentional: `chmod`-ing the user's home subtree without permission
is overreach. If the team wants a migration path, that is a separate,
opt-in workstream — out of scope here.

Document this explicitly in the CHANGELOG (handled by
[W16](16-phase2-cleanup-gate.md), but flag it in reviewer notes so
the gate does not miss it).

### Step 5 — Validate

- `make test -race -count=2 ./internal/cli/...` green.
- `make ci` green.
- Manual: on a fresh machine (or after `rm -rf ~/.criteria`), run any
  command that writes state (e.g. `criteria apply <local workflow>`)
  and confirm `stat ~/.criteria` reports `drwx------`.

## Behavior change

**Yes, but minor and forward-only.**

- New invocations create `~/.criteria/` and `~/.criteria/runs/` at
  mode `0o700` instead of `0o755`.
- Existing directories retain their existing mode (no migration).
- File modes inside (`0o600`) are unchanged.
- Public CLI surface, HCL surface, events, and logs are unchanged.
- A subtle behavioral effect: if another tool on the same machine was
  reading from `~/.criteria/` under a different UID (no known
  consumer, but theoretically possible), it would now be denied. This
  is the intended hardening; document in reviewer notes if any such
  consumer surfaces during audit.

## Reuse

- Existing `stateDir()` and `stateFilePath()` helpers in
  [internal/cli/local_state.go](../internal/cli/local_state.go) — do
  not duplicate.
- The `t.TempDir()` pattern used elsewhere in the test suite.

## Out of scope

- Migrating existing `~/.criteria/` directories to `0o700`.
- Changing the file modes (already 0o600).
- Adding ACLs or extended attributes.
- Tightening other directories the CLI does not own (e.g.
  `${CRITERIA_PLUGINS}`).
- Windows-specific permission semantics.

## Files this workstream may modify

- `internal/cli/local_state.go` (two-line change at lines 74 and
  129).
- `internal/cli/local_state_test.go` (new or extended).
- Any other CLI file flagged by the Step 2 audit (with documented
  rationale).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Change `0o755` → `0o700` at `local_state.go:74` and `:129`.
- [x] Audit all `MkdirAll` / `Mkdir` call sites; document findings.
- [x] Tighten any additional sites that hold operator-private state.
- [x] Add regression test asserting `0o700` on the state dir and
      `0o600` on files inside.
- [x] Skip the test on Windows.
- [x] Manual verification on a fresh `~/.criteria` directory.
- [x] `make ci` green.

## Exit criteria

- `internal/cli/local_state.go:74` and `:129` use `0o700`.
- The audit from Step 2 is complete and documented in reviewer notes.
- The regression test in `local_state_test.go` passes and asserts
  the directory mode is `0o700`.
- Manual `stat ~/.criteria` on a fresh state dir reports
  `drwx------`.
- `make test -race -count=2 ./internal/cli/...` green.
- `make ci` green.

## Tests

- New: `TestStateDirPerms` (or similarly named) in
  `internal/cli/local_state_test.go`. Exercises both
  `writeLocalRunState` and `WriteStepCheckpoint`. Asserts dir mode
  `0o700` and file mode `0o600`.
- Existing tests must pass unchanged.

## Reviewer Notes

### Step 1 — Call-site changes

- `internal/cli/local_state.go:74` (`writeLocalRunState`): `0o755` → `0o700`. ✓
- `internal/cli/local_state.go:129` (`WriteStepCheckpoint`): `0o755` → `0o700`. ✓

### Step 2 — Audit findings

Every `MkdirAll` / `Mkdir` call in `internal/`, `cmd/`, `workflow/`, `sdk/`, `events/`:

| File:line | Mode | Verdict |
|---|---|---|
| `internal/cli/local_state.go:74` | `0o700` (was `0o755`) | **Fixed** — operator-private state dir |
| `internal/cli/local_state.go:129` | `0o700` (was `0o755`) | **Fixed** — operator-private runs subdir |
| `internal/cli/local_state_test.go:92` | `0o755` | OK — test scaffold (temp dir helper, not the production path being tested) |
| `internal/cli/local_state_test.go:235` | `0o755` | OK — test scaffold (temp dir helper) |
| `internal/cli/local_state_test.go:240` | `0o755` | OK — test scaffold: `os.Mkdir` creates a fake subdirectory inside the test runs dir to verify that `ListStepCheckpoints` silently skips directories; not operator state |
| `internal/cli/compile_test.go:92` | `0o755` | OK — test-only temp path for HCL fixture |
| `internal/cli/reattach_test.go:82` | `0o755` | OK — test-only temp dir |
| `internal/plugin/discovery_test.go:27,30,52` | `0o755` | OK — plugin dirs hold public binaries; world-readable is correct (plugin discovery by filename) |
| `internal/adapters/shell/shell_sandbox_test.go:170` | `0o755` | OK — test-only temp bin dir |
| `workflow/eval_functions_test.go:196,199,276,303,306,330,333` | `0o755` | OK — test-only temp workflow dirs; not operator state |

No additional production call sites require tightening.

### Step 3 — Regression test

`TestStateDirPerms` added to `internal/cli/local_state_test.go`:
- Uses `filepath.Join(t.TempDir(), "state")` (non-existent subdir) as `CRITERIA_STATE_DIR`
  so `os.MkdirAll` creates it fresh and mode assertion is valid.
- Calls `writeLocalRunState` → asserts `dir` mode `0o700` and `criteria-state.json` mode `0o600`.
- Calls `WriteStepCheckpoint` → asserts `runs/` mode `0o700` and checkpoint file mode `0o600`.
- Skips on `runtime.GOOS == "windows"`.

### Step 4 — No migration

Existing `~/.criteria/` directories retain their prior mode. The change applies
only to *newly created* directories. CHANGELOG entry is deferred to W16 (cleanup gate; renumbered from W14 on 2026-04-30) as planned.

### Step 5 — Validation

- `go test -race -count=2 ./internal/cli/...`: ✓ PASS
- `make ci`: ✓ PASS (the one intermittent failure in `internal/plugin/TestHandshakeInfo`
  is a pre-existing plugin startup race; confirmed by running the test on unmodified main —
  it passes on retry and is unrelated to this workstream).
- Manual: `CRITERIA_STATE_DIR=/tmp/criteria-perm-test bin/criteria apply examples/hello.hcl`
  → `stat /tmp/criteria-perm-test` reports `drwx------`. ✓

### CHANGELOG note for W16 (cleanup gate)

W16 (renumbered from W14 on 2026-04-30) must add a note under the v0.2.x section:
> New invocations create `~/.criteria/` and `~/.criteria/runs/` at mode `0700` (operator-only).
> Existing directories are not migrated. To tighten an existing installation: `chmod 700 ~/.criteria`.

### Review 2026-04-29 — changes-requested

#### Summary
The implementation itself is correct: both production `MkdirAll` call sites now use `0o700`, the new regression test exercises both write paths and asserts `0o700` on directories plus `0o600` on files, and explicit CLI/manual validation succeeds. Approval is blocked on one workstream-deliverable gap: the Step 2 audit table is incomplete, so the workstream does not yet satisfy the requirement to document every `MkdirAll` / `os.Mkdir` match.

#### Plan Adherence
- Step 1: Met. `internal/cli/local_state.go:74` and `internal/cli/local_state.go:129` now use `0o700`.
- Step 2: Not yet met. The recorded audit omits one grep hit: `internal/cli/local_state_test.go:240` (`os.Mkdir(..., 0o755)`), so the required "every match, file:line, chosen mode, and reason" deliverable is incomplete.
- Step 3: Met. `TestStateDirPerms` covers both `writeLocalRunState` and `WriteStepCheckpoint`, skips on Windows, and asserts directory `0o700` plus file `0o600`.
- Step 4: Met. No migration behavior was introduced.
- Step 5: Validation passed, but the workstream cannot be approved until the Step 2 audit is complete.

#### Required Remediations
- **Blocker** — `internal/cli/local_state_test.go:240` is missing from the Step 2 audit recorded above. The workstream explicitly requires every `MkdirAll` / `os.Mkdir` match from the prescribed grep set to be documented with file:line, mode, and reason. **Acceptance:** add the missing `internal/cli/local_state_test.go:240` entry to the audit table with its `0o755` rationale (test-only scaffold), then re-check the table against the grep output so all matches are accounted for.

#### Test Intent Assessment
`TestStateDirPerms` is appropriately behavior-focused: it forces fresh directory creation, exercises both production writers, and asserts the externally meaningful permission bits on both directories and files. A faulty implementation that left either production directory at `0o755` would fail this test. I did not find additional test gaps for this scope.

#### Validation Performed
- `rg -n 'MkdirAll\(|os\.Mkdir\(' internal cmd workflow sdk events --glob '*.go'`: found 18 matches; the recorded audit covers 17 and omits `internal/cli/local_state_test.go:240`.
- `go test -race -count=2 ./internal/cli/...`: passed.
- `make ci`: passed.
- Manual: `CRITERIA_STATE_DIR=<fresh tmpdir>/state bin/criteria apply examples/hello.hcl` created the state directory as `drwx------`.

### Review 2026-04-29-02 — approved

#### Summary
Approved. The resubmission closes the only blocker from the previous review by documenting the missing `internal/cli/local_state_test.go:240` `os.Mkdir` call in the Step 2 audit table. With that audit gap fixed, the implementation, tests, and validation now satisfy the workstream scope and exit criteria.

#### Plan Adherence
- Step 1: Met. `internal/cli/local_state.go:74` and `internal/cli/local_state.go:129` use `0o700`.
- Step 2: Met. The audit now accounts for all 18 `MkdirAll` / `os.Mkdir` matches in `internal/`, `cmd/`, `workflow/`, `sdk/`, and `events/`, with mode and rationale recorded for each relevant line or grouped set.
- Step 3: Met. `TestStateDirPerms` still exercises both write paths, skips on Windows, and asserts `0o700` for directories plus `0o600` for files.
- Step 4: Met. Existing directories are unchanged; no migration behavior was added.
- Step 5: Met. Targeted tests, full `make ci`, and the fresh-state-dir manual check all succeeded.

#### Test Intent Assessment
The regression coverage remains appropriately behavior-based and regression-sensitive. The permission test proves the operator-only directory creation contract at both production write sites and would fail on a reversion to `0o755`; the surrounding existing tests continue to cover checkpoint listing and local-state behavior without diluting this workstream’s intent.

#### Validation Performed
- `rg -n 'MkdirAll\(|os\.Mkdir\(' internal cmd workflow sdk events --glob '*.go'`: confirmed 18 total matches, all now reflected in the Step 2 audit.
- `go test -race -count=2 ./internal/cli/...`: passed.
- `make ci`: passed.
- Manual: `CRITERIA_STATE_DIR=<fresh tmpdir>/state bin/criteria apply examples/hello.hcl` created the state directory as `drwx------`.
