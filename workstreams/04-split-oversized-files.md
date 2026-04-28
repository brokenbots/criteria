# Workstream 4 — Split oversized files

**Owner:** Workstream executor · **Depends on:** [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md) · **Unblocks:** [W08](08-for-each-multistep.md) (which adds compile-time validation to `workflow/`).

## Context

Three single files violate the single-responsibility principle so
loudly that future workstreams (notably W08's `for_each` compile
validation) cannot land cleanly in them:

| File | Lines | Concerns mixed |
|---|---|---|
| [workflow/compile.go](../workflow/compile.go) | 1099 | HCL parsing, schema validation, agent binding, step compile, variable compile, value coercion |
| [internal/adapter/conformance/conformance.go](../internal/adapter/conformance/conformance.go) | 797 | Test harness, ten contract assertions, fixtures, helpers |
| [internal/transport/server/client.go](../internal/transport/server/client.go) | 644 | Client construction, auth, control stream, publish stream, heartbeat, reattach, resume |

This workstream is **pure file split**. No behavior change. No new
features. The lock-in is the existing test suite plus
[W01](01-flaky-test-fix.md)'s deterministic CI plus
[W02](02-golangci-lint-adoption.md)'s `make lint-go`. Each split:

- Moves whole functions verbatim into new files in the same
  package. No signature changes; no API changes; no renames.
- Preserves the existing import set per file (each new file
  imports only what it uses).
- Includes a one-line file-level doc comment naming the slice of
  responsibility (e.g. `// compile_steps.go — step block compile
  and validation.`).

Splits are a force multiplier for [W03](03-god-function-refactor.md)'s
extractions: the helpers W03 introduced into the same file can
move to the appropriate split here, leaving each file readable
end-to-end.

## Prerequisites

- [W03](03-god-function-refactor.md) merged. Splitting a file
  while it still contains a 194-line god-function would obscure
  the diff.
- `make ci` green on `main`.

## In scope

### Step 1 — Split `workflow/compile.go`

Target layout (all in `package workflow`):

| New file | Contents (move from `compile.go`) |
|---|---|
| `compile.go` (kept; ≤ 200 lines) | `Compile` entry point + the top-level walk over `Spec`. |
| `compile_variables.go` | `parseVariableType`, `convertCtyValue`, `isListStringValue`, plus the variable-decode block currently inlined in `Compile`. |
| `compile_agents.go` | Agent binding logic: `adapterInfo`, agent-config decoding, agent-level allow-tools (`workflowAllowTools`, `unionAllowTools`). |
| `compile_steps.go` | Step compile + step-level allow-tools (`allowToolsForStep`), outcome/transition wiring, step-input handling. |
| `compile_validation.go` | `validateSchemaAttrs`, `decodeAttrsToStringMap`, `decodeBodyToStringMap`. |
| `compile_lifecycle.go` | `isValidOnCrash`, `isValidLifecycle`, `isValidAdapterName` (small but logically grouped). |

`Compile` itself stays in `compile.go` and is the only function
that calls into the per-concern helpers. Do not introduce new
exported symbols. Do not change function signatures. Internal
helpers may need to switch from package-private struct fields to
explicit parameters if a helper moves to a new file and previously
relied on closure capture; in that case, pass the necessary
arguments explicitly rather than introducing a shared mutable
state struct.

Test files mirror the split:

- `compile_variables_test.go` already exists (rename
  `variable_compile_test.go` → `compile_variables_test.go` for
  symmetry).
- `compile_agent_config_test.go` is already named consistently;
  leave it.
- `compile_steps_test.go` (new — move step-related tests from
  `workflow_test.go` if they cleanly belong there; if they don't,
  leave them in `workflow_test.go`).

Test file renames are mechanical `git mv` operations — no test
body changes. If a test asserts internal state via a function
that moved, the assertion still compiles because the function is
in the same package.

### Step 2 — Split `internal/adapter/conformance/conformance.go`

Target layout (all in `package conformance`):

