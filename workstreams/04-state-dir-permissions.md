# Workstream 4 — State directory permissions hardening

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W14](14-phase2-cleanup-gate.md) (cleanup gate verifies the perms).

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
[W14](14-phase2-cleanup-gate.md), but flag it in reviewer notes so
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

- [ ] Change `0o755` → `0o700` at `local_state.go:74` and `:129`.
- [ ] Audit all `MkdirAll` / `Mkdir` call sites; document findings.
- [ ] Tighten any additional sites that hold operator-private state.
- [ ] Add regression test asserting `0o700` on the state dir and
      `0o600` on files inside.
- [ ] Skip the test on Windows.
- [ ] Manual verification on a fresh `~/.criteria` directory.
- [ ] `make ci` green.

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

## Risks

| Risk | Mitigation |
|---|---|
| The test cannot easily redirect `$HOME` because `stateDir()` reads it once and caches | Use a small refactor that allows a test-only injection of the state-dir root; do not change the production code path. If the existing helper already supports a `CRITERIA_STATE_DIR` env var, prefer that. |
| An operator's existing `~/.criteria/` is unaffected and continues at `0o755` | Document explicitly in CHANGELOG (W14). The forward-only stance is deliberate. |
| The audit (Step 2) finds a case where 0o755 was intentional and required | Document the rationale in a code comment and skip the change for that site. The audit is about *finding* the cases; not all of them require fixes. |
| `os.MkdirAll` umask interaction reduces the actual mode below 0o700 | `os.MkdirAll` applies `mode & ~umask`; on the typical operator umask (`0o022`) the final mode is `0o700 & ~0o022 = 0o700`. On unusual umasks (e.g. `0o077`) the mode could be tighter, which is acceptable. The test asserts `>=` semantics? No — assert exact `0o700`; if a tight umask produces `0o600` the test will fail and the operator can investigate. |
