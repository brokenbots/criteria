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

- [ ] Snapshot the starting baseline (Step 1).
- [ ] Burn down all 9 `errcheck` entries (Step 2).
- [ ] Burn down all 9 `contextcheck` entries (Step 3).
- [ ] Burn down `gocritic` to ≤ 8 entries (Step 4).
- [ ] Confirm complexity entries are left for siblings and document the deferral (Step 5).
- [ ] Triage `revive` entries to ≤ 4 (Step 6).
- [ ] Lower `cap.txt` to the new measured count (Step 7).
- [ ] Append the Phase 3 W01 burn-down section to `docs/contributing/lint-baseline.md` (Step 8).
- [ ] Validation (`make lint-go`, `make lint-baseline-check`, full test suite, `make ci`) (Step 9).

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

This workstream does not add tests. The signals are:

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
