# test-02 — HCL parsing & eval coverage gaps

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** C (test buffer) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** [test-03-ci-coverage-gate.md](test-03-ci-coverage-gate.md) (test-03 establishes a coverage ratchet; test-02 raises the floor first).

## Context

Three functions in the workflow package are deeply load-bearing and weakly tested in isolation:

1. **`mergeSpecs`** at [workflow/parse_dir.go:177](../workflow/parse_dir.go#L177) — multi-file directory module merge with singleton-conflict detection. Quadruple-suppressed: `cyclop,gocognit,gocyclo,funlen` (W17). High-complexity, must-be-correct, sensitive to ordering and conflict semantics. Today it is exercised primarily through end-to-end `parse_dir_test.go` happy paths and via `criteria validate` in `make validate`. Negative-path coverage (conflict cases, partial overlap, alphabetical-merge edge cases) is thin.

2. **`SerializeVarScope`** at [workflow/eval.go:489](../workflow/eval.go#L489) — cursor-stack + variable-scope JSON serialization for crash-resume. Triple-suppressed in baseline: `gocognit,gocyclo,funlen` (W10). Today it is exercised end-to-end by crash-resume integration tests; round-trip semantics under nested iteration / mixed-type variables are not directly tested.

3. **`RestoreVarScope`** at [workflow/eval.go:552](../workflow/eval.go#L552) — paired inverse of `SerializeVarScope`. Suppressed inline: `gocognit` (W03). Same observation: round-trip coverage is by integration only.

These three are the **highest-risk deserialization / merge surfaces** in the workflow package. The adapter rework will not touch them directly, but Phase 4 may add a fourth iteration construct (`while`, see [feat-04-while-step-modifier.md](feat-04-while-step-modifier.md)) that serialises new state through `SerializeVarScope`. Strong unit tests on the round-trip surface protect that work.

This workstream adds **focused unit tests** for each of the three functions, plus negative-path tests for the parser entry points in [workflow/parse_legacy_reject.go](../workflow/parse_legacy_reject.go) which currently has no direct coverage of the rejection branches.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with the `workflow.Spec`, `workflow.FSMGraph`, `workflow.IterCursor`, `workflow.EachBinding` types in [workflow/schema.go](../workflow/schema.go).
- The functions targeted are still at the line numbers cited above. Re-confirm if the file has changed since this workstream was scoped:
  ```sh
  grep -n 'func mergeSpecs\|func SerializeVarScope\|func RestoreVarScope' workflow/
  ```

## In scope

### Step 1 — `mergeSpecs` round-trip and conflict tests

New file: `workflow/parse_dir_merge_test.go`. (The existing `parse_dir_test.go` covers happy paths; this new file is the focused merge-semantics suite.)

Required tests:

1. `TestMergeSpecs_SingletonConflict_WorkflowHeader_TwoFiles` — two files each declare a `workflow "x" { ... }` block. Assert: `mergeSpecs` returns a diagnostic whose summary names both source files and the keyword `workflow`. Use `hcl.DiagError` severity.

2. `TestMergeSpecs_SingletonConflict_Policy_TwoFiles` — two files each declare a `policy { ... }` block. Same diagnostic shape.

3. `TestMergeSpecs_SingletonConflict_Permissions_TwoFiles` — two files each declare `permissions { ... }`. Same.

4. `TestMergeSpecs_DuplicateNamedBlock_Step` — two files declare `step "build" { ... }` with the same name. Assert: diagnostic summary names the duplicate name and both source files.

5. `TestMergeSpecs_DuplicateNamedBlock_Adapter_DifferentTypes` — two files declare adapters with same name but different type label: `adapter "shell" "primary" { ... }` and `adapter "copilot" "primary" { ... }`. Assert: diagnostic — the adapter name is the singleton key regardless of type label.

6. `TestMergeSpecs_DuplicateNamedBlock_Adapter_SameTypeAndName` — two files declare the same `adapter "shell" "primary" { ... }`. Assert: same diagnostic.

7. `TestMergeSpecs_DistinctBlocksAcrossFiles_NoConflict` — file A has `step "a" { ... }`, file B has `step "b" { ... }`. Assert: merged spec has both steps; no diagnostics.

8. `TestMergeSpecs_AlphabeticalMergeOrder_DiagnosticsStable` — three files (`a.hcl`, `b.hcl`, `c.hcl`) each declaring distinct steps. Run merge twice, comparing the resulting `Spec.Steps` slice order. Assert: order is stable across runs (alphabetical by source filename).

9. `TestMergeSpecs_AlphabeticalMergeOrder_ConflictDiagnostic_StableSourceFile` — two files (`b.hcl`, `a.hcl`) both declare `step "build" { ... }`. Assert: the conflict diagnostic names `a.hcl` first as the "original" and `b.hcl` as the "duplicate" (alphabetical-priority semantics).

10. `TestMergeSpecs_EmptyDirectory_NoSpec_NoDiagnostics` — directory with no `.hcl` files. Assert: returns `nil, nil` (or whichever the documented "no spec" return is).

11. `TestMergeSpecs_SingleFile_NoMergeNeeded` — one file in the directory. Assert: returned `Spec` equals the parse of that single file (deep-equal via `cmp.Diff`).

12. `TestMergeSpecs_MultipleNonHCLFiles_Ignored` — directory with `foo.txt`, `bar.json`, plus one valid `.hcl`. Assert: only the `.hcl` file is parsed; non-`.hcl` files are silently skipped.

Each test constructs synthetic file content via `t.TempDir()` and `os.WriteFile`. Helper:

```go
func writeHCLFiles(t *testing.T, dir string, files map[string]string) {
    t.Helper()
    for name, content := range files {
        if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
            t.Fatalf("write %s: %v", name, err)
        }
    }
}
```

For each test, assert the diagnostic count, severity, and summary substring. Asserting on the entire summary string is brittle; use `strings.Contains` against the load-bearing tokens (block keyword, name, source file).

### Step 2 — `SerializeVarScope` / `RestoreVarScope` round-trip tests

New file: `workflow/eval_varscope_roundtrip_test.go`.

The contract: `RestoreVarScope(SerializeVarScope(vars, cursors...), g)` returns values equal to the input under structural comparison (cty values via `value.Equals`, IterCursors via `reflect.DeepEqual` after JSON marshal-roundtrip).

Required tests:

1. `TestVarScope_RoundTrip_EmptyScope` — `vars = map[string]cty.Value{}`, no cursors. Assert round-trip preserves emptiness.

2. `TestVarScope_RoundTrip_PrimitiveTypes` — vars with one of each primitive cty type: `cty.StringVal("hi")`, `cty.NumberIntVal(42)`, `cty.BoolVal(true)`. Assert each round-trips.

3. `TestVarScope_RoundTrip_ListAndMap` — vars with a `cty.ListVal([]cty.Value{...})` and a `cty.MapVal(map[string]cty.Value{...})`. Round-trip; assert `value.Equals` for each.

4. `TestVarScope_RoundTrip_NestedObject` — `cty.ObjectVal({"steps": cty.ObjectVal({"build": cty.ObjectVal({"output": cty.StringVal("ok")})})})`. Three-deep nesting, mixed types. Round-trip assertion.

5. `TestVarScope_RoundTrip_NullValue` — vars containing `cty.NullVal(cty.String)`. Round-trip; assert null preservation.

6. `TestVarScope_RoundTrip_UnknownValue_Errors` — vars containing `cty.UnknownVal(cty.String)`. Assert: `SerializeVarScope` returns a diagnostic-style error naming the unknown variable. (Unknown values are not serialisable — confirm against current behavior in [workflow/eval.go](../workflow/eval.go); if current code silently allows unknowns, that is a bug — file a follow-up.)

7. `TestVarScope_RoundTrip_SingleCursor_ForEach` — one `IterCursor` representing a `for_each = ["a","b","c"]` step paused at `Index = 1`. Round-trip; assert all `IterCursor` fields are preserved (Items, Keys, Index, Total, AnyFailed, InProgress, OnFailure, Prev, EarlyExit).

8. `TestVarScope_RoundTrip_NestedCursors` — two cursors representing an outer `for_each` over a list of lists, with the inner `for_each` paused. Assert the cursor stack order is preserved (outer first, inner second).

9. `TestVarScope_RoundTrip_CursorWithEachPrev` — cursor where `Prev` is a non-trivial cty value (e.g. an object). Assert `Prev` round-trips bit-equal.

10. `TestVarScope_RoundTrip_LargeScope_HandlesLengthEfficiently` — vars with 100 keys, each a small primitive. Assert round-trip succeeds and the JSON output is < 100 KiB. (Soft sanity — `SerializeVarScope` should not pathologically expand small inputs.)

11. `TestRestoreVarScope_MalformedJSON_ReturnsError` — pass `"{invalid"` to `RestoreVarScope`. Assert error is non-nil; error message names "invalid" or "parse" or "json".

12. `TestRestoreVarScope_UnknownStepReference_ReturnsError` — JSON references a step that does not exist in the provided `*FSMGraph`. Assert error names the missing step.

13. `TestRestoreVarScope_TypeMismatch_ReturnsError` — JSON declares `"foo": "string"` but the `FSMGraph`'s variable `foo` is typed `number`. Assert error.

For the round-trip helpers, define a small assertion utility:

```go
func assertCtyMapEqual(t *testing.T, want, got map[string]cty.Value) {
    t.Helper()
    if len(want) != len(got) {
        t.Fatalf("map length: want %d, got %d", len(want), len(got))
    }
    for k, wv := range want {
        gv, ok := got[k]
        if !ok {
            t.Errorf("missing key %q", k)
            continue
        }
        if !wv.RawEquals(gv) {
            t.Errorf("key %q: want %#v, got %#v", k, wv, gv)
        }
    }
}
```

`RawEquals` (not `Equals`) catches type-tag differences that `Equals` sometimes glosses over; this is correct for a round-trip test.

### Step 3 — `parse_legacy_reject.go` rejection-branch tests

`workflow/parse_legacy_reject.go` rejects pre-v0.3 syntax. The current test surface (find via `grep -l 'parse_legacy_reject\|legacyReject' workflow/*_test.go`) covers some but not all rejection cases.

New file (or extend existing): `workflow/parse_legacy_reject_test.go`.

Required tests — one per rejection branch in `parse_legacy_reject.go`. Read the file, identify each `if ...` that emits a diagnostic, write a test that triggers that branch:

For each branch:
1. Construct a synthetic HCL string that triggers the legacy syntax.
2. Call `Parse("test.hcl", []byte(hcl))`.
3. Assert: returned diagnostics are non-empty, contain `hcl.DiagError`, and the summary names the legacy keyword and points the user to the v0.3 replacement.

Examples (confirm against actual `parse_legacy_reject.go`):

- `TestLegacyReject_TopLevelAgentBlock` — `agent "x" { ... }` at top level (legacy; replaced by `adapter "<type>" "<name>"`).
- `TestLegacyReject_TransitionTo` — `outcome "x" { transition_to = "y" }` (legacy; replaced by `next = "y"`).
- `TestLegacyReject_StepConfigAttribute` — `step "x" { config = { ... } }` (legacy; replaced by `input { ... }` or `target = ...`).
- `TestLegacyReject_BranchBlock` — top-level `branch { ... }` block (legacy; replaced by `switch { ... }`).
- (Add one per remaining rejection branch — read the file to enumerate.)

Use the existing diagnostic-assertion helper if one exists in [workflow/](../workflow/); otherwise write `assertDiagnosticContains(t, diags, "summary substring")`.

### Step 4 — Coverage measurement

After Steps 1–3, measure the workflow-package coverage of the targeted functions:

```sh
go test -coverprofile=/tmp/test-02-cover.out ./workflow/...
go tool cover -func=/tmp/test-02-cover.out | grep -E 'mergeSpecs|SerializeVarScope|RestoreVarScope|parse_legacy_reject'
```

Targets:

| Function | Pre-workstream coverage | Target |
|---|---:|---:|
| `mergeSpecs` | (measure on main) | ≥ 90% |
| `SerializeVarScope` | (measure on main) | ≥ 90% |
| `RestoreVarScope` | (measure on main) | ≥ 90% |
| Each rejection branch in `parse_legacy_reject.go` | (varies) | 100% |

Record the pre/post coverage numbers in reviewer notes. If any target is missed, add the missing tests until the bar is met. The targets are non-negotiable — they are the contract for this workstream's delivery.

### Step 5 — Validation

```sh
go test -race -count=2 ./workflow/...
go test -coverprofile=/tmp/test-02-cover.out ./workflow/...
go tool cover -func=/tmp/test-02-cover.out | grep -E 'mergeSpecs|SerializeVarScope|RestoreVarScope'
make ci
```

All four must exit 0. The coverage report inspection is manual; document the per-function percentages in reviewer notes.

## Behavior change

**No behavior change.** This workstream adds tests only. No production source code is modified.

If a new test reveals a real bug in `mergeSpecs`, `SerializeVarScope`, `RestoreVarScope`, or `parse_legacy_reject.go`, the bug fix is in scope:
- Document the bug in reviewer notes.
- Add the failing test first (red), then fix the production code (green).
- Cap the additional production change at 50 lines per bug.
- If the bug is structural (> 50 lines to fix), open a follow-up workstream and mark the test `t.Skip("known bug — see workstream X")` so the test stays in the suite as a TODO marker (this is the one place a "known bug" skip is acceptable).

## Reuse

- Existing `workflow.Parse`, `workflow.ParseFile`, `workflow.parseDir` (or similar) entry points.
- `cty.ListVal`, `cty.ObjectVal`, `cty.StringVal`, etc. constructors from `github.com/zclconf/go-cty/cty`.
- `cty.Value.RawEquals` for round-trip equality — strictly compares type tag + value.
- Existing diagnostic-assertion patterns in `workflow/*_test.go`.
- `t.TempDir()` for synthetic file-based merge tests.
- `github.com/google/go-cmp/cmp` if it's already a dep — convenient for diff output on assertion failures.

## Out of scope

- Refactoring `mergeSpecs` to reduce its complexity score. The W17 baseline suppression is intentional; reducing complexity here is a separate workstream.
- Refactoring `SerializeVarScope` or `RestoreVarScope`. Same.
- Refactoring `parse_legacy_reject.go` beyond bug fixes uncovered by the new tests.
- Adding tests for other workflow functions not listed in the Context table.
- Performance benchmarking. The `LargeScope` test is a sanity check, not a benchmark.
- Tests for the engine consumer of these functions (e.g. crash-resume integration tests in `internal/engine/`). Out of scope.
- Changing the `IterCursor` shape or any other type.
- Changing the JSON schema emitted by `SerializeVarScope`.

## Files this workstream may modify

- New file: [`workflow/parse_dir_merge_test.go`](../workflow/) — Step 1 tests.
- New file: [`workflow/eval_varscope_roundtrip_test.go`](../workflow/) — Step 2 tests.
- New file: [`workflow/parse_legacy_reject_test.go`](../workflow/) — Step 3 tests. If a file with this name already exists, extend it instead of creating a new one.
- (Production code) Bug fixes only, capped at 50 lines per bug, per the Behavior-change section. Touched production files would be one of `workflow/parse_dir.go`, `workflow/eval.go`, `workflow/parse_legacy_reject.go`.
- [`go.mod`](../go.mod), [`go.sum`](../go.sum) — only if `github.com/google/go-cmp/cmp` is needed and not yet a dep; check first.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file outside the `workflow/` package.
- Generated proto files.
- [`.golangci.yml`](../.golangci.yml), [`.golangci.baseline.yml`](../.golangci.baseline.yml).
- `tools/lint-baseline/cap.txt`.

## Tasks

- [ ] Add the 12 `mergeSpecs` tests in `workflow/parse_dir_merge_test.go` (Step 1).
- [ ] Add the 13 `SerializeVarScope` / `RestoreVarScope` round-trip tests in `workflow/eval_varscope_roundtrip_test.go` (Step 2).
- [ ] Enumerate `parse_legacy_reject.go` rejection branches and add one test per branch (Step 3).
- [ ] Measure coverage and confirm targets (Step 4).
- [ ] Validation (Step 5).

## Exit criteria

- `mergeSpecs`, `SerializeVarScope`, `RestoreVarScope` each have ≥ 90% line coverage measured by `go tool cover`.
- Every rejection branch in `parse_legacy_reject.go` has a dedicated test.
- All new tests pass under `-race -count=2`.
- `make ci` exits 0.
- Reviewer notes contain pre/post coverage numbers for the four functions and a list of any production bugs uncovered + their fixes.
- No production code changes exceed 50 lines per bug discovered.
- No new `//nolint` directives added.

## Tests

The workstream IS tests. The exit-criteria coverage targets are the contract.

Coverage reports for `internal/engine/` and other downstream packages may shift as a side effect; this workstream does not gate on those — that's [test-03-ci-coverage-gate.md](test-03-ci-coverage-gate.md)'s territory.

## Risks

| Risk | Mitigation |
|---|---|
| `mergeSpecs` round-trip tests reveal a real ordering bug that production has been masking | The bug fix is in scope (≤ 50 lines). If the fix is bigger, defer to a follow-up workstream and `t.Skip` the test with a reason. |
| `SerializeVarScope`/`RestoreVarScope` round-trip is not bit-exact for some cty type (e.g. number precision) | If `RawEquals` fails for a legitimate semantic-equal case, document and use `Equals` for that specific test, justifying the relaxation. The contract is "correct round-trip", not "byte-identical JSON". |
| Coverage measurement varies across Go versions (the `cover` tool's line counting can shift) | Run on the version pinned in `go.mod`; document the version in reviewer notes. The 90% bar should hold across minor versions; if it doesn't, raise the floor manually. |
| The 12+13 test count produces noisy CI output | Use `t.Run` subtests where appropriate to group related cases (e.g. all primitive-type round-trips under one parent test). |
| `parse_legacy_reject.go` has more rejection branches than expected and the test count balloons | Cap at one test per rejection branch; if a branch has multiple equivalent triggers, one test suffices. The goal is branch coverage, not exhaustive trigger coverage. |
| The `LargeScope_HandlesLengthEfficiently` test is flaky on CI under load | The 100-key / 100KiB threshold is generous; if it flakes, raise the threshold by 50% rather than removing the test. The test is a sanity guard, not a benchmark. |