| New file | Contents |
|---|---|
| `conformance.go` (kept; ≤ 150 lines) | `Run`, `RunPlugin`, `runContractTests` orchestration; `Options` struct; `targetFactory` type. |
| `conformance_happy.go` | `testHappyPath`, `testNilSink`, `testChunkedIO`. |
| `conformance_lifecycle.go` | `testCancel`, `testTimeout`, lifecycle-related tests. |
| `conformance_outcomes.go` | `testOutcomeDomain` and any other outcome-shape assertions. |
| `assertions.go` | Shared assertion helpers (e.g. `assertEvent`, `assertSinkClosed`) currently inlined in test bodies. Extract only when the same assertion appears ≥ 3 times; otherwise leave inlined. |
| `fixtures.go` | Fake adapters, channel helpers, sink fakes (e.g. `executeNoPanic` if applicable). |

Each `testXxx` function is a top-level test orchestration; they
do not need to live in `_test.go` because the conformance package
is itself a test helper consumed by other packages.

Reviewer rejects splits that introduce new exported symbols. The
public surface of the conformance package is `Run`, `RunPlugin`,
and `Options`; everything else stays unexported.

### Step 3 — Split `internal/transport/server/client.go`

Target layout (all in `package server`):

| New file | Contents |
|---|---|
| `client.go` (kept; ≤ 200 lines) | `Client` struct definition, `NewClient`, `buildHTTPClient`, accessor methods (`CriteriaID`, `Token`, `RunCancelCh`, `ResumeCh`, `Close`, `isClosed`, `authorize`, `backoffSleep`). |
| `client_runs.go` | `Register`, `CreateRun`, `ReattachRun`, `Resume`, `Drain`. |
| `client_streams.go` | `StartStreams`, `StartPublishStream`, `startControl`, `controlLoop`, `startPublish`, `publishLoop`, `runSubmitEvents`, `sendLoop`, `recvAcks`, `Publish`. |
| `client_pending.go` | `appendPending`, `snapshotPending`, `clearPending` and the in-memory pending-envelope buffer. |
| `client_heartbeat.go` | `StartHeartbeat`, `heartbeat`. |
| `client_credentials.go` | `SetCredentials` plus any credential-bookkeeping helpers. |

`Client` struct definition stays in `client.go`. Methods may move
freely between files because Go binds methods to the type, not
the file.

If a method has a bidirectional dependency that cuts across two
of the proposed files, group the pair together (e.g. if
`startPublish` and `runSubmitEvents` truly cannot live in
separate files, document the coupling in a single-line comment
above each and keep them together). Do **not** introduce a new
abstraction to break the coupling — that is a [W03](03-god-function-refactor.md)
class of work, not a split.

### Step 4 — Burn down baseline entries

Splits do not reduce `funlen`/`gocyclo` — those are per-function.
But splits often reveal `unused` or `revive`/exported findings
that the baseline currently suppresses on the monolithic file. In
the same diff:

- Re-run `make lint-go`. Any baseline entries that are now
  unreachable (because the file path no longer exists) get
  deleted.
