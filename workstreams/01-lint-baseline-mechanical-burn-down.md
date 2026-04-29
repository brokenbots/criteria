# Workstream 1 — Lint baseline mechanical burn-down

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W02](02-lint-ci-gate.md), [W08](08-contributor-on-ramp.md) (good-first-issue material).

## Context

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md))
parks the project at **Tech Debt = C** primarily because of a 240-entry,
962-line `.golangci.baseline.yml` carrying suppressions tagged
`W03=42`, `W04=133`, `W06=54`, `W10=11`. About 80 of those entries are
purely mechanical: 71 `gofmt`, 40 `goimports`, 10 `unused` findings —
most of them artifacts of the W04 file-split that landed in Phase 1.
Another ~27 are `revive` rules suppressing proto-generated names
(`Envelope_*`, `LogStream_*`) that are untouchable without regenerating
protos.

This workstream burns down the mechanical chunk and re-classifies the
proto-generated `revive` entries from baselined-debt to permanent
`//nolint:revive` annotations with justifications. The targets are:

- W04 entries: from 133 → < 40
- Total baseline: from 240 → ≤ 120

The non-mechanical residuals (W03 funlen/gocyclo on
`handlePermissionRequest`, real `unused` cases that need code review,
W06 style findings) stay for [W03](03-copilot-file-split.md) and a
later phase. The point of this workstream is to remove the mass of
debt-paid-with-debt so the *real* exceptions are visible.

## Prerequisites

- `make ci` green on `main`.
- Local Go toolchain ≥ the version pinned in `go.mod`.
- `goimports` installed (`go install golang.org/x/tools/cmd/goimports@latest`).

## In scope

### Step 1 — Mechanical formatting pass

Run from repo root:

```sh
gofmt -w $(git ls-files '*.go')
goimports -w $(git ls-files '*.go' | grep -v '\.pb\.go$' | grep -v '\.pb\.gw\.go$')
```

Excluding generated files (`*.pb.go`, `*.pb.gw.go`) from `goimports` is
deliberate — those files are managed by `make proto`, not by hand.

After the pass, run `make lint-go` and check:

- gofmt entries in `.golangci.baseline.yml` should drop to zero.
- goimports entries should drop to zero.
- All previously-baselined `gofmt` and `goimports` lines tagged
  `# W04:` are removed from `.golangci.baseline.yml`.

If `make lint-go` reports new findings the pass cannot remove (e.g. an
import that goimports cannot order because of a build tag), document
each remaining finding with a `//nolint:goimports // <justification>`
inline annotation, not a baseline entry.

### Step 2 — Dead-code review for `unused` findings

The 10 `unused` baseline entries are individual judgement calls. For
each one:

1. Identify the symbol from the baseline-line context (file:line + rule).
2. Inspect the symbol. If it is genuinely dead code, **delete it**.
3. If it is part of an exported public API and intentionally unused
   internally (e.g. a struct field for future use, a method required by
   an interface), keep the symbol and convert the baseline entry to an
   inline `//nolint:unused // <reason>` with a one-sentence
   justification.
4. If the symbol is referenced only by tests in a different package,
   confirm the tests still compile and run.

Do not preserve dead code "in case we need it later."

### Step 3 — Reclassify proto-generated `revive` suppressions

Approximately 27 of the 54 W06-tagged entries suppress `revive`
findings on proto-generated names like `Envelope_TYPE_X` or
`LogStream_KIND_Y`. These names cannot be renamed without breaking the
wire contract.

For every such entry:

1. Locate the generated file (`sdk/pb/criteria/v1/*.pb.go`).
2. Add a single `//nolint:revive // proto-generated; cannot rename
   without breaking wire contract` annotation **at the top of the
   file** (file-level nolint), not per-symbol.
3. Remove the corresponding entries from `.golangci.baseline.yml`.

If `make proto` regenerates these files, the file-level annotation
must be re-added. Update `tools/proto-gen/` (or the equivalent
generation script) to inject the `//nolint:revive` header so the
annotation survives regeneration. If the generation tooling does not
support a header inject, document this in the workstream notes and add
a Makefile post-step that prepends the line — but a generator-side fix
is preferred.

### Step 4 — Validate baseline counts

After Steps 1–3, verify:

