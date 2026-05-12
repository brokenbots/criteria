# td-01 — Lint baseline ratchet 24 → 16

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** B (tech debt) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** every other Track B/C/D workstream that adds new code (the cap is currently at 24/24, so any new lint hit fails CI until headroom exists).

## Context

The Phase-3 cleanup gate ([archived/v3/01-lint-baseline-burndown.md](archived/v3/01-lint-baseline-burndown.md)) closed with `tools/lint-baseline/cap.txt` at exactly **20**, then it crept to 24 across the v0.3.x patch releases as W11/W12/W13/W16 added complexity. The cap is now at the count, which means the very next lint hit fails CI. Phase 4 (adapter rework) is large and will inevitably introduce new complexity findings; we need headroom before opening that surgery.

This workstream burns down **8 entries** from the current 24 to land at exactly **16**, then drops `cap.txt` to 16. The deletions are targeted at extractable functions in `workflow/compile*.go` and ctx-threading findings in `internal/cli/`. SDK conformance entries (W12 lines 94/98/102) and the deeply-load-bearing `SerializeVarScope` complexity (W10 lines 44/48/52) are explicitly **out of scope** here — they are intrinsic complexity, not extractable, and rewriting them is a separate workstream.

The current 24 entries break down as:

| Owner | Lines in `.golangci.baseline.yml` | Entries | Rule(s) | Category |
|---|---|---:|---|---|
| W04 (compile_nodes.go) | 4–19 | 4 | gocognit×2, funlen, gocyclo | Extractable function complexity |
| W04 (compile.go) | 20–43 | 6 | gocognit×2, funlen×2, gocyclo×2 | Extractable function complexity |
| W10 (eval.go SerializeVarScope) | 44–55 | 3 | gocognit, gocyclo, funlen | **Intrinsic — out of scope** |
| W13 gocritic hugeParam (applyOptions) | 56–60 | 1 | gocritic | Pointer-conversion refactor (W02-split-cli-apply scope) |
| W13 contextcheck | 61–73 | 3 | contextcheck | ctx-threading fix |
| W13 compileSubworkflows | 74–82 | 2 | gocognit, funlen | Extractable function complexity |
| W16 nodeTargets / compileSwitchConditionBlock | 83–92 | 2 | gocognit, funlen | Small extractions |
| W12 SDK conformance lifecycle | 93–105 | 3 | gocognit, funlen×2 | **Intrinsic — out of scope** |
| **Total** | | **24** | | |

**Target deletions (exactly 8):**

1. The 3 W13 `contextcheck` entries (`internal/cli/apply_setup.go`, `internal/cli/compile.go`, `internal/cli/reattach.go`) — fixed by threading the caller `ctx` through `compileSubworkflows` and friends.
2. The 3 W04 entries on `compile.go::checkReachability` (gocognit, gocyclo, funlen) — fixed by extracting helpers.
3. The 2 W13 entries on `compileSubworkflows` (gocognit, funlen) — fixed by extracting validation phases.

That is **8 entries removed**, landing the baseline at exactly **16**.

If a chosen entry resists removal (e.g. `checkReachability` cannot be cleanly split without behavior risk), substitute another entry of equivalent count from the table above (W04 `compile_nodes.go::compileForEachs` is the second-best candidate at 3 entries). Document the substitution in reviewer notes. The end count must be 16; this is the contract.

## Prerequisites

- `make ci` green on `main`.
- `tools/lint-baseline/cap.txt` reads `24`. Confirm before any change:
  ```sh
  cat tools/lint-baseline/cap.txt   # expect: 24
  grep -c '^\s*- path:' .golangci.baseline.yml   # expect: 24
  ```
  If either differs, stop and reconcile against `main` before any edit.
- `golangci-lint` installed at the version `make lint-go` invokes.

## In scope

### Step 1 — Snapshot the starting state

Run from repo root and record the output in reviewer notes:

```sh
make lint-baseline-check
grep -c '^\s*- path:' .golangci.baseline.yml
grep -oE 'linters:\s*\n\s+-\s+\w+' .golangci.baseline.yml | sort | uniq -c
```

