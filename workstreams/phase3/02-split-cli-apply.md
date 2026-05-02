# Workstream 02 — Split `internal/cli/apply.go` into focused files

**Phase:** 3 · **Track:** A · **Owner:** Workstream executor · **Depends on:** none (pre-rework cleanup; can interleave with [01](01-lint-baseline-burndown.md)). · **Unblocks:** [04-server-mode-coverage.md](04-server-mode-coverage.md) (server tests need the file split first), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) (the `CompileWithOpts` call site at L412 is where `SubWorkflowResolver` wires in; cleaner to wire after the split).

## Context

[internal/cli/apply.go](../../internal/cli/apply.go) is 728 LOC and contains four orthogonal concerns: local apply orchestration, server-mode apply orchestration, local pause/resume orchestration, and shared compile/setup helpers. [TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §2 calls this out as a maintainability item; §3 calls out the server-mode coverage hole (0% on `executeServerRun`, `runApplyServer`, `setupServerRun`, `drainResumeCycles`).

This is a **pure code-motion** workstream. No symbol renames, no signature changes, no behavior changes. The goal is to separate the concerns so [04](04-server-mode-coverage.md) can drop a fake-server harness against a smaller, focused file, and [13](13-subworkflow-block-and-resolver.md) can wire `SubWorkflowResolver` into `compileForExecution` without scrolling 600 lines of unrelated code.

## Prerequisites

- Phase 2 closed at `v0.2.0`.
- `make ci` green on `main`.

## In scope

### Step 1 — Carve the file

Move functions from [internal/cli/apply.go](../../internal/cli/apply.go) into the four new files below. Keep `package cli`. Imports follow the symbols. The `applyOptions` struct, `NewApplyCmd`, and `runApply` (the dispatcher) **stay in [apply.go](../../internal/cli/apply.go)**.

| New file | Functions to move | Rationale |
|---|---|---|
| `internal/cli/apply_local.go` | `runApplyLocal` (L86), `resumeLocalInFlightRuns` (L621), `prepareReattach` (L641), `resumeOneLocalRun` (L665), `buildReattachTrackerAndEngine` (L702) | Local-mode entry path + reattach |
| `internal/cli/apply_server.go` | `executeServerRun` (L257), `drainResumeCycles` (L300), `runApplyServer` (L332), `setupServerRun` (L353), `applyClientOptions` (L178), `buildServerSink` (L232) | Server-mode entry path + transport setup |
| `internal/cli/apply_resume.go` | `pauseTracker` type + all its methods (L444–L490), `buildLocalResumer` (L494), `drainLocalResumeCycles` (L523), `resolveLocalPause` (L552), `ensureLocalModeSupported` (L588) | Pause/resume orchestration shared by local mode |
| `internal/cli/apply_setup.go` | `compileForExecution` (L399), `newLocalRunState` (L247), `newApplyLogger` (L174), `writeRunCheckpoint` (L188), `buildLocalCheckpointFn` (L210), `localRunState` type (find via grep) | Construction / setup helpers consumed by both modes |

Keep in [internal/cli/apply.go](../../internal/cli/apply.go):

- `applyOptions` struct (L31).
- `NewApplyCmd` (L47).
- `runApply` (L76) — the dispatcher between local and server.

After the split, [apply.go](../../internal/cli/apply.go) should be ≤ 100 LOC.

### Step 2 — Preserve `//nolint` annotations and exception comments

The existing `//nolint:funlen // W03: ...` on `runApplyLocal` (L86) moves with the function into [apply_local.go](../../internal/cli/apply_local.go) verbatim. **Do not retag** the comment from `W03` to a Phase 3 workstream — the historical attribution is part of the audit trail. If the function complexity drops below the linter threshold post-split, remove the `//nolint` comment entirely (preferred outcome) — but do not modify the comment text.

Same rule for any other `//nolint` comments in functions that move.

### Step 3 — Update intra-package references

Functions in the same package (`cli`) that reference the moved symbols continue to work without import changes. Verify by running:

```sh
go build ./internal/cli/...
```

If a build error surfaces, it indicates a moved function referenced an unexported helper that did not move with it. Move the helper too (prefer keeping helpers next to their primary caller).

### Step 4 — Update test files

Tests live alongside the moved functions. The current shape of [internal/cli/apply_test.go](../../internal/cli/apply_test.go) (and any `*_test.go` siblings) covers the local path. Inventory the tests:

```sh
grep -ln 'runApplyLocal\|runApplyServer\|executeServerRun\|drainResumeCycles\|setupServerRun\|drainLocalResumeCycles\|resolveLocalPause\|compileForExecution\|resumeOneLocalRun' internal/cli/*_test.go
```

For each test file, decide whether it covers a single moved function (move the test alongside that function) or multiple (leave it in [apply_test.go](../../internal/cli/apply_test.go)).

**Do not rename tests.** Test names are part of CI's stable surface; keep `TestRunApplyLocal_...`, `TestPauseTracker_...`, etc. exactly as-is. Move them to a new file if appropriate but never rename.

### Step 5 — Validation

```sh
go build ./internal/cli/...
go test -race -count=2 ./internal/cli/...
make lint-go
make lint-baseline-check
make ci
```

All exit 0. The `lint-baseline-check` gate is critical: a code-motion workstream **must not** introduce a single new baseline entry. If `funlen` / `gocognit` / `gocyclo` measurements move (a moved function might cross a threshold that the original file masked via aggregation), the executor must adjust the function's structure (extract an obvious helper, no semantic change) — never add a baseline entry.

### Step 6 — Snapshot the LOC delta in reviewer notes

```sh
wc -l internal/cli/apply.go internal/cli/apply_*.go
```

Document the before/after:

- Before: `apply.go` 728 LOC.
- After: `apply.go` ≤ 100 LOC + four siblings, each ≤ 250 LOC ideally.

If any sibling crosses 300 LOC, the carve was wrong — re-split before submitting.

## Behavior change

**No behavior change.** Pure code motion. CI is the lock-in:

- `make test -race -count=2` covers all current behavior.
- `make ci` runs the integration matrix.
- Existing golden files in [internal/cli/testdata/](../../internal/cli/testdata/) lock in compile and plan output.

If any test fails after the move, the split was not pure — investigate which function pulled an implicit dependency (package-level state, init() ordering, etc.) and fix the move, not the test.

## Reuse

- Existing build/test/lint infrastructure. Nothing new is added here.
- The naming pattern `<base>_<concern>.go` is already used in the repo (e.g. [internal/adapters/shell/shell.go](../../internal/adapters/shell/shell.go) + [internal/adapters/shell/sandbox.go](../../internal/adapters/shell/sandbox.go)). Match it.

## Out of scope

- Renaming any function or type. The four target functions stay named `executeServerRun` / `drainResumeCycles` / `runApplyServer` / `setupServerRun` etc.
- Refactoring `runApplyLocal` to reduce its complexity. The `//nolint:funlen` stays. If the split happens to drop it below the threshold, the comment can be removed but no internal restructuring beyond extracting a single moved file.
- Adding tests for currently uncovered functions — that's [04](04-server-mode-coverage.md).
- Wiring `SubWorkflowResolver` into `compileForExecution` — that's [13](13-subworkflow-block-and-resolver.md).
- Splitting [internal/cli/localresume/resumer.go](../../internal/cli/localresume/resumer.go) (547 LOC). That happens in a future cleanup if it's still needed; not in scope here.

## Files this workstream may modify

- [`internal/cli/apply.go`](../../internal/cli/apply.go) — reduce to ≤ 100 LOC.
- `internal/cli/apply_local.go` — new.
- `internal/cli/apply_server.go` — new.
- `internal/cli/apply_resume.go` — new.
- `internal/cli/apply_setup.go` — new.
- Test files in [`internal/cli/`](../../internal/cli/) — only to move test functions adjacent to the function under test, never to rename or change them.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`.
- Any other workstream file.
- Anything outside `internal/cli/` (the carve is intra-package).
- Generated files.
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — code motion must not require new entries; if the carve adds findings, restructure the carve.

## Tasks

- [x] Carve [apply.go](../../internal/cli/apply.go) into the four new files per Step 1.
- [x] Verify `go build ./internal/cli/...` clean (Step 3).
- [x] Move test functions adjacent to their target functions (Step 4).
- [x] `go test -race -count=2 ./internal/cli/...` green.
- [x] `make lint-go` green.
- [x] `make lint-baseline-check` green at the count from [01](01-lint-baseline-burndown.md).
- [x] `make ci` green.
- [x] Snapshot LOC before/after in reviewer notes.

## Exit criteria

- [internal/cli/apply.go](../../internal/cli/apply.go) ≤ 100 LOC.
- Four new sibling files exist, each ≤ 300 LOC, with the function ownership exactly per Step 1.
- No new baseline entries in [`.golangci.baseline.yml`](../../.golangci.baseline.yml).
- All tests pass on `-race -count=2`.
- `make ci` exits 0.
- Reviewer notes contain the LOC before/after snapshot.

## Tests

This workstream does not add tests. Existing [internal/cli/apply_test.go](../../internal/cli/apply_test.go) and any `*_test.go` siblings cover the moved code. The post-move test pass under `-race -count=2` is the lock-in.

## Reviewer Notes

### LOC snapshot

| File | Before | After |
|---|---|---|
| `internal/cli/apply.go` | 728 LOC | 69 LOC |
| `internal/cli/apply_local.go` | — | 216 LOC |
| `internal/cli/apply_server.go` | — | 189 LOC |
| `internal/cli/apply_resume.go` | — | 220 LOC |
| `internal/cli/apply_setup.go` | — | 91 LOC |
| **Total** | 728 | 785 (net +57 for package headers/imports per file) |

All siblings well under the 300 LOC ceiling.

### Baseline change

The existing `.golangci.baseline.yml` entry for `hugeParam: opts is heavy` had `path: internal/cli/apply.go`.
After the split, the same pre-existing findings appear in `apply_local.go`, `apply_resume.go`, and `apply_server.go`.
The single existing entry was updated (not added) — path broadened from `internal/cli/apply.go` to `internal/cli/apply`
so the regex prefix matches all split files. Annotation `# P3-02:` added to the entry.
Baseline count remains 20/20; no new entries were added.

Converting `applyOptions` to a pointer (to eliminate the `hugeParam` finding entirely) is a signature change
outside this workstream's scope. That refactor is tracked as a future workstream item.

### Test file disposition

Existing test files (`apply_test.go`, `reattach_test.go`, `apply_local_approval_test.go`,
`apply_server_required_test.go`) each cover multiple moved functions and were left in place.
No test was renamed or removed; all pass under `-race -count=2`.

### Validation run

```
go build ./internal/cli/...          exit 0
go test -race -count=2 ./internal/cli/...  exit 0 (43s)
make lint-go                         exit 0
make lint-baseline-check             exit 0 (20/20)
make ci                              exit 0
```

## Risks
|---|---|
| A moved function relies on an unexported helper that should have moved with it | `go build ./internal/cli/...` catches this immediately. Move the helper alongside the function. |
| A `//nolint:funlen` annotation goes stale (the function complexity drops below threshold) | Remove the comment entirely. Re-run `make lint-go` to confirm. |
| A test moved to a sibling file imports a test-helper that's still in `apply_test.go` | Move the helper to a shared `apply_helpers_test.go` file alongside the others, or leave the test in `apply_test.go`. Don't duplicate the helper. |
| Code motion accidentally changes function order in a way that breaks `init()` ordering or package-level var initialization | Run `go test -race -count=2` and `make ci`. If any flake surfaces, root-cause and order the new files alphabetically by their containing file name (Go evaluates package files in lexicographic order). |
| The split surfaces a `gocognit`/`gocyclo` finding the previous file structure was averaging out | Extract an obviously-named helper (no behavior change) inside the moved function. Do not add a baseline entry. |