```sh
wc -l .golangci.baseline.yml
grep -c '^\s*-' .golangci.baseline.yml
```

The total baseline entry count must be ≤ 120. If it is higher,
investigate which class of finding survived and whether Step 1 missed
files (e.g. a build-tagged `_test.go` file).

Also check distribution:

- `# W04:` entries: < 40
- `# gofmt` entries: 0
- `# goimports` entries: 0 (excepting generated files)
- `# revive` proto-name entries: 0 (replaced by file-level nolint)

### Step 5 — Document the burn-down in `tools/lint-baseline/`

Update `tools/lint-baseline/README.md` (or whatever the convention
file is — check `docs/contributing/lint-baseline.md`) to note the
counts before and after this workstream. Include the rule-level
breakdown so future contributors know what the residual baseline
contains. Do **not** edit `PLAN.md`, `README.md`, `AGENTS.md`, or
`CHANGELOG.md` — those are owned by [W14](14-phase2-cleanup-gate.md).

## Behavior change

**No behavior change.** This workstream is mechanical formatting and
suppression hygiene. The lock-in is the existing test suite plus
`make lint-go` itself. All existing unit, integration, and conformance
tests must pass unchanged. No HCL surface change. No CLI flag change.
No event change. No log change. No new errors.

If any test fails after Step 1's mechanical pass, the failure is a
pre-existing bug exposed by reformatting — investigate and fix as
part of this workstream (it counts as scope) but document it
explicitly in reviewer notes.

## Reuse

- The lint baseline tooling lives in `tools/lint-baseline/`. Reuse
  `make lint-go` and the existing baseline diff/cap script — do not
  reimplement.
- Existing `.golangci.yml` rule configuration is correct; this
  workstream does not edit `.golangci.yml`, only `.golangci.baseline.yml`.

## Out of scope

- W03-tagged `funlen` / `gocyclo` entries on `handlePermissionRequest`
  and `permissionDetails`. Those move with [W03](03-copilot-file-split.md).
- Real (non-mechanical) `unused` findings that uncover dead code in
  active subsystems. If removal is non-trivial, leave the entry, file
  a follow-up, and document in reviewer notes.
- Adding new linter rules to `.golangci.yml`. New rules belong in a
  later phase.
- Editing generated proto files by hand to "fix" naming. Wire contract
  is immutable in this workstream.
- Changes to the lint CI gate. That is [W02](02-lint-ci-gate.md).

## Files this workstream may modify

- Any non-generated `*.go` file under the repo (mechanical formatting
  only, except for genuinely dead code removal in Step 2).
- `.golangci.baseline.yml` (entry removals only).
- `sdk/pb/criteria/v1/*.pb.go` — file-level `//nolint:revive` header
  only; do not edit generated symbols.
- `tools/proto-gen/` (if it exists, to inject the `//nolint:revive`
  header) — otherwise the generation Makefile target.
- `docs/contributing/lint-baseline.md` (update count snapshot).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Run `gofmt -w` and `goimports -w` across non-generated `*.go`.
- [x] Remove `# W04:`-tagged gofmt and goimports entries from
      `.golangci.baseline.yml`.
- [x] Triage all `unused` baseline entries; delete dead code or convert
      to inline `//nolint:unused`.
- [x] Reclassify proto-generated `revive` suppressions to file-level
      `//nolint:revive`; update the generator (or Makefile) to keep
      the header on regen.
- [x] Verify `make lint-go` clean.
- [x] Verify total baseline entry count ≤ 120.
- [x] Update `docs/contributing/lint-baseline.md` count snapshot.
- [x] `make ci` green.

## Exit criteria

- `make lint-go` exits 0 from a clean tree on the workstream branch.
- `.golangci.baseline.yml` has ≤ 120 entries.
- W04-tagged entries < 40 (down from 133).
- Zero `gofmt` and zero `goimports` baseline entries (excepting
  generated files where applicable).
- Zero proto-generated `revive` baseline entries (replaced by
  file-level nolint).
- `make test -race -count=1` green across all three modules.
- `make ci` green.

## Tests

This workstream does not add new tests. The signal is:

- `make ci` green proves the formatting pass did not break anything.
- The reduced baseline count proves the burn-down landed.
- A regeneration of the proto bindings (`make proto`) followed by
  `make lint-go` proves the file-level nolint survives proto regen.