Expected: `24/24`, exactly 24 `- path:` entries, the rule distribution from the Context table.

### Step 2 — Burn down `contextcheck` (target: 0 entries; drops 3)

Three `contextcheck` entries flag `compileSubworkflows`-related call sites that pass `context.Background()` instead of threading the caller `ctx`. Locate each:

- `internal/cli/apply_setup.go` — find the call that triggers `should pass the context parameter`. Likely a call into a compile helper. Thread the caller's `ctx`.
- `internal/cli/compile.go` — same pattern.
- `internal/cli/reattach.go` — same pattern.

For each:

1. Find the caller via `grep -n "context.Background()" <file>`.
2. Identify the wrapping function. If it already accepts `ctx context.Context`, simply pass `ctx` instead of `context.Background()`. If it does not, add `ctx context.Context` as the first parameter and update all call sites in the same module.
3. If a call genuinely needs detached cancellation (background cleanup outliving the request), use `context.WithoutCancel(ctx)` and add a one-line comment: `// detached so background subworkflow compile survives request cancellation`. Do NOT use `context.Background()`. Do NOT add `//nolint:contextcheck`.
4. Run `make lint-go` after each fix; confirm the entry count drops by 1.
5. Remove the corresponding entry block from `.golangci.baseline.yml`.

If a `contextcheck` fix transitively breaks a test (e.g. a test that relied on detached behavior), fix the test to use the new signature; do not revert the lint fix. Document the test change in reviewer notes.

### Step 3 — Burn down `checkReachability` complexity (target: 0 entries on this function; drops 3)

`checkReachability` in [workflow/compile.go](../workflow/compile.go) (find via `grep -n 'func checkReachability' workflow/compile.go`) has 3 baseline entries: `gocognit`, `gocyclo`, `funlen`.

Refactor by extracting helpers. Likely shape (confirm against the actual code):

- `func collectReachableNodes(g *FSMGraph, start string) map[string]bool` — BFS from `start`, returns the reachable set.
- `func diagnoseUnreachableSteps(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics` — for each step not in `reachable`, emit a diagnostic.
- `func diagnoseUnreachableStates(g *FSMGraph, reachable map[string]bool) hcl.Diagnostics` — same for states.
- `func checkReachability(g *FSMGraph) hcl.Diagnostics` — orchestrator that calls the three helpers and `append`s their diagnostics.

Constraints:
- Each helper ≤ 50 lines (the `funlen` cap).
- No behavior change. The diagnostics emitted (count, severity, summary text, source range) MUST match the pre-refactor output exactly. The existing reachability tests are the lock-in.
- The helpers can be unexported; place them in the same file unless the file is itself flirting with `funlen` after the change (in which case split into `compile_reachability.go`).

Run `make lint-go` and confirm the 3 `checkReachability` entries can be removed. Remove them from `.golangci.baseline.yml`.

If the refactor exposes a behavior bug (e.g. a stale diagnostic that was masked by the previous shape), the bug is in scope: fix it and add a regression test. Do not revert the refactor.

### Step 4 — Burn down `compileSubworkflows` complexity (target: 0 entries; drops 2)

`compileSubworkflows` (find file via `grep -rn 'func compileSubworkflows' workflow/`) has `gocognit` and `funlen` baseline entries.

Refactor by extracting:

- `func validateSubworkflowSourcePaths(specs []*SubworkflowSpec, opts CompileOpts) hcl.Diagnostics` — confines path traversal, checks existence.
- `func detectSubworkflowCycle(refs map[string][]string) hcl.Diagnostics` — pure cycle detection on the dependency graph.
- `func parseSubworkflowSourceFile(path string, opts CompileOpts) (*Spec, hcl.Diagnostics)` — single-file parse + early validation.

The orchestrator `compileSubworkflows` then calls these in sequence. Same constraints as Step 3 (≤ 50 lines per helper, no behavior change, existing tests are the lock-in).

Remove the 2 entries from `.golangci.baseline.yml` after `make lint-go` confirms they no longer fire.

### Step 5 — Substitution policy if a target resists removal

