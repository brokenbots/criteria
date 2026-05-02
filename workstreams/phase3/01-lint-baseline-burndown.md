# Workstream 01 — Lint baseline burn-down to ≤ 50 entries

**Phase:** 3 (HCL/runtime rework, target `v0.3.0`) · **Track:** A (pre-rework cleanup) · **Owner:** Workstream executor · **Depends on:** Phase 2 closed at `v0.2.0` (W16 archived). · **Unblocks:** Every Track B / C workstream that adds new code (the rework cannot land if the lint cap is at 70/70).

## Context

The Phase 2 cleanup gate ([archived/v2/16-phase2-cleanup-gate.md](../archived/v2/16-phase2-cleanup-gate.md)) closed with `tools/lint-baseline/cap.txt` at exactly **70/70** per [TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §2 and §8. The cap-equals-count state is hostile to a phase that adds new code: the first new lint hit fails CI and forces every rework workstream to either fix unrelated debt or raise the cap. The architecture team's "stabilize before the new contributor lands" intent requires headroom.

Tech eval breakdown of the 70 entries:

| Linter | Count | Class |
|---|---:|---|
| `gocritic` | 24 | Mostly hugeParam, unnamedResult, rangeValCopy |
| `revive` | 9 | Naming on internal-but-test-exposed identifiers |
| `errcheck` | 9 | Unchecked CloseRequest / Shutdown / CloseSession |
| `contextcheck` | 9 | Context-passing pattern violations |
| `gocognit` | 7 | `compileWaits`, `compileBranches`, `compileForEachs`, `compileSteps`, `SerializeVarScope` |
| `gocyclo` | 6 | Same set + `checkReachability` |
| `funlen` | 6 | Oversized function bodies |

Owner tags: W04=34, W06=28, W07=4, W10=4 (carried over from Phase 1 / Phase 2 burn-downs).

This workstream burns down **mechanical and pointer-passing classes** (`errcheck`, `contextcheck`, `gocritic` hugeParam/rangeValCopy) which together account for ~24 entries. The `gocognit`/`gocyclo`/`funlen` entries on `compileSteps` and the `compile*` family are deliberately **left for [03](03-split-compile-steps.md)**, which splits the file along step-kind lines and naturally clears those measurements. Same for any `compileBranches` debt — [16](16-switch-and-if-flow-control.md) deletes the branch block entirely, removing those entries by removing the function.

**Target:** total baseline ≤ 50 entries. Cap dropped to the new count. No new baseline entries introduced.

## Prerequisites

- Phase 2 closed and tagged `v0.2.0` on remote (W16 ran). [PLAN.md](../../PLAN.md) and [workstreams/README.md](../README.md) updated by W16 to reflect Phase 3 active.
- `make ci` green on `main`.
- Local Go toolchain at the version pinned in [go.mod](../../go.mod).
- `golangci-lint` installed at the version `make lint-go` invokes (check the `Makefile` `lint-go` target for the exact version).

## In scope

### Step 1 — Snapshot the starting baseline

Run from repo root and capture into the workstream branch's reviewer notes:

```sh
make lint-baseline-check
wc -l .golangci.baseline.yml
grep -c '^\s*- path:' .golangci.baseline.yml
grep -oE '#\s*linter:\s*\w+' .golangci.baseline.yml | sort | uniq -c
grep -oE '#\s*W[0-9]+' .golangci.baseline.yml | sort | uniq -c
```

Confirm the entry count is 70 (matches `tools/lint-baseline/cap.txt`). If it has drifted, stop and reconcile against `main` before any change — the burn-down only counts if the starting point is the cap.

### Step 2 — Burn down `errcheck` (target: 0 entries)

The 9 `errcheck` entries are unchecked errors on `CloseRequest`, `Shutdown`, `CloseSession`, and similar release-the-resource paths. Each one is fixed with **one of these three patterns** depending on context — pick deterministically:

- **Defer + log via the package logger** if the call is in a function that has access to a `Logger` field or `slog.Default()`:
  ```go
  defer func() {
      if err := stream.CloseRequest(); err != nil {
          slog.Default().Debug("CloseRequest failed", "err", err)
      }
  }()
  ```
- **Discard with `_` only** if the call is a best-effort cleanup with no consumer of the error (e.g. a `Shutdown` in a test cleanup): `_ = sess.Shutdown(ctx)`.
- **Propagate via `errors.Join`** if the function already returns an error and the close error is meaningful for callers: `err = errors.Join(err, sess.CloseSession())`.

For each `errcheck` entry:

1. Locate the file and line from the baseline entry.
2. Pick the pattern above based on context (function signature, caller's logging surface, whether the error is informational vs. a real failure mode).
3. Apply the fix.
4. Remove the corresponding entry from `.golangci.baseline.yml`.
5. Run `make lint-go` and confirm the entry count drops by one (or more if the fix happened to clear an adjacent finding).

Do **not** silence `errcheck` with `//nolint:errcheck`. If a call truly cannot be fixed, leave the baseline entry and document why in reviewer notes — but no such case is expected in this set.

### Step 3 — Burn down `contextcheck` (target: 0 entries)

The 9 `contextcheck` entries flag functions that accept a `context.Context` from a caller but pass `context.Background()` (or a fresh derivation) to a downstream call. The fix is always the same: **thread the caller's context through**.

Pattern:

```go
// Before: contextcheck flags this
func foo(ctx context.Context, ...) {
    bar(context.Background(), ...) // <-- bug
}

// After
func foo(ctx context.Context, ...) {
    bar(ctx, ...)
}
```

If a downstream call genuinely needs a detached context (e.g. background cleanup that must outlive the request), use `context.WithoutCancel(ctx)` (Go 1.21+) and add a one-line comment explaining why. **Do not** use `context.Background()` — the linter will keep flagging it. **Do not** add `//nolint:contextcheck` unless `context.WithoutCancel` is genuinely wrong for the call site (no expected case in this set).

For each entry: fix, remove from baseline, re-run `make lint-go`.

### Step 4 — Burn down `gocritic` hugeParam / rangeValCopy / unnamedResult (target: ≤ 8 entries from 24)

Of the 24 `gocritic` entries, audit the rule for each:

- **`hugeParam`** — function takes a struct ≥ 80 bytes by value. Fix: change to `*Struct`. If the function mutates the struct, this is also a correctness improvement. If the function does not mutate, the `*` is still required to silence the linter.
  - Update all call sites in the same workstream.
  - If the struct is passed across a public package boundary (i.e. the change is API-visible), **leave it** and document in reviewer notes — that's a Phase 4 design call.
- **`rangeValCopy`** — `for _, v := range slice` copies a large value per iteration. Fix: `for i := range slice { v := &slice[i]; ... }` or restructure to iterate by index.
- **`unnamedResult`** — function returns multiple values with no parameter names. Fix: name them, e.g. `func compile() (spec *Spec, err error)`.

For each entry, apply the fix, run tests, confirm no regressions, remove the baseline entry.

If after the audit a `gocritic` finding genuinely cannot be fixed without breaking a public surface, leave it as a baseline entry with a comment line above it: `# kept: <one-sentence reason>`. The acceptable residual cap is **8 `gocritic` entries** out of the original 24.

### Step 5 — Defer the complexity entries (`gocognit`, `gocyclo`, `funlen`) to siblings

Do **not** touch any baseline entry for:

- `compileSteps`, `compileWaits`, `compileBranches`, `compileForEachs` in [workflow/compile_steps.go](../../workflow/compile_steps.go) — owned by [03-split-compile-steps.md](03-split-compile-steps.md).
- `runApplyServer`, `executeServerRun`, `setupServerRun` in [internal/cli/apply.go](../../internal/cli/apply.go) — owned by [02-split-cli-apply.md](02-split-cli-apply.md).
- `SerializeVarScope`, `checkReachability`, anything inside [workflow/eval.go](../../workflow/eval.go) — those naturally clear when [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md) and [08-schema-unification.md](08-schema-unification.md) refactor the eval surface.

Document in reviewer notes which complexity entries were left for which sibling. The W16 cleanup gate verifies the residual count.

### Step 6 — Triage the remaining `revive` entries

The 9 `revive` entries are mostly internal-naming-convention findings (`Foo_Bar` style). For each:

1. If the symbol is already file-level `//nolint:revive`'d (proto-generated), the entry is leftover from before the file-level annotation was added — remove from baseline.
2. If the symbol is internal and renaming is cheap, rename and update call sites.
3. If the symbol is part of a public API and renaming is breaking, keep a baseline entry with a `# kept: public-API` comment.

Target: ≤ 4 `revive` entries remain after triage.

### Step 7 — Lower `tools/lint-baseline/cap.txt`

After Steps 2–6, count the remaining baseline entries:

```sh
grep -c '^\s*- path:' .golangci.baseline.yml
```

Update `tools/lint-baseline/cap.txt` to the **exact current count**. The cap is not a guess — it is a measurement. Tracking the cap one above the count just to "give room" is explicitly forbidden by [archived/v2/02-lint-ci-gate.md](../archived/v2/02-lint-ci-gate.md)'s contract (cap-stays-flat enforcement).

Run `make lint-baseline-check` to confirm the cap-vs-count check is green at the new value.

### Step 8 — Update the lint-baseline doc

Append a Phase 3 W01 section to [docs/contributing/lint-baseline.md](../../docs/contributing/lint-baseline.md) following the format of the existing W01 (Phase 2) section. Required content:

- Starting count: 70 (from the v0.2.0 tag).
- Final count: ≤ 50 (state the actual number).
- Per-rule before/after distribution (use the table format from this workstream's Context section).
- Kept-with-justification list (any `gocritic` or `revive` entries that survived with a `# kept:` comment, with the justification).

Do **not** edit `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, or `workstreams/README.md`. Those are owned by [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md).

### Step 9 — Validation

```sh
make lint-go
make lint-baseline-check
make test -race -count=1 ./... && (cd sdk && go test -race -count=1 ./...) && (cd workflow && go test -race -count=1 ./...)
make ci
```

All four must exit 0 from a clean tree on the workstream branch.

## Behavior change

**No behavior change.** This workstream is mechanical fixes (errcheck/contextcheck), pointer-passing (gocritic), and naming (revive). Existing tests are the lock-in. No HCL surface change. No CLI flag change. No event/log change. No new errors.

If any test fails after a fix in Step 2 or Step 3, that is a real bug exposed by the lint fix (e.g. a swallowed error that masked a regression). Fix it as part of this workstream and document in reviewer notes. Do not revert the lint fix.

## Reuse

- Existing [`make lint-go`](../../Makefile) and `make lint-baseline-check` targets — do not reimplement.
- Existing baseline tooling at [tools/lint-baseline/](../../tools/lint-baseline/).
- Existing burn-down doc format in [docs/contributing/lint-baseline.md](../../docs/contributing/lint-baseline.md).
- The `errcheck` / `contextcheck` / `gocritic` rule definitions in [.golangci.yml](../../.golangci.yml) — confirmed correct at v0.2.0; do not edit.

## Out of scope

- Splitting [workflow/compile_steps.go](../../workflow/compile_steps.go) — owned by [03](03-split-compile-steps.md).
- Splitting [internal/cli/apply.go](../../internal/cli/apply.go) — owned by [02](02-split-cli-apply.md).
- Splitting [internal/cli/localresume/resumer.go](../../internal/cli/localresume/resumer.go) or [internal/engine/node_step.go](../../internal/engine/node_step.go) — those splits happen as part of the rework workstreams that touch them, not this one.
- Adding new linter rules to [.golangci.yml](../../.golangci.yml). New rules are a Phase 4 concern.
- Editing generated proto files (`*.pb.go`) directly. Wire contract is immutable in this workstream.
- Removing `//nolint` comments outside the baseline file. Those are permanent inline exceptions added by past workstreams; not this workstream's territory unless one is provably wrong.

## Files this workstream may modify

- Any non-generated `*.go` file touched by an `errcheck`, `contextcheck`, or `gocritic` baseline entry.
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — entry removals only. **No new entries.**
- [`tools/lint-baseline/cap.txt`](../../tools/lint-baseline/cap.txt) — lower the cap to the new measured count.
- [`docs/contributing/lint-baseline.md`](../../docs/contributing/lint-baseline.md) — append the Phase 3 W01 burn-down section.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`.
- Any other workstream file in `workstreams/phase3/` or `workstreams/`.
- Generated proto files (`sdk/pb/criteria/v1/*.pb.go`).
- The complexity-baseline entries owned by sibling Track A workstreams (Step 5 list).
- [`.golangci.yml`](../../.golangci.yml) — rule configuration is immutable here.

## Tasks

- [x] Snapshot the starting baseline (Step 1).
- [x] Burn down all 9 `errcheck` entries (Step 2).
- [x] Burn down `contextcheck` to 0 entries (Step 3). _(All fixed: 7 via ctx threading; 2 final via new RunFailed/StepResumed ctx-bearing methods)_
- [x] Burn down `gocritic` to ≤ 8 entries (Step 4). _(1 hugeParam kept — applyOptions/W02; 4 fixed by pointer conversion; 3 dead entries removed)_
- [x] Confirm complexity entries are left for siblings and document the deferral (Step 5).
- [x] Triage `revive` entries to ≤ 4 (Step 6). _(0 remain)_
- [x] Lower `cap.txt` to the new measured count (Step 7). _(20)_
- [x] Append the Phase 3 W01 burn-down section to `docs/contributing/lint-baseline.md` (Step 8).
- [x] Validation (`make lint-go`, `make lint-baseline-check`, full test suite with race, `make ci`) (Step 9).

## Exit criteria

- `grep -c '^\s*- path:' .golangci.baseline.yml` returns ≤ 50.
- Zero `errcheck` entries in the baseline.
- Zero `contextcheck` entries in the baseline.
- ≤ 8 `gocritic` entries in the baseline.
- ≤ 4 `revive` entries in the baseline.
- `tools/lint-baseline/cap.txt` matches the measured entry count exactly.
- `make lint-go` exits 0.
- `make lint-baseline-check` exits 0.
- `make test -race -count=1` exits 0 across root, `sdk/`, and `workflow/`.
- `make ci` exits 0.
- `docs/contributing/lint-baseline.md` contains the new Phase 3 W01 section with accurate counts.

## Tests

The workstream adds targeted context contract tests to `internal/run/sink_test.go`:

- **`TestSink_RunFailed_InheritsContextValuesAndDetachesCancellation`**: creates a context carrying a value, cancels it, then calls `sink.RunFailed(canceledCtx, ...)`. Asserts the published context (a) is NOT canceled (WithoutCancel worked) and (b) retains the caller's value (not lost to Background). A broken `context.Background()` implementation fails assertion (b); omitting `WithoutCancel` fails assertion (a).
- **`TestSink_StepResumed_InheritsContextValuesAndDetachesCancellation`**: identical contract test for `StepResumed`.

Both tests use a new `contextCapturingPublisher` helper that records both the context and envelope from each `Publish` call.

The broader regression signals remain:

- `make ci` green proves the fixes did not break behavior.
- `make lint-go` green proves the baseline is consistent with the rules.
- `make lint-baseline-check` green proves the cap matches the count.

## Risks

| Risk | Mitigation |
|---|---|
| Threading `ctx` for `contextcheck` exposes a deadlock or cancellation regression | Run `make test -race -count=2` after Step 3; investigate any new test failure as a real correctness bug. Do not revert the threading. |
| Pointer-passing for `gocritic` hugeParam changes a struct's mutation semantics in a way callers depended on | Review every call site. If any caller relied on copy-by-value semantics, restructure that caller; do not revert the pointer change. |
| The complexity entries left for siblings (Step 5) accidentally get re-numbered/re-keyed during another workstream's edit, masking a regression | Each sibling workstream independently re-runs `make lint-baseline-check`; the cleanup gate (W21) re-asserts. Mitigation is not in this workstream. |
| `make lint-go` fails on a non-default build tag combination after a fix | Run `make ci` (which exercises the matrix); investigate any tag-specific failure as an inline `//nolint:<linter> // <reason>` rather than restoring the baseline entry. |
| The cap.txt drop from 70 → ≤ 50 collides with an in-flight Phase 3 PR that was assuming the higher cap | Phase 3 hasn't started other workstreams when this one runs (per Track A sequencing). If Track A workstreams interleave, run this one first. |

## Implementation Notes

### Starting baseline (v0.2.0)

```
Entries: 70  (errcheck:9, contextcheck:9, gocritic:24, revive:9, gocognit:7, gocyclo:6, funlen:6)
cap.txt: 70
```

### Final baseline (this workstream)

```
Entries: 20  (gocritic:1, gocognit:7, gocyclo:6, funlen:6)
cap.txt: 20
```

Per-rule changes:

| Linter | Before | After | Notes |
|---|---:|---:|---|
| `errcheck` | 9 | 0 | All fixed (discard `_` for best-effort cleanup paths) |
| `contextcheck` | 9 | 0 | 7 fixed by threading ctx; 2 final fixed via new RunFailed/StepResumed ctx-bearing methods |
| `gocritic` | 24 | 1 | 19 fixed (rangeValCopy, unnamedResult, emptyStringTest, builtinShadow, stringXbytes); 4 hugeParam fixed by pointer conversion; 1 hugeParam kept (applyOptions/W02); 3 dead entries removed |
| `revive` | 9 | 0 | All fixed (camelCase rename of internal-test functions) |
| `gocognit` | 7 | 7 | Deferred to W03 / W02 / W07 siblings |
| `gocyclo` | 6 | 6 | Deferred to W04 / W07 siblings |
| `funlen` | 6 | 6 | Deferred to W02 / W03 / W10 siblings |

### Kept entries with justification

**hugeParam (1 entry kept):**
- `internal/cli/apply.go` — `opts applyOptions` (208 bytes): `applyOptions` is threaded through 6 apply-command functions (`runApply`, `runApplyLocal`, `runApplyServer`, `executeServerRun`, `drainResumeCycles`, `drainLocalResumeCycles`). Converting all 6 to pointer is a broad refactor that belongs to W02-split-cli-apply.

**hugeParam (4 entries fixed by pointer conversion):**
- `eval.go` — `WithEachBinding(b EachBinding)` → `b *EachBinding`; callers updated with `&workflow.EachBinding{...}`.
- `internal/cli/apply.go` — `setupServerRun(clientOpts servertrans.Options)` → `*servertrans.Options`; caller uses `copts := applyClientOptions(opts); &copts`.
- `internal/cli/reattach.go` — 3 functions with `clientOpts servertrans.Options` → `*servertrans.Options`; `buildRecoveryClient` deferences with `*clientOpts`.
- `internal/transport/server/client.go` — `buildHTTPClient(u, o Options)` → `o *Options`; caller uses `&o`.

**contextcheck (0 entries kept):**
All 9 contextcheck findings are resolved. The 2 that remained after the first round (`OnRunFailed→publish`, `OnStepResumed→publish`) were fixed by adding `RunFailed(ctx, reason, step)` and `StepResumed(ctx, step, attempt, reason)` as new ctx-bearing methods on `run.Sink`. These call `publishWithCtx(ctx, ...)` directly, bypassing the `sinkCtx()` field. `reattach.go` callers updated to use the new methods. The `engine.Sink` interface remains unchanged (no breaking change required).

### Deferred complexity entries (left for siblings)

| Entry | Owner |
|---|---|
| `compileWaits`, `compileSteps` gocognit/gocyclo/funlen | [W03-split-compile-steps](03-split-compile-steps.md) |
| `compileBranches`, `compileForEachs` gocognit/gocyclo/funlen | [W03](03-split-compile-steps.md) + [W16-switch-flow](16-switch-and-if-flow-control.md) |
| `resolveTransitions`, `checkReachability` gocyclo/funlen | [W02-split-cli-apply](02-split-cli-apply.md) |
| `SerializeVarScope` gocognit/gocyclo/funlen | [W07-local-block-fold](07-local-block-and-fold-pass.md) / [W08](08-schema-unification.md) |

### Dead entries removed

1. `conformance/caller_ownership.go` tooManyResultsChecker — `ownershipSetup` returns exactly 5 values; gocritic fires for >5, so this was never a real finding.
2. `internal/adapter/conformance/conformance_lifecycle.go` hugeParam — function already had `//nolint:gocritic` on its signature.
3. `internal/adapter/conformance/conformance_outcomes.go` hugeParam — same.

### Notable fixes

- `sdk/conformance/ack.go:137`: second `stream.CloseRequest()` call uncovered by lint (was outside the originally-audited line range).
- `apply.go:292`: `context.WithTimeout(context.Background(), ...)` → `context.WithTimeout(context.WithoutCancel(ctx), ...)` — proper draining context now inherits the ambient request context.
- `internal/run/sink.go`: added `Ctx context.Context` field and `sinkCtx()` helper. `publish` uses `context.WithoutCancel(s.sinkCtx())`. All `run.Sink` constructors in CLI code now set `Ctx: ctx`.
- Named return `:=` gotcha: three functions (conformance_test.go, compile_test.go, cmd/criteria-adapter-mcp/conformance_test.go) had pre-existing named-return declarations; adding named returns to sibling functions required converting `:=` to `=` in bodies that re-assigned those names.

### Validation

```
make lint-go:              PASS (exit 0)
make lint-baseline-check:  PASS (20/20)
make lint-imports:         PASS (Import boundaries OK)
go test -race ./...:       PASS (all root packages ok)
(cd sdk && go test -race ./...):      PASS
(cd workflow && go test -race ./...): PASS
make ci:                   PASS (all targets including example run)
```

## Reviewer Notes (Round 2 Response)

All four reviewer blockers and the nit have been addressed:

**Blocker 1 — contextcheck entries removed:**
Added `RunFailed(ctx, reason, step)` and `StepResumed(ctx, step, attempt, reason)` as new ctx-bearing methods on `run.Sink`. These call `publishWithCtx(ctx, ...)` directly so contextcheck can trace the context chain without touching the `engine.Sink` interface. Updated `reattach.go` callers. Both contextcheck baseline entries removed. Zero contextcheck entries remain.

**Blocker 2 — gocritic hugeParam reconciled:**
Converted 4 entries to pointers (`eval.go`, `apply.go` clientOpts, `reattach.go` clientOpts, `client.go` o). One entry kept (`apply.go opts/applyOptions`) with accurate `# kept:` annotation and documented rationale. Inaccurate conformance/SDK claims removed from notes. Baseline: 1 hugeParam entry, cap: 20.

**Blocker 3 — context contract tests added:**
`internal/run/sink_test.go` now has `contextCapturingPublisher` + two contract tests (`TestSink_RunFailed_InheritsContextValuesAndDetachesCancellation`, `TestSink_StepResumed_InheritsContextValuesAndDetachesCancellation`). Both tests cancel the caller ctx, then assert the published ctx is (a) not canceled, (b) retains the caller's value. Would fail with `context.Background()` regression.

**Nit — validation notes updated:**
Implementation notes now record the full acceptance-bar sequence: `go test -race ./...` for all three modules plus `make ci`.

**Opportunistic fix:**
`internal/cli/apply_test.go:245` updated to pass `&servertrans.Options{}` (pointer) to match the `resumeInFlightRuns` signature change.

### Validation (Round 2)

```
make lint-go:              PASS (exit 0)
make lint-baseline-check:  PASS (20/20)
go test -race ./...:       PASS (root)
(cd sdk && go test -race ./...):      PASS
(cd workflow && go test -race ./...): PASS
make ci:                   PASS
```

Final baseline: 20 entries (from 70). Target was ≤ 50.

### Review 2026-05-02 — changes-requested

#### Summary

The branch clears the numeric cap and passes the validation sequence, but it does not meet the plan as written. Two `contextcheck` entries remain even though the exit criteria require zero, and the current `[ARCH-REVIEW]` rationale is not sufficient because the cited `engine.Sink` surface is explicitly internal (`internal/engine/engine.go:20-88`), not an SDK/public contract. The residual `gocritic` story is also internally inconsistent: the baseline still keeps `eval.go`, `internal/cli/apply.go`, `internal/cli/reattach.go`, and `internal/transport/server/client.go` entries (`.golangci.baseline.yml:80-109`), while both the workstream notes (`01-lint-baseline-burndown.md:274-285`) and the contributor doc (`docs/contributing/lint-baseline.md:198-218`) claim the survivors are different public/SDK entry points.

#### Plan Adherence

- **Step 2 (`errcheck`)**: implemented; baseline has zero `errcheck` entries.
- **Step 3 (`contextcheck`)**: **not complete**. `.golangci.baseline.yml:82-89` still carries two `contextcheck` suppressions, so the exit criterion "Zero `contextcheck` entries in the baseline" is unmet.
- **Step 4 (`gocritic`)**: numeric target is met, but the retained-entry justification is not. The file still keeps five `hugeParam` entries at `.golangci.baseline.yml:90-109`; they do not match the five public/SDK APIs claimed in `01-lint-baseline-burndown.md:276-281` and `docs/contributing/lint-baseline.md:198-205`.
- **Step 5 / Step 6 / Step 7**: deferred complexity entries, `revive` cleanup, and cap drop to `26` are consistent with the current baseline.
- **Step 8 (doc update)**: **not complete** because the kept-entry inventory is inaccurate and the baseline does not contain the required per-entry `# kept:` annotations for surviving `gocritic` items.
- **Step 9 (validation)**: the branch passes the intended validation sequence, but the implementation notes only record `make test` and omit the race suite / `make ci`.

#### Required Remediations

- **Blocker — remove the two residual `contextcheck` baseline entries or replace them with a justified, approved architecture exception.**  
  **Files:** `.golangci.baseline.yml:82-89`, `internal/engine/engine.go:20-88`, `internal/run/sink.go:34-68`, `internal/cli/reattach.go:165-186, 272-290`, `01-lint-baseline-burndown.md:283-285, 318-325`  
  **Why:** the workstream promises zero `contextcheck` entries. The current deferral says this is a "breaking SDK-level change", but the affected interface is internal to this repo, and the call sites/implementations are local. That is executor-owned work, not a demonstrated cross-repo architectural dependency.  
  **Acceptance:** make the two `contextcheck` findings disappear from the baseline and remove the invalid `[ARCH-REVIEW]` claim, or obtain an explicit human exception that revises the workstream scope/exit criteria.

- **Blocker — reconcile the residual `gocritic` inventory with the actual baseline, and add the required `# kept:` annotations for any survivor left intentionally.**  
  **Files:** `.golangci.baseline.yml:80-109`, `docs/contributing/lint-baseline.md:198-218`, `01-lint-baseline-burndown.md:274-281`  
  **Why:** the branch currently keeps `hugeParam` entries for `eval.go`, `internal/cli/apply.go` (2), `internal/cli/reattach.go`, and `internal/transport/server/client.go`, but the notes/docs claim the survivors are conformance/SDK entry points. This is inaccurate reviewer-facing documentation, and it also skips the Step 4 requirement to leave explicit `# kept:` comments above retained entries.  
  **Acceptance:** either fix the remaining `hugeParam` findings, or for each genuinely unavoidable survivor add a `# kept: <reason>` comment directly above the baseline entry and update both documents so the kept list matches the exact remaining entries by file and rationale.

- **Blocker — add tests that prove the new context-threading behavior, not just that publishing still happens.**  
  **Files:** `internal/run/sink_test.go:17-25, 99-143`, `internal/cli/reattach_test.go`, `cmd/criteria-adapter-mcp/*_test.go` as appropriate  
  **Why:** the workstream changed context semantics in `run.Sink.publish`, reattach/server drain paths, and MCP session shutdown, but the current tests do not assert the intended contract. `fakePublisher.Publish` discards the `context.Context`, so the tests cannot fail if the code regresses back to `context.Background()` or stops preserving ambient values while detaching cancellation.  
  **Acceptance:** add focused tests that assert the published/shutdown context inherits caller values while remaining usable after cancellation, and that a plausible broken implementation (`context.Background()` / lost ctx) would fail those tests.

- **Nit — make the implementation notes' validation section reflect the actual acceptance-bar commands.**  
  **Files:** `01-lint-baseline-burndown.md:309-316`  
  **Why:** the current notes only record `make test`, but the workstream exit criteria require the race suite across root/sdk/workflow plus `make ci`.  
  **Acceptance:** update the notes so they accurately record the validation that satisfies Step 9.

#### Test Intent Assessment

The existing suite gives decent regression coverage for "code still runs" and "events still publish", and the branch now passes lint, race tests, and `make ci`. What is missing is proof of the new context contract. The current `run.Sink` tests assert payload shape only; because the fake publisher ignores the `context.Context`, they would still pass if `publish` reverted to `context.Background()` or lost request-scoped values. That makes the context-threading changes weak on the regression-sensitivity rubric and insufficient for the specific behavior this workstream changed.

#### Validation Performed

- `make lint-go` — passed.
- `make lint-baseline-check` — passed (`26 / 26`).
- `go test -race -count=1 ./...` — passed.
- `(cd sdk && go test -race -count=1 ./...)` — passed.
- `(cd workflow && go test -race -count=1 ./...)` — passed.
- `make ci` — passed.

### Review 2026-05-02-02 — approved

#### Summary

The follow-up commit resolves the prior blockers and now meets the workstream exit criteria. The branch removes the last 2 `contextcheck` suppressions without widening the internal `engine.Sink` interface, reconciles the residual `gocritic` inventory down to a single documented `applyOptions` entry, and adds targeted tests that prove the new transport context contract. The measured baseline is now 20 entries, well below the ≤ 50 target.

#### Plan Adherence

- **Step 2 (`errcheck`)**: complete; no `errcheck` entries remain in `.golangci.baseline.yml`.
- **Step 3 (`contextcheck`)**: complete; baseline count is now zero, and the remaining reattach sites use `run.Sink.RunFailed(ctx, ...)` / `StepResumed(ctx, ...)` so the linter can trace the caller context directly.
- **Step 4 (`gocritic`)**: complete; four `hugeParam` findings were removed by pointer conversion and one residual `applyOptions` entry remains with a clear `# kept:` rationale tied to W02 scope.
- **Step 5 / Step 6 / Step 7**: complete; deferred complexity entries remain isolated to sibling workstreams, `revive` is at zero, and `cap.txt` matches the measured count (`20`).
- **Step 8 (doc update)**: complete; `docs/contributing/lint-baseline.md` and the implementation notes now match the actual residual baseline.
- **Step 9 (validation)**: complete; the workstream notes now reflect the full acceptance-bar sequence and the branch satisfies it.

#### Test Intent Assessment

The new `contextCapturingPublisher` tests are strong enough for the behavior that changed. They assert both required invariants at the transport boundary: published contexts retain caller-scoped values and do not inherit cancellation. A regression to `context.Background()` would lose the value assertion, and a regression that dropped `context.WithoutCancel` would fail the cancellation assertion. That closes the prior intent gap.

#### Validation Performed

- `make lint-go` — passed.
- `make lint-baseline-check` — passed (`20 / 20`).
- `go test -race -count=1 ./...` — passed.
- `(cd sdk && go test -race -count=1 ./...)` — passed.
- `(cd workflow && go test -race -count=1 ./...)` — passed.
- `make ci` — passed.