Reviewer should run `make proto && make lint-go` once locally to
confirm Step 3 is durable.

## Risks

| Risk | Mitigation |
|---|---|
| `goimports` reorders an import group inside a build-tagged file in a way that breaks compilation on a non-default build | Run `make ci` after the mechanical pass; investigate any build-tag failures and inline-nolint rather than baseline. |
| The proto generator strips file-level comments on regen | Add the `//nolint:revive` header injection to the generator script (preferred) or as a Makefile post-step (fallback). Document the choice in reviewer notes. |
| Removing dead code in Step 2 turns out to break a downstream consumer | Run `make ci` after each removal. Removed code can be restored in the same PR if a consumer surfaces. |
| The baseline drops below the cap [W02](02-lint-ci-gate.md) is going to enforce | This is the desired outcome — W02 sizes its cap from W01's final count. |

## Reviewer notes (batch 1)

- Mechanical pass executed with `gofmt -w` and `goimports -local github.com/brokenbots/criteria -w` over repo `*.go` excluding `*.pb.go` and `*.pb.gw.go`.
- Removed all baseline rules for `gofmt`, `goimports`, and `unused`. Current baseline shape after this batch: 156 entries total, 49 `# W04:` entries, zero `gofmt`/`goimports`/`unused` entries.
- Deleted dead code for all previously baselined `unused` findings (no inline `//nolint:unused` needed):
  - `workflow/branch_compile_test.go`: removed `branchBaseWorkflow`.
  - `workflow/compile_validation.go`: removed `decodeBodyToStringMap`.
  - `sdk/conformance/helpers.go`: removed `payloadArmName`.
  - `sdk/conformance/inmem_subject_test.go`: removed unused `runRecord.once` and `(*runRecord).stop`.
  - `internal/run/console_sink.go`: removed unused `(*ConsoleSink).writef`.
  - `internal/transport/server/reattach_scope_integration_test.go`: removed unused `captureInputSink` test helper type/methods.
- Validation run in this batch:
  - `make lint-go` (pass)
  - `go test ./internal/run ./internal/transport/server -count=1` (pass)
  - `go test ./workflow/... -count=1` (pass)
  - `go test ./sdk/conformance -count=1` (pass)

## Reviewer Notes

### Review 2026-04-29 — changes-requested

#### Summary

Steps 1 and 2 are correctly implemented: all gofmt/goimports/unused entries have been
removed from the baseline and all six dead-code symbols have been legitimately deleted
with no lingering references. `make lint-go` exits 0. Steps 3, 4, and 5 are not
implemented. Four exit criteria fail: total entries 156 > 120; W04-tagged entries
49 ≥ 40; 28 proto-generated `revive` entries remain in the baseline (Step 3 incomplete);
`docs/contributing/lint-baseline.md` count snapshot is stale. Additionally, a
pre-existing golden test failure in `internal/cli` causes `make test -race` and
`make ci` to fail — the executor's batch notes do not mention this and the
`make ci` exit criterion is unmet.

#### Plan Adherence

| Task | Status |
|---|---|
| Run `gofmt -w` / `goimports -w` across non-generated `.go` | ✅ Done |
| Remove `# W04:` gofmt and goimports entries from baseline | ✅ Done |
| Triage all `unused` entries; delete dead code or convert to inline nolint | ✅ Done |
| Reclassify proto-generated `revive` suppressions; update generator/Makefile | ❌ Not done |
| Verify `make lint-go` clean | ✅ Passes |
| Verify total baseline entry count ≤ 120 | ❌ 156 entries (target ≤ 120) |
| Update `docs/contributing/lint-baseline.md` count snapshot | ❌ Not done |
| `make ci` green | ❌ Fails (golden tests) |

#### Required Remediations

**[BLOCKER 1] — Step 3 not completed: 28 proto-generated `revive` entries remain in baseline**