- Any new lint findings that surface from the split (likely
  `revive`'s `package-comments` rule firing on the new files)
  get fixed in place by adding the package-doc comment to the
  new files. Do not add new baseline entries.

Each new file must start with a `// <filename> — <one-line
purpose>` comment immediately after the `package` declaration.
Example:

```go
package workflow

// compile_steps.go — step block compile, outcome wiring, and
// step-level allow-tools resolution.

import (
    ...
)
```

This satisfies the package-comments rule when only one file
carries the package-level doc comment proper, and provides a
human-readable nav anchor.

## Out of scope

- Changing function signatures or behavior. Pure relocation only.
- Adding new tests. The lock-in is the existing test suite.
- Splitting the Copilot adapter `copilot.go` (614 lines). The W03
  refactor of `Execute` already brings it within range; if it
  still exceeds 500 lines after W03, defer to Phase 2 — it is
  not on the tech-eval critical list.
- Splitting `internal/cli/apply.go` or `internal/cli/reattach.go`.
  The W03 refactor brings both within range.
- Renaming the `workflow` / `conformance` / `server` packages.
- Introducing new abstractions to bridge cross-file coupling.

## Files this workstream may modify

**Created:**

- `workflow/compile_variables.go`
- `workflow/compile_agents.go`
- `workflow/compile_steps.go`
- `workflow/compile_validation.go`
- `workflow/compile_lifecycle.go`
- `internal/adapter/conformance/conformance_happy.go`
- `internal/adapter/conformance/conformance_lifecycle.go`
- `internal/adapter/conformance/conformance_outcomes.go`
- `internal/adapter/conformance/assertions.go` (only if ≥ 3 reuse)
- `internal/adapter/conformance/fixtures.go`
- `internal/transport/server/client_runs.go`
- `internal/transport/server/client_streams.go`
- `internal/transport/server/client_pending.go`
- `internal/transport/server/client_heartbeat.go`
- `internal/transport/server/client_credentials.go`

**Modified (mostly shrunk):**

- `workflow/compile.go`
- `internal/adapter/conformance/conformance.go`
- `internal/transport/server/client.go`
- `.golangci.baseline.yml` (delete unreachable / fixed entries
  pointed at W04 only).

**Renamed (`git mv`):**

- `workflow/variable_compile_test.go` → `workflow/compile_variables_test.go`
  (only if a similar rename keeps test files paired with the
  source file they exercise — skip if it fights existing
  conventions).

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, any other
workstream file, or any source file outside the three packages
listed above.

## Tasks

- [ ] Split `workflow/compile.go` per Step 1; one commit per
      target file is fine, or one bundled commit if the diff is
      review-friendly.
- [ ] Split `internal/adapter/conformance/conformance.go` per
      Step 2.
- [ ] Split `internal/transport/server/client.go` per Step 3.
- [ ] Add file-level purpose comments to every new file.
- [ ] Re-run `make lint-go`; remove unreachable baseline entries;
      fix any new findings in place.
- [ ] `make ci` green.
- [ ] `go test -race -count=10 ./...` green across all three
      modules.
- [ ] CLI smoke: `./bin/criteria apply examples/hello.hcl` exits 0.

## Exit criteria

- No file in `workflow/`, `internal/adapter/conformance/`, or
  `internal/transport/server/` exceeds 350 lines (target: 200;
  hard ceiling: 350 to allow legitimate cohesion).
- Every new file starts with a `package` declaration followed by
  a one-line purpose comment.
- `make lint-go` exits 0 with no new baseline entries added.
- `make ci` green; `go test -race -count=10 ./...` green.
- Cross-module conformance test (`make test-conformance`) green —
  proves the conformance-package split preserved the contract.
- The example workflows continue to validate (`make validate`).
- `git diff --stat` shows mostly-additive file creation; the three
  monolith files shrink commensurately.
- No new exported symbols introduced anywhere in the diff.

## Tests

This workstream **adds no new tests**. Lock-in:

- The existing `workflow/*_test.go` test suite (compile, parse,
  schema, eval, for_each, branch, wait, agents, variables).
- The conformance package consumers under
  `internal/adapter/conformance/` and the in-tree adapter
  conformance suites that exercise it (e.g. Copilot).
- The server-transport tests under `internal/transport/server/`.
- `make test-conformance` against the in-memory Subject.
- `make validate` against the full `examples/` corpus.

If a split would break a test compile, that is a signal the split
is wrong (e.g. a function moved to a file with a more restrictive
import set). Restructure the split, do not change the test.

## Risks

| Risk | Mitigation |
|---|---|
| A function moved into a new file silently changes import-cycle structure | Each new file's import block is the union of imports the moved functions need; `go vet` and `make build` catch cycles. The conformance package and `workflow` package are leaf packages by `lint-imports`, so no cycle is reachable. |
| The split diff is too large to review | Land each of the three packages as its own PR, or as three separate commits within this workstream. Reviewer enforces commit boundaries. |
| Renaming test files breaks `go test ./...` discovery | Test files are discovered by `_test.go` suffix, not by name pairing. Renames are safe. Skip renames if they introduce diff churn without value. |
| New file-level doc comments stutter against the package doc | Only one file per package may carry the canonical package doc (`// Package workflow ...`). Other files use file-level `// filename.go — purpose` comments without `Package` prefix. `revive`'s `package-comments` rule accepts this convention. |
| Split file count grows — discovery feel suffers | Cap at the layout above. If a future workstream wants finer granularity, that is its own work. The cap exists to prevent "one file per function" fragmentation. |
| Method receiver moves to a new file but a test file relies on package-private fields exposed in the same file | Same package — package-private access works regardless of file. No mitigation needed; flag if tests fail to compile. |
| Splits open the door to new exported symbols by accident | Reviewer must scan `go doc ./...` before/after; the public surface must be byte-identical. Append the diff to reviewer notes. |