If Step 3 or Step 4 cannot land the targeted deletions cleanly (e.g. the extraction would require touching public API or tests that this workstream's scope cannot absorb), pick replacement entries from this priority-ordered fallback list:

1. **W04 `compile_nodes.go::compileForEachs`** (lines 8/12/16 in baseline, 3 entries: gocognit/funlen/gocyclo). Extract per-iteration validation into a helper.
2. **W04 `compile_nodes.go::compileWaits`** (line 4 in baseline, 1 entry: gocognit). Extract wait-attribute validation into a helper.
3. **W04 `compile.go::resolveTransitions`** (lines 20/28/36 in baseline, 3 entries: gocognit/funlen/gocyclo). Extract per-target resolution.
4. **W16 `compile_steps_graph.go::nodeTargets`** (line 84 in baseline, 1 entry: gocognit). Small switch-case extraction.
5. **W16 `compile_switches.go::compileSwitchConditionBlock`** (line 89 in baseline, 1 entry: funlen). Extract attribute decoding from value validation.

Pick the smallest combination that lands the count at exactly 8 deletions. Document the substitution in reviewer notes with one sentence per swap.

### Step 6 — Lower `tools/lint-baseline/cap.txt` to 16

After Steps 2–5, count the remaining baseline entries:

```sh
grep -c '^\s*- path:' .golangci.baseline.yml
```

Expected: 16 exactly. If 17, find one more entry to remove. If 15, the workstream over-delivered — document in reviewer notes; the lower count is acceptable (set the cap to the actual count).

Update `tools/lint-baseline/cap.txt`:

```sh
echo 16 > tools/lint-baseline/cap.txt   # or the actual lower count if Step 5 over-delivered
```

The cap MUST equal the count exactly. Tracking the cap one above the count "to give room" is forbidden by the cap-stays-flat contract from [archived/v2/02-lint-ci-gate.md](archived/v2/02-lint-ci-gate.md).

Run `make lint-baseline-check` and confirm green.

### Step 7 — Append a burn-down entry to `docs/contributing/lint-baseline.md`

This file is the historical log of baseline burn-downs. Find the most recent section (likely "Phase 3 W01") and append a new section:

```markdown
## td-01 (pre-Phase-4) — 2026-MM-DD

- **Starting count:** 24
- **Final count:** 16
- **Cap:** 24 → 16

### Removed entries

| Linter | Function | File | Reason |
|---|---|---|---|
| contextcheck | (apply_setup.go call site) | internal/cli/apply_setup.go | Threaded caller ctx through. |
| contextcheck | (compile.go call site) | internal/cli/compile.go | Threaded caller ctx through. |
| contextcheck | (reattach.go call site) | internal/cli/reattach.go | Threaded caller ctx through. |
| gocognit, gocyclo, funlen | checkReachability | workflow/compile.go | Extracted collectReachableNodes / diagnoseUnreachableSteps / diagnoseUnreachableStates helpers. |
| gocognit, funlen | compileSubworkflows | workflow/compile_subworkflows.go | Extracted validateSubworkflowSourcePaths / detectSubworkflowCycle / parseSubworkflowSourceFile helpers. |

### Kept entries (16 remaining)

(Brief one-line note per remaining entry, citing owner workstream.)
```

Use the actual function names and file paths from the work done. The "Reason" column is one sentence per row.

### Step 8 — Validation

```sh
make lint-go
make lint-baseline-check
go test -race -count=1 ./...
(cd sdk && go test -race -count=1 ./...)
(cd workflow && go test -race -count=1 ./...)
make ci
```

All six must exit 0. Inspect:

- `tools/lint-baseline/cap.txt` reads `16`.
- `grep -c '^\s*- path:' .golangci.baseline.yml` returns `16`.
- No new `//nolint` directives were added inline (this workstream is lowering suppression, not relocating it). Verify with:
  ```sh
  git diff main -- '*.go' | grep '^+.*//nolint' && echo "FAIL: new nolint directive added" || echo "OK"
  ```

## Behavior change

**No behavior change.** This workstream is mechanical refactoring (function extraction) and ctx-threading. The only observable differences are internal:

- Function call graphs in `workflow/compile.go` and `workflow/compile_subworkflows.go` are flatter (helpers extracted).
- Three `internal/cli/` functions now accept and forward `ctx context.Context` (or already did and now use it instead of `context.Background()`).

No HCL surface change. No CLI flag change. No event/log change. No new error messages. Existing tests are the lock-in for behavior preservation.

If a test fails after a refactor in Step 3 or Step 4, that is a real bug exposed by the cleanup (e.g. a swallowed reachability case, a context that was being detached unintentionally). Fix it as part of this workstream and add a regression test. Do not revert the refactor.

## Reuse

- Existing [`make lint-go`](../Makefile) and `make lint-baseline-check` targets — do not reimplement.
- Existing baseline tooling at [tools/lint-baseline/](../tools/lint-baseline/).
- Existing burn-down doc format in [docs/contributing/lint-baseline.md](../docs/contributing/lint-baseline.md) — match the established Phase 1 / Phase 3 W01 section structure.
- The `errcheck` / `contextcheck` / `gocritic` rule definitions in [.golangci.yml](../.golangci.yml) — confirmed correct at v0.3.0; do not edit.
- The function-extraction patterns established in archived/v3 W03 (compile_steps split) and archived/v3 W02 (cli apply split) — same patterns apply here.

## Out of scope

- The W10 `SerializeVarScope` entries (3 entries on lines 44/48/52). Cursor-stack serialization complexity is intrinsic; rewriting it is a separate workstream.
- The W12 SDK conformance lifecycle entries (3 entries on lines 93/98/102). Test infrastructure complexity; rewriting is a separate workstream.
- The W13 `applyOptions` `gocritic` hugeParam entry (line 57). Conversion to pointer requires the W02-split-cli-apply refactor scope; documented in `archived/v3/01-lint-baseline-burndown.md` as deferred.
- Adding new linter rules to [.golangci.yml](../.golangci.yml). Rule changes are a Phase 4 concern.
- Editing generated proto files (`*.pb.go`) directly. Wire contract is immutable in this workstream.
- Removing `//nolint` directives outside the baseline file. Inline suppressions are owned by [td-02-nolint-suppression-sweep.md](td-02-nolint-suppression-sweep.md).
- Burning down past 16. The target is a precise number (16); over-delivery is acceptable per Step 6 but not the goal.

## Files this workstream may modify

- [`workflow/compile.go`](../workflow/compile.go) — extract `checkReachability` helpers.
- (Optional) New file `workflow/compile_reachability.go` — only if the helpers don't fit cleanly in `compile.go`.
- [`workflow/compile_subworkflows.go`](../workflow/compile_subworkflows.go) — extract validation helpers.
- (Optional) New file `workflow/compile_subworkflows_validate.go` — only if the helpers don't fit cleanly in `compile_subworkflows.go`.
- [`internal/cli/apply_setup.go`](../internal/cli/apply_setup.go), [`internal/cli/compile.go`](../internal/cli/compile.go), [`internal/cli/reattach.go`](../internal/cli/reattach.go) — ctx threading.
- Any test file under `workflow/` or `internal/cli/` that needs signature updates after Step 2 or Step 3.
- [`.golangci.baseline.yml`](../.golangci.baseline.yml) — entry removals only. **No new entries.**
- [`tools/lint-baseline/cap.txt`](../tools/lint-baseline/cap.txt) — set to 16 (or the actual lower count).
- [`docs/contributing/lint-baseline.md`](../docs/contributing/lint-baseline.md) — append the new burn-down section per Step 7.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files (`sdk/pb/criteria/v1/*.pb.go`).
- The W10 / W12 / W13-applyOptions baseline entries (Out of scope).
- [`.golangci.yml`](../.golangci.yml) — rule configuration is immutable here.
- Files in `cmd/criteria-adapter-*/` (no changes required for this workstream's scope).

## Tasks

- [x] Snapshot the starting state (Step 1).
- [x] Burn down all 3 `contextcheck` entries by ctx threading (Step 2).
- [x] Refactor `checkReachability` and remove its 3 baseline entries (Step 3).
- [x] Refactor `compileSubworkflows` and remove its 2 baseline entries (Step 4).
- [x] Lower `tools/lint-baseline/cap.txt` to 16 (Step 6).
- [x] Append the burn-down section to `docs/contributing/lint-baseline.md` (Step 7).
- [x] Validation (Step 8).

## Reviewer Notes

### Step 1 — Snapshot confirmed
- `tools/lint-baseline/cap.txt` = 24
- `grep -c '^\s*- path:' .golangci.baseline.yml` = 24 ✓
- `make lint-baseline-check` → `Lint baseline within cap (24 / 24).`
- Linter distribution in starting baseline:

| Linter | Count |
|--------|------:|
| `gocognit` | 8 |
| `funlen` | 8 |
| `gocyclo` | 4 |
| `contextcheck` | 3 |
| `gocritic` | 1 |
| **Total** | **24** |

### Step 2 — contextcheck fix (3 entries removed)

**Approach taken (post-reviewer-remediation):** Added `CompileWithContext(ctx, spec, schemas, opts)` as a new
exported function that carries the context through to `compileSubworkflows` → `ResolveSource`. The existing
`CompileWithOpts(spec, schemas, opts)` is kept as a backward-compatible wrapper that calls
`CompileWithContext(context.Background(), ...)` — its signature is **unchanged**. `Compile(spec, schemas)`
is also unchanged (calls `CompileWithOpts`).

This preserves the public API for all existing external callers while giving internal CLI callers the ability
to propagate their request context explicitly via `workflow.CompileWithContext(ctx, ...)`.

**Call sites updated:**
- `workflow/compile.go`: `Compile()` unchanged; `CompileWithOpts` is now a 1-line backward-compat wrapper; new `CompileWithContext` is the implementation; `compileSubworkflows` call updated
- `workflow/compile_subworkflows.go`: `compileSubworkflows(ctx, g, spec, opts)` + recursive call updated to `CompileWithContext`
- `internal/cli/apply_setup.go`: `workflow.CompileWithContext(ctx, spec, schemas, opts)`
- `internal/cli/compile.go`: same
- `internal/cli/reattach.go`: same
- `internal/cli/validate.go`: same
- 6 workflow test files reverted to `CompileWithOpts(spec, nil, opts)` (no ctx arg); `"context"` import removed from those files
- `workflow/compile_subworkflows_test.go`: 4 existing calls reverted to `CompileWithOpts(spec, nil, opts)`; `"context"` import retained for new tests

No `//nolint` added. `make lint-go` confirmed contextcheck entries gone.

### Step 8 — Context propagation tests added

Added two focused tests in `workflow/compile_subworkflows_test.go`:

1. **`TestCompileWithContext_ContextPropagation`**: Defines a stub `recordingResolver` that wraps
   `LocalSubWorkflowResolver` and records the context passed to each `ResolveSource` call. Calls
   `CompileWithContext` with a context carrying a sentinel value. Asserts the resolver received that
   exact context (sentinel present on every call). Proves caller context reaches the resolver boundary.

2. **`TestCompileWithContext_CancellationPropagates`**: Calls `CompileWithContext` with a pre-cancelled
   context. Asserts the cancelled context reached `ResolveSource` — proving the compiler does not mask
   cancellation by substituting `context.Background()`.

Both tests pass. `make test` exit 0.

### Step 3 — checkReachability refactor (3 entries removed)

Created `workflow/compile_reachability.go` with:
- `collectReachableNodes(g, start)` — iterative BFS, reuses existing `nodeTargets(name, g)` from
  `compile_steps_graph.go` (no duplication)
- `diagnoseUnreachableSteps(g, reachable)` — error per unreachable step
- `diagnoseUnreachableNodes(g, reachable)` — warning per unreachable wait/approval/switch/state

`checkReachability` in `compile.go` became a 4-line orchestrator. Removed `"strings"` import from
`compile.go` (no longer needed). Behavior identical to pre-refactor.

### Step 4 — compileSubworkflows refactor (2 entries removed)

Extracted from `compile_subworkflows.go`:
- `missingResolverDiags(subworkflows)` — error per subworkflow when resolver is nil
- `compileSingleSubworkflow(ctx, g, swSpec, opts, seenNames)` — inner loop body (~47 lines ≤ 50)
- `buildChildOpts(opts, resolvedDir)` — builds child CompileOpts for recursive call
- `detectSubworkflowCycle(resolvedDir, chain)` — returns `*hcl.Diagnostic` or nil

`compileSubworkflows` became a 16-line orchestrator. Also removed intermediate `declaredVars` copy
(was `make(map[string]*VariableNode)` + loop) — now passes `calleeGraph.Variables` directly.
Fixed `appendAssign` gocritic warning in `buildChildOpts`.

### Step 5 — No substitutions needed
All 8 target entries removed as planned; no fallback substitutions required.

### Validation
- `make lint-go` → exit 0
- `make lint-baseline-check` → "Lint baseline within cap (16 / 16)."
- `go test -race -count=1 ./...` → all packages pass
- `cd sdk && go test -race -count=1 ./...` → pass
- `cd workflow && go test -race -count=1 ./...` → pass
- `make test` → exit 0
- No new `//nolint` directives added (verified)
- `grep -c '^\s*- path:' .golangci.baseline.yml` = 16
- `tools/lint-baseline/cap.txt` = 16

## Exit criteria

- `grep -c '^\s*- path:' .golangci.baseline.yml` returns exactly `16`.
- `tools/lint-baseline/cap.txt` reads `16` (or the actual lower count if over-delivered).
- Zero `contextcheck` entries in the baseline.
- `checkReachability` has zero baseline entries.
- `compileSubworkflows` has zero baseline entries.
- `make lint-go` exits 0.
- `make lint-baseline-check` exits 0.
- `go test -race -count=1` exits 0 across root, `sdk/`, and `workflow/`.
- `make ci` exits 0.
- No new `//nolint` directives added inline (verified via diff).
- `docs/contributing/lint-baseline.md` contains the new td-01 section with accurate counts.

## Tests

This workstream is "no behavior change." The existing test suite is the lock-in.

Specifically required:

- `workflow/compile_test.go` already covers `checkReachability` outcomes. Run `go test -run 'Reachability|Reachable' ./workflow` and confirm green both before and after the refactor. If pre-refactor output differs from post-refactor for any case, that is a regression — fix the refactor.
- `workflow/compile_subworkflows_test.go` similarly covers `compileSubworkflows`. Same drill.
- For each `contextcheck` fix that changes a function signature, the corresponding test in `internal/cli/*_test.go` is updated; run `go test ./internal/cli/...` after each.

If `checkReachability` or `compileSubworkflows` lacks a regression test for a behavior the refactor depends on, **add one** before the refactor (test-first) so the lock-in is real. Document the added test in reviewer notes.

## Risks

| Risk | Mitigation |
|---|---|
| `checkReachability` extraction subtly changes diagnostic ordering, breaking a test that asserts specific diag indices | The existing tests assert message content and source range, not order. If any test does assert order, fix the test to be order-insensitive (sort diagnostics by source range) — that is a real fragility and the cleanup exposes it. |
| `compileSubworkflows` extraction changes the order in which subworkflow files are parsed, surfacing a hidden dependency on that order | Subworkflow parsing should be order-independent by design. If a test fails because of order, it has been masking a real bug; the bug is in scope. |
| `contextcheck` fix in `internal/cli/reattach.go` causes a reattach goroutine to terminate when the parent ctx is cancelled, breaking unattended-mode behavior | Reattach is intentionally detached from the request lifecycle. Use `context.WithoutCancel(ctx)` if so. The test `TestReattach_SurvivesParentCancellation` (or equivalent) is the lock-in; if it doesn't exist, add it. |
| The ratchet to 16 is reached but a subsequent merge from `main` brings the count back to 17 (e.g. an in-flight PR) | Run `make lint-baseline-check` immediately before merge; if the count differs from 16, rebase and re-extract one more entry to land exactly at the cap. |
| A refactor accidentally introduces a new `//nolint` directive | The Step 8 verification step (`git diff` for `+.*//nolint`) catches this. If a directive is genuinely needed, the work belongs in [td-02-nolint-suppression-sweep.md](td-02-nolint-suppression-sweep.md) instead. |

## Reviewer Notes

### Review 2026-05-12 — changes-requested

#### Summary
The baseline ratchet itself lands at 16/16 and the full validation suite is green, but this pass does **not** meet the acceptance bar yet. Two blockers remain: the implementation breaks the exported `workflow.CompileWithOpts` API to thread context, and the required td-01 burn-down entry in `docs/contributing/lint-baseline.md` does not match the workstream's mandated format/content. There is also a coverage gap on the new context-threading behavior and the Step 1 snapshot evidence is incomplete in the workstream notes.

#### Plan Adherence
- **Step 1:** Not fully satisfied. The workstream notes record the starting cap/count, but they omit the requested `make lint-baseline-check` output and linter distribution snapshot.
- **Step 2:** Functionally implemented, but not acceptably. Context is now threaded to subworkflow resolution, yet it was done by changing the exported `CompileWithOpts` signature instead of preserving the existing public API.
- **Step 3:** Implemented. `checkReachability` was flattened into helpers and the three baseline entries were removed.
- **Step 4:** Implemented. `compileSubworkflows` was split into helpers and the two baseline entries were removed.
- **Step 6 / Step 8:** Implemented. The baseline count and cap are both 16, and the required validation commands pass on the current tree.
- **Step 7:** Not satisfied. The new doc entry does not use the required td-01 heading/date, does not include the required removed-entries table, and does not enumerate the 16 kept entries.

#### Required Remediations
- **Blocker — `workflow/compile.go:56-68` and all `CompileWithOpts` call sites/tests updated in this patch.** The workstream turned `CompileWithOpts` into a breaking API change by adding a required `context.Context` parameter to an exported function in the standalone `workflow` module. The `workflow_test` package already proves there are external-package callers. The plan called for ctx threading through compile helpers, not a public API break. **Acceptance:** restore backwards compatibility for `CompileWithOpts(spec, schemas, opts)` while still propagating caller context to subworkflow resolution through a non-breaking path (for example an option field or private helper), update callers/tests accordingly, and rerun the full validation suite.
- **Blocker — `docs/contributing/lint-baseline.md:228-264`.** Step 7 required a td-01 burn-down entry with the specified heading/date, starting/final/cap bullets, a `### Removed entries` table, and a `### Kept entries (16 remaining)` section with one line per remaining entry citing owner workstream. The current prose summary does not satisfy that contract. **Acceptance:** rewrite this td-01 section to match the required structure exactly, using the actual function/file names removed and a one-line note for each of the 16 remaining baseline entries.
- **Blocker — `workflow/compile_subworkflows_test.go:53-64`, `136-138`, `525-527`, `711-713` (coverage gap).** Step 2's core behavioral change is caller-context propagation into the `SubWorkflowResolver` boundary, but the tests only adapted call sites to the new invocation shape. They do not prove that the resolver receives the caller context or that cancellation is no longer masked by `context.Background()`. **Acceptance:** add a focused compile/subworkflow test with a stub resolver that records the incoming context and proves the intended caller context reaches `ResolveSource`; include a failure-path assertion that would regress if the compiler fell back to `context.Background()` again.
- **Nit — `workstreams/td-01-lint-baseline-ratchet.md:253-255`.** The Step 1 snapshot notes are incomplete: they omit the requested `make lint-baseline-check` result and linter distribution breakdown. **Acceptance:** append the missing starting-state evidence to the workstream notes so Step 1 is fully documented.

#### Test Intent Assessment
The reachability and subworkflow refactors are generally well covered by the existing workflow tests plus the full repo validation pass; those tests are plausibly regression-sensitive for the mechanical helper extractions. The weak spot is the context-threading change: the current tests prove only that callers were rewritten to compile, not that the compiler now forwards the caller context across the resolver interface or preserves the intended cancellation semantics. The remediation above needs a focused contract-style test at that boundary.

#### Validation Performed
- `make lint-go` — passed
- `make lint-baseline-check` — passed (`16 / 16`)
- `go test -race -count=1 ./...` — passed
- `(cd sdk && go test -race -count=1 ./...)` — passed
- `(cd workflow && go test -race -count=1 ./...)` — passed
- `make ci` — passed
- `git diff main -- '*.go' | grep '^+.*//nolint'` — no new inline `//nolint` directives found

### Remediation 2026-05-12

All three blockers and the nit have been addressed:

**Blocker 1 (API break) — resolved.** Restored `CompileWithOpts(spec, schemas, opts)` as a
backward-compatible wrapper. Added `CompileWithContext(ctx, spec, schemas, opts)` as the new exported
context-bearing function. CLI callers updated to `CompileWithContext`. All 6 workflow test files that were
incorrectly updated to pass `context.Background()` as the first arg have been reverted to the original
`CompileWithOpts(spec, nil, opts)` signature with `"context"` import removed. Build and tests pass.

**Blocker 2 (doc format) — resolved.** Rewrote the td-01 section in `docs/contributing/lint-baseline.md`
to match the required structure: heading with date, starting/final/cap bullets, `### Removed entries`
table (8 rows), `### Kept entries (16 remaining)` with one line per entry citing owner workstream.

**Blocker 3 (coverage gap) — resolved.** Added `TestCompileWithContext_ContextPropagation` and
`TestCompileWithContext_CancellationPropagates` in `workflow/compile_subworkflows_test.go` using a
`recordingResolver` stub. Both tests pass; see Step 8 notes above for details.

**Nit (Step 1 evidence) — resolved.** Added `make lint-baseline-check` output and full linter
distribution table to Step 1 notes above.

#### Validation after remediation
- `make lint-go` → exit 0
- `make lint-baseline-check` → `Lint baseline within cap (16 / 16).`
- `go test ./workflow/... -run TestCompileWithContext` → PASS (2 tests)
- `make test` → exit 0 (all packages pass)
- `git diff main -- '*.go' | grep '^+.*//nolint'` → empty (no new inline nolint directives)

### Review 2026-05-12-02 — approved

#### Summary
The executor addressed the prior blockers. `CompileWithOpts` is backward-compatible again via a wrapper, the context-bearing path is isolated in `CompileWithContext`, the td-01 burn-down entry now records the removed and kept baseline entries, and focused tests now verify that caller context reaches the `SubWorkflowResolver` boundary. The workstream now meets the acceptance bar.

#### Plan Adherence
- **Step 1:** Satisfied. The starting snapshot now includes `make lint-baseline-check`, the 24-entry count, and the per-linter distribution.
- **Step 2:** Satisfied. The three `contextcheck` entries were removed without breaking the existing `CompileWithOpts` API; CLI callers use `CompileWithContext`.
- **Step 3:** Satisfied. `checkReachability` was reduced to an orchestrator and its three baseline entries were removed.
- **Step 4:** Satisfied. `compileSubworkflows` was flattened into helpers and its two baseline entries were removed.
- **Step 6:** Satisfied. `.golangci.baseline.yml` and `tools/lint-baseline/cap.txt` both land at 16.
- **Step 7:** Satisfied. `docs/contributing/lint-baseline.md` now contains the td-01 burn-down entry with removed-entry details and the 16 kept entries.
- **Step 8:** Satisfied. The required validation suite passes on the current tree.

#### Test Intent Assessment
The new `recordingResolver` tests are appropriately contract-focused: they assert that the exact caller context reaches `ResolveSource` and that a cancelled caller context is not silently replaced with `context.Background()`. Those assertions would fail on the prior broken implementation, so they are regression-sensitive for the behavior this workstream changed.

#### Validation Performed
- `make lint-go` — passed
- `make lint-baseline-check` — passed (`16 / 16`)
- `go test -race -count=1 ./...` — passed
- `(cd sdk && go test -race -count=1 ./...)` — passed
- `(cd workflow && go test -race -count=1 ./...)` — passed
- `make ci` — passed
- `git diff main -- '*.go' | grep '^+.*//nolint'` — no new inline `//nolint` directives found