- **Files:** `.golangci.baseline.yml`, `sdk/events.go`, `sdk/payloads_step.go`
- **Evidence:** `grep -c 'revive' .golangci.baseline.yml` → 71 total. 24 entries point at `events.go` (all `Envelope_*` type aliases); 4 entries point at `payloads_step.go` (all `LogStream_LOG_STREAM_*` constants). The remaining 43 entries are legitimate W06 naming-convention findings (test functions with underscores in `conformance/caller_ownership.go` and `conformance/resume.go`), which are out of scope for W01.
- **Required:** Add a file-level `//nolint:revive // proto-generated names: Envelope_* and LogStream_* aliases cannot be renamed without breaking the wire contract` annotation to `sdk/events.go` and `sdk/payloads_step.go`. Remove the 28 corresponding `path: events.go` and `path: payloads_step.go` revive entries from `.golangci.baseline.yml`. Additionally, add a Makefile post-step (or generator-side hook in `tools/proto-gen/`) to re-inject the annotation after `make proto` regenerates the `.pb.go` files — or confirm that `sdk/events.go` and `sdk/payloads_step.go` are hand-maintained SDK files (not generated) and therefore survive `make proto` untouched. Either conclusion must be documented in the reviewer notes.
- **Acceptance criteria:** `grep -c 'revive' .golangci.baseline.yml` for paths `events.go` or `payloads_step.go` returns 0. `make lint-go` still exits 0. File-level nolint comment is present in both files and contains a one-sentence justification.

**[BLOCKER 2] — Exit criterion ≤ 120 entries not met; will not be met even after Step 3**

- **Evidence:** Current count is 156 entries (`grep -c '^\s*- path:' .golangci.baseline.yml`). Completing Step 3 removes 28 entries → ~128, still 8 over the cap. W04 entries will remain at 49 (Step 3 doesn't touch W04-tagged items), still ≥ 40.
- **Baseline distribution after batch 1:** W03=42, W04=49, W06=54, W10=11 → total 156.
- **Required:** After Step 3, the executor must audit the remaining W04 entries to eliminate at least another 8 baseline entries from `.golangci.baseline.yml` AND reduce W04-tagged entries below 40. The 49 remaining W04 entries break down as: `errcheck`×9, `contextcheck`×9, `gocognit`×6, `unparam`×5, `gocyclo`×5, `funlen`×5, `staticcheck`×3, `prealloc`×2, `errorlint`×2, `nilerr`×1, `gosimple`×1, `dupword`×1. Mechanical candidates include: `dupword`×1 (comment fix), `gosimple`×1 (simplification), `prealloc`×2 (slice preallocation), and `unparam`×5 (remove or use the parameter). Fixing these 9 would bring W04 to 40 — still not < 40. The executor must fix at least 10 W04 entries and in total remove at least 36 more baseline entries (combining Step 3 and additional fixes). Document each W04 entry removed or justify why it cannot be reduced further.
- **Acceptance criteria:** `grep -c '^\s*- path:' .golangci.baseline.yml` ≤ 120. `grep -c '# W04:' .golangci.baseline.yml` < 40. `make lint-go` exits 0.

**[BLOCKER 3] — Pre-existing golden test failures in `internal/cli` not addressed**

- **Files:** `internal/cli/testdata/compile/workstream_review_loop__examples__workstream_review_loop_hcl.json.golden`, `internal/cli/testdata/plan/workstream_review_loop__examples__workstream_review_loop_hcl.golden`
- **Evidence:** `go test ./internal/cli/... -run TestCompileGolden_JSONAndDOT` and `TestPlanGolden` both fail with golden mismatch. Root cause: commit `636e629` (Phase 2 plan) changed `examples/workstream_review_loop.hcl` but did not update the golden files. This failure is pre-existing on `main` and is not introduced by the executor's batch 1 changes (confirmed with `git stash`).
- **Workstream responsibility:** The workstream's exit criterion requires `make ci` green and `make test -race -count=1` green across all three modules. The workstream plan also states: "If any test fails after Step 1's mechanical pass, the failure is a pre-existing bug exposed by reformatting — investigate and fix as part of this workstream." Although the failure predates the mechanical pass, the executor's validation did not run `go test ./internal/cli/...` and did not surface or address it.
- **Required:** Run `go test -run TestCompileGolden_JSONAndDOT/workstream_review_loop ./internal/cli/... -update` (or the equivalent golden update flag) to regenerate the two stale golden files against the current HCL, then verify both tests pass and the updated golden content is correct (not vacuously empty). Document the golden update in the batch notes.
- **Acceptance criteria:** `go test -race -count=1 ./internal/cli/...` exits 0. The updated `.golden` files are committed. The executor explicitly states the pre-existing cause in the reviewer notes.

**[BLOCKER 4] — Executor's batch validation did not include `internal/cli`**

- **Files:** executor's "Reviewer notes (batch 1)" validation list
- **Evidence:** Validation only covers `internal/run`, `internal/transport/server`, `workflow/...`, and `sdk/conformance`. `internal/cli` was not tested. This allowed the golden test failures to go undetected.
- **Required:** Final validation before submission must include `go test -race -count=1 ./...` across the root module (or at minimum all packages with tests) plus `make ci`. Add these to the reviewer notes for the batch that resolves all blockers.
- **Acceptance criteria:** Executor's notes list `go test -race -count=1 ./...` (root module) and `make ci` as passing.

**[REQUIRED] — `docs/contributing/lint-baseline.md` count snapshot not updated**

- **Files:** `docs/contributing/lint-baseline.md`
- **Evidence:** No diff to this file between `main` and the workstream branch. The file contains no before/after count section for W01.
- **Required:** Add a W01 burn-down section to `docs/contributing/lint-baseline.md` documenting the per-rule breakdown before and after this workstream (as required by Step 5). The section must include at minimum: starting count (240), final count (≤ 120), and per-tag distribution (`W03`, `W04`, `W06`, `W10`). Must be completed before the `make ci` exit criterion can be met.
- **Acceptance criteria:** `docs/contributing/lint-baseline.md` contains a W01 before/after section with numeric counts. `make ci` is green when this task is complete.

#### Test Intent Assessment

This workstream does not add tests. The relevant signal is `make ci` being green. The executor ran a partial package subset; `internal/cli` was omitted, hiding the golden test failures. The subset that was run (`internal/run`, `internal/transport/server`, `workflow`, `sdk/conformance`) all passed correctly — the dead-code removals and formatting changes did not break any tested behavior. The omitted `internal/cli` package has two failing golden tests unrelated to this workstream's code changes but required by the exit criterion.

No additional test intent concerns beyond the golden test fix required by Blocker 3.

#### Validation Performed

```
make lint-go                                  → exit 0 ✅
go test -race -count=1 ./sdk/... ./workflow/... → exit 0 ✅
go test -race -count=1 ./internal/...         → FAIL (internal/cli golden tests) ❌
grep -c '^\s*- path:' .golangci.baseline.yml  → 156 (target ≤ 120) ❌
grep -c '# W04:' .golangci.baseline.yml       → 49 (target < 40) ❌
grep -c 'revive' .golangci.baseline.yml       → 71 (28 on proto-name files remain) ❌
diff docs/contributing/lint-baseline.md       → no changes (update required) ❌
```

## Reviewer notes (batch 2)

- Completed Step 3 by moving proto-name `revive` suppressions from baseline into file-level annotations:
  - `sdk/events.go`: `//nolint:revive // Proto-generated Envelope_* alias names are wire-compatibility shims and cannot be renamed.`
  - `sdk/payloads_step.go`: `//nolint:revive // Proto-generated LogStream_* constant names are wire-compatibility shims and cannot be renamed.`
- Removed all `revive` baseline entries for `events.go` and `payloads_step.go` (24 + 4 entries).
- Confirmed regeneration durability path: `make proto` only regenerates `sdk/pb/` (`buf generate`); `sdk/events.go` and `sdk/payloads_step.go` are hand-maintained SDK wrapper files and remain unchanged by proto generation, so no generator hook/Makefile post-step is required.
- Addressed additional W04 reductions (beyond Step 3) and removed corresponding baseline entries:
  - `sdk/conformance/ack.go`: fixed `dupword` finding.
  - `workflow/eval.go`: fixed `gosimple` blank identifier assignment.
  - `sdk/conformance/inmem_subject_test.go` and `internal/cli/local_state.go`: fixed `prealloc` findings.
  - `sdk/conformance/caller_ownership.go` and `internal/engine/node_wait.go`: fixed `unparam` findings.
  - `cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main_test.go` and `cmd/criteria-adapter-mcp/testfixtures/echo-mcp/main.go`: fixed `errorlint` findings via `errors.Is`.
- Resolved pre-existing `internal/cli` golden drift (introduced by earlier workflow example changes):
  - Regenerated golden files with `go test ./internal/cli/... -run 'TestCompileGolden_JSONAndDOT/workstream_review_loop__examples__workstream_review_loop_hcl_json|TestPlanGolden/workstream_review_loop__examples__workstream_review_loop_hcl' -update`
  - Updated:
    - `internal/cli/testdata/compile/workstream_review_loop__examples__workstream_review_loop_hcl.json.golden`
    - `internal/cli/testdata/plan/workstream_review_loop__examples__workstream_review_loop_hcl.golden`
- Updated `docs/contributing/lint-baseline.md` with W01 before/after counts and residual linter distribution.
- Final baseline counts after batch 2:
  - total entries: 117 (≤ 120)
  - `# W04:` entries: 38 (< 40)
  - `gofmt`: 0, `goimports`: 0, `unused`: 0
  - `revive` entries for `events.go`/`payloads_step.go`: 0
- Validation run in this batch:
  - `make lint-go` (pass)
  - `go test ./internal/cli/... -run 'TestCompileGolden_JSONAndDOT/workstream_review_loop__examples__workstream_review_loop_hcl_json|TestPlanGolden/workstream_review_loop__examples__workstream_review_loop_hcl' -update` (pass)
  - `go test -race -count=1 ./... && (cd sdk && go test -race -count=1 ./...) && (cd workflow && go test -race -count=1 ./...)` (pass)
  - `make proto && make lint-go` (pass)
  - `make ci` (pass)

### Review 2026-04-29-02 — approved

#### Summary

All four blockers and the required doc update from the prior review are resolved. Every exit
criterion is now met and independently verified. `make ci` passes cleanly (a transient file-not-found
error on a first cold run was traced to the `golangci-lint` merged-config creation racing with a
prior `make proto` cleanup; a second run and standalone `make lint-go` both exit 0). No new
baseline entries were introduced. The code changes are all correct and appropriately scoped.

#### Plan Adherence

| Task | Status |
|---|---|
| Run `gofmt -w` / `goimports -w` across non-generated `.go` | ✅ Done (batch 1) |
| Remove `# W04:` gofmt and goimports entries from baseline | ✅ Done (batch 1) |
| Triage all `unused` entries; delete dead code or convert to inline nolint | ✅ Done (batch 1) |
| Reclassify proto-generated `revive` suppressions; confirm generator durability | ✅ Done (batch 2) |
| Verify `make lint-go` clean | ✅ Passes |
| Verify total baseline entry count ≤ 120 | ✅ 117 entries |
| Update `docs/contributing/lint-baseline.md` count snapshot | ✅ Done (batch 2) |
| `make ci` green | ✅ Passes |

#### Validation Performed

```
grep -c '^\s*- path:' .golangci.baseline.yml   → 117 (≤ 120 ✅)
grep -c '# W04:' .golangci.baseline.yml         → 38 (< 40 ✅)
grep -c '# W06:' .golangci.baseline.yml         → 29 ✅
grep -c '# W10:' .golangci.baseline.yml         → 8 ✅
gofmt/goimports/unused entries                  → 0 ✅
revive entries for events.go / payloads_step.go → 0 ✅
head -1 sdk/events.go                           → //nolint:revive // Proto-generated... ✅
head -1 sdk/payloads_step.go                    → //nolint:revive // Proto-generated... ✅
make lint-go                                    → exit 0 ✅
go test -race -count=1 ./... (root module)      → all ok ✅
cd sdk && go test -race -count=1 ./...          → all ok ✅
cd workflow && go test -race -count=1 ./...     → ok ✅
make proto && make lint-go                      → exit 0 (nolint survives regen) ✅
make ci                                         → exit 0 ✅
docs/contributing/lint-baseline.md W01 section → present, counts verified accurate ✅
```

Linter distribution in final baseline matches `docs/contributing/lint-baseline.md` exactly:
`funlen`×30, `gocritic`×25, `gocognit`×18, `gocyclo`×13, `revive`×9, `errcheck`×9,
`contextcheck`×9, `staticcheck`×3, `nilerr`×1 → total 117.
