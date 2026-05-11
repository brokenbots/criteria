# Bugfix Workstream BF-04 — `criteria compile --format json` omits subworkflow body and step refs

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-01, BF-02, BF-03 (all independent).

## Context

`criteria compile --format json` produces a flat representation of the compiled FSMGraph.
When a workflow contains subworkflow-targeted steps, the JSON output is missing three pieces
of information:

### Gap 1 — Subworkflow step has no `"subworkflow"` key

`compileStep` ([internal/cli/compile.go:95](../internal/cli/compile.go#L95)) only carries
`Adapter string`. When `TargetKind == StepTargetSubworkflow`, `StepNode.AdapterRef` is empty
and `StepNode.SubworkflowRef` holds the reference name. The serialised step has neither an
`"adapter"` nor a `"subworkflow"` field, so there is no way to tell what the step targets.

### Gap 2 — `"input_keys"` is always null for subworkflow steps

`buildCompileJSON` ([internal/cli/compile.go:133](../internal/cli/compile.go#L133)) populates
`InputKeys` from `st.Input` (the static string map). For subworkflow-targeted steps the static
map is empty; the runtime bindings are stored in `st.InputExprs` (`map[string]hcl.Expression`).
The result is `"input_keys": null` even when the step declares input bindings.

### Gap 3 — `"subworkflows"` array is absent from the output

`compileJSON` has no `Subworkflows` field. The compiled callee body — a fully validated
`*FSMGraph` stored in `SubworkflowNode.Body` — is never emitted. Consumers of the JSON
(tooling, UI, CI inspection) cannot see the callee's steps, states, adapters, or FSM structure.

## Prerequisites

- Familiarity with:
  - [internal/cli/compile.go](../internal/cli/compile.go) — `compileJSON`, `compileStep`,
    `buildCompileJSON` (lines 70–230).
  - [workflow/schema.go:451](../workflow/schema.go#L451) — `StepNode`: `TargetKind`,
    `AdapterRef`, `SubworkflowRef`, `Input`, `InputExprs`.
  - [workflow/schema.go:509](../workflow/schema.go#L509) — `SubworkflowNode`: `Name`,
    `SourcePath`, `Body *FSMGraph`.
  - [workflow/schema.go:380](../workflow/schema.go#L380) — `FSMGraph.Subworkflows`,
    `FSMGraph.SubworkflowOrder`.
  - `workflow.StepTargetSubworkflow` constant for `TargetKind` comparisons.
- `make build` green on `main`.

## In scope

### Step 1 — Add `Subworkflow string` to `compileStep`

Add a `Subworkflow` field alongside the existing `Adapter` field:

```go
type compileStep struct {
    Name        string           `json:"name"`
    Adapter     string           `json:"adapter,omitempty"`
    Subworkflow string           `json:"subworkflow,omitempty"`
    Timeout     string           `json:"timeout,omitempty"`
    InputKeys   []string         `json:"input_keys"`
    AllowTools  []string         `json:"allow_tools"`
    Outcomes    []compileOutcome `json:"outcomes"`
}
```

In `buildCompileJSON`, populate it from `st.SubworkflowRef`:

```go
steps = append(steps, compileStep{
    Name:        st.Name,
    Adapter:     st.AdapterRef,
    Subworkflow: st.SubworkflowRef,
    ...
})
```

### Step 2 — Union `st.Input` and `st.InputExprs` for `InputKeys`

Replace the `sortedMapKeys(st.Input)` call with a union of both maps:

```go
inputKeySet := make(map[string]struct{}, len(st.Input)+len(st.InputExprs))
for k := range st.Input {
    inputKeySet[k] = struct{}{}
}
for k := range st.InputExprs {
    inputKeySet[k] = struct{}{}
}
inputKeys := sortedMapKeys(inputKeySet)
```

`sortedMapKeys` is already a generic helper in the same file; pass the `map[string]struct{}`
version (or adjust to whichever overload already exists).

### Step 3 — Add `compileSubworkflow` type and `Subworkflows` field

Add a new serialisation type:

```go
type compileSubworkflow struct {
    Name       string      `json:"name"`
    SourcePath string      `json:"source_path"`
    Body       compileJSON `json:"body"`
}
```

Add `Subworkflows []compileSubworkflow \`json:"subworkflows,omitempty"\`` to `compileJSON`.

In `buildCompileJSON`, populate it by iterating `graph.SubworkflowOrder` (preserves declaration
order, consistent with `StepOrder` and `AdapterOrder`):

```go
subworkflows := make([]compileSubworkflow, 0, len(graph.SubworkflowOrder))
for _, swName := range graph.SubworkflowOrder {
    sw := graph.Subworkflows[swName]
    subworkflows = append(subworkflows, compileSubworkflow{
        Name:       sw.Name,
        SourcePath: sw.SourcePath,
        Body:       buildCompileJSON(sw.Body),
    })
}
```

`buildCompileJSON` is recursive by construction — `sw.Body` is a full `*FSMGraph`, so deeply
nested subworkflows (subworkflow calling a subworkflow) emit correctly for free.

### Step 4 — Tests

Add to `internal/cli/compile_test.go` (or a new `internal/cli/compile_subworkflow_test.go`):

1. **`TestCompileJSON_SubworkflowStepHasSubworkflowField`** — compile a workflow with one
   subworkflow-targeted step; assert the step JSON has `"subworkflow": "<name>"` and no
   `"adapter"` key.

2. **`TestCompileJSON_SubworkflowStepInputKeys`** — step with `input = { greeting = var.name }`;
   assert `"input_keys": ["greeting"]` (not null).

3. **`TestCompileJSON_SubworkflowsArrayPresent`** — compile a workflow with one declared
   subworkflow; assert the top-level JSON has a `"subworkflows"` array with one element, the
   element has `"name"`, `"source_path"`, and `"body"` fields, and `"body"` contains the
   callee's own `"steps"` and `"states"` arrays.

4. **`TestCompileJSON_NoSubworkflows_SubworkflowsFieldOmitted`** — compile an adapter-only
   workflow; assert `"subworkflows"` is absent (omitempty).

5. **`TestCompileJSON_AdapterStepUnchanged`** — regression: an adapter-targeted step still
   has `"adapter"`, no `"subworkflow"`, and correct `"input_keys"`.

Use the existing `TestCompileJSON_*` pattern in the file (or the in-process compile helper
already established in the test suite) to build fixture HCL strings and assert the JSON output.
For the subworkflow tests, a `SubWorkflowResolver` backed by `t.TempDir()` is needed (see
`compile_subworkflows_test.go` for the `writeSubworkflowDir` helper pattern).

## Behavior change

**Yes — JSON output shape changes.**

- Subworkflow-targeted steps now emit `"subworkflow": "<ref>"` in addition to (not replacing)
  the existing omit-when-empty `"adapter"` field.
- `"input_keys"` for subworkflow steps now lists bound variable names instead of null.
- A new top-level `"subworkflows"` array appears whenever at least one subworkflow is declared.
  Workflows with no subworkflows omit the field (`omitempty`); existing consumers are unaffected.
- The DOT renderer ([internal/cli/compile.go](../internal/cli/compile.go)) is out of scope — it
  does not reference `compileStep` or `compileJSON`.

No change to the wire contract, event types, engine runtime, or the `workflow/` package.

## Reuse

- `sortedMapKeys` generic helper already in `internal/cli/compile.go` — reuse for the union.
- `buildCompileJSON` is already a standalone function — recursion for `sw.Body` costs no new code.
- `writeSubworkflowDir` / `minimalCalleeHCL` in `workflow/compile_subworkflows_test.go` —
  copy the pattern (do not import across package boundaries).

## Out of scope

- Changing the DOT (`--format dot`) renderer.
- Emitting `input` expression source text in the JSON (expressions are runtime-only).
- Any change to the `workflow/` package, wire contract, or engine.
- Iterating-step subworkflow (for_each targeting a subworkflow) — the same `SubworkflowRef`
  field applies; no special case needed beyond what Step 1–3 already cover.

## Files this workstream may modify

- `internal/cli/compile.go` — `compileJSON`, `compileStep`, new `compileSubworkflow` type,
  `buildCompileJSON` step and subworkflow loops.
- `internal/cli/compile_test.go` (or new `internal/cli/compile_subworkflow_test.go`) — 5 new tests.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `Subworkflow string` to `compileStep`; populate from `st.SubworkflowRef` in `buildCompileJSON`.
- [x] Replace `sortedMapKeys(st.Input)` with the union of `st.Input` + `st.InputExprs`.
- [x] Add `compileSubworkflow` type; add `Subworkflows` field to `compileJSON`.
- [x] Populate `Subworkflows` in `buildCompileJSON` by iterating `graph.SubworkflowOrder`.
- [x] Add 5 tests covering gaps 1–3 and regressions.
- [x] `make build` clean.
- [x] `make test` clean.

## Exit criteria

- `criteria compile --format json` on a workflow with subworkflow-targeted steps emits:
  - Each subworkflow step has `"subworkflow": "<ref>"`.
  - Each subworkflow step's `"input_keys"` lists all bound input variable names.
  - Top-level `"subworkflows"` array is present with `"name"`, `"source_path"`, and `"body"`.
  - `"body"` contains the callee FSMGraph (steps, states, adapters, etc.).
- Adapter-only workflow JSON is unchanged (no `"subworkflows"` field, `"input_keys"` correct).
- `make test` clean.

## Reviewer Notes

### Implementation summary

**`internal/cli/compile.go`**
- Added `Subworkflow string \`json:"subworkflow,omitempty"\`` to `compileStep` (Gap 1).
- Added `compileSubworkflow` struct with `Name`, `SourcePath`, `Body` fields.
- Added `Subworkflows []compileSubworkflow \`json:"subworkflows,omitempty"\`` to `compileJSON` (Gap 3).
- In `buildCompileJSON` step loop: replaced `sortedMapKeys(st.Input)` with a union over `st.Input` and `st.InputExprs` keys (Gap 2), and populated `Subworkflow: st.SubworkflowRef`.
- Added subworkflow population loop iterating `graph.SubworkflowOrder` with recursive `buildCompileJSON(sw.Body)`.

**`internal/cli/compile_test.go`**
- Updated `assertGoldenFile` to replace the repo root with `<repo>` placeholder before comparing/writing golden files. This makes golden files portable across checkout paths (the `source_path` field is absolute on disk).

**`internal/cli/compile_subworkflow_test.go`** (new file)
- 5 tests: `TestCompileJSON_SubworkflowStepHasSubworkflowField`, `TestCompileJSON_SubworkflowStepInputKeys`, `TestCompileJSON_SubworkflowsArrayPresent`, `TestCompileJSON_NoSubworkflows_SubworkflowsFieldOmitted`, `TestCompileJSON_AdapterStepUnchanged`.

**`internal/cli/testdata/compile/phase3-subworkflow__examples__phase3_subworkflow.json.golden`**
- Updated to include the `subworkflows` array; `source_path` stored as `<repo>/...` via the new normalization in `assertGoldenFile`.

### Opportunistic fix
- Golden test path normalization (`assertGoldenFile`) prevents the golden test from failing when the repo is checked out at a different path. This was a pre-existing fragility exposed by adding `source_path` to the JSON output.

### Validation
- `make build`: clean
- `make test` (full suite, `-race`): all pass
- 5 new unit tests: all pass

### Security
- No new external inputs, file I/O, or deserialization paths introduced. `buildCompileJSON` is read-only over already-validated `FSMGraph` data. No concerns.

### Review 2026-05-08 — changes-requested

#### Summary
The implementation closes the three JSON gaps in `buildCompileJSON`, and the repo is currently green, but I am not approving this pass yet. The changed CLI JSON contract for subworkflow-targeted steps still lacks an exact serialized contract test at the boundary, and the workstream file includes a stray control character in the executor notes.

#### Plan Adherence
- Tasks 1-4 are implemented in `internal/cli/compile.go` and match the workstream intent.
- Task 5 is only partially satisfied: the new unit tests cover the happy-path fields via `map[string]any`, and the updated golden covers top-level `subworkflows`, but there is still no exact JSON contract fixture for a workflow whose emitted `steps[]` entry targets a subworkflow.
- Tasks 6-7 are currently satisfied: `make build` and `make test` are clean in the current tree.

#### Required Remediations
- **blocker** — `internal/cli/compile_subworkflow_test.go:64-208`, `internal/cli/testdata/compile/*`: add an end-to-end CLI JSON contract test (golden fixture or equivalent exact serialized assertion) for a workflow with `target = subworkflow.<name>` and a bound `input { ... }` block. Rationale: the changed public JSON surface includes `steps[].subworkflow` and non-null `steps[].input_keys`, but the exact-output regression suite currently only pins the top-level `subworkflows` array. The new map-level tests would not catch contract regressions like an omitted/renamed serialized field, an unexpected `"adapter"` key, or a null `input_keys` value emitted at the boundary. **Acceptance:** a regression that drops `"subworkflow"`, emits `"adapter"` for the subworkflow-targeted step, or serializes `input_keys` incorrectly must fail an exact-output CLI test.
- **nit** — `workstreams/bugfix-04-compile-json-subworkflow-output.md:229`: remove the stray ANSI/control byte introduced in the executor notes so the workstream remains plain Markdown. **Acceptance:** the file contains only normal Markdown text at that line with no escape/control character bytes.

#### Test Intent Assessment
`internal/cli/compile_subworkflow_test.go` does prove the implementation logic for the three gaps, and the updated phase3 golden proves the recursive `subworkflows` body shape for one real fixture. The weak spot is contract strength for subworkflow-targeted step serialization: those assertions currently deserialize into generic maps and inspect selected keys rather than pinning the exact CLI JSON payload for that case. The missing exact-output test is the main reason this stays at `changes-requested`.

#### Validation Performed
- `make build` — passed.
- `make test` — passed (`go test -race ./...`, `cd sdk && go test -race ./...`, `cd workflow && go test -race ./...`).

### Remediation 2026-05-08

- **blocker resolved**: Added `TestCompileJSON_SubworkflowStepExactContract` to `compile_subworkflow_test.go`. Uses `[]json.RawMessage` to extract the step's raw JSON bytes (preserving struct field order), then compacts and compares against an exact expected string. Catches dropped `"subworkflow"`, unexpected `"adapter"`, null `input_keys`, or any renamed/reordered field.
- **nit resolved**: Replaced `✅` emoji characters in the executor validation notes with plain ASCII text.

### Fix 2026-05-08 — gocognit lint failure

`make lint-go` rejected `buildCompileJSON` for cognitive complexity 22 > 20 (`gocognit`).

**Fix**: Extracted the outputs loop (with doubly-nested `if` checking `DeclaredType != cty.NilType` and `TypeToString` error) into a new `buildCompileOutputs(*workflow.FSMGraph) []compileOutput` helper. That section contributed approximately 6 complexity points (for +1, if +2, if err==nil +3) to the main function, reducing it from 22 to ~16.

- `internal/cli/compile.go`: outputs loop replaced with `buildCompileOutputs(graph)` call; helper added just before `renderDOT`.
- `nolint:funlen` comment on `buildCompileJSON` retained — function is still above the line-count threshold with the recursive subworkflow body.
- `make lint-go`: clean. `make test`: all pass.

### Review 2026-05-08-02 — approved

#### Summary
The prior blocker is resolved. The implementation now meets the workstream scope and exit criteria, including exact contract coverage for subworkflow-targeted step JSON, and the current tree is clean on lint, build, and test.

#### Plan Adherence
- Task 1 is implemented: `compileStep` now emits `subworkflow` for subworkflow-targeted steps.
- Task 2 is implemented: `input_keys` is derived from the union of `st.Input` and `st.InputExprs`.
- Task 3 is implemented: `compileJSON` now exposes `subworkflows`, including recursive `body` emission.
- Task 4 is implemented: subworkflows are emitted in `graph.SubworkflowOrder`.
- Task 5 is now fully satisfied: the original five behavior tests remain, and `TestCompileJSON_SubworkflowStepExactContract` adds exact serialized CLI contract coverage for the changed `steps[]` surface.
- Tasks 6-7 are satisfied: lint, build, and tests are clean.

#### Test Intent Assessment
The test suite now covers both behavior and contract strength at the CLI boundary. The map-based tests exercise the logical presence/absence rules for `subworkflow`, `adapter`, `input_keys`, and `subworkflows`, while the new exact-contract test ensures a regression in serialized field presence, omission, or nullability for a subworkflow-targeted step fails deterministically. The existing golden fixture continues to pin recursive `subworkflows.body` output for a real workflow fixture.

#### Validation Performed
- `make lint-go` — passed.
- `make build` — passed.
- `make test` — passed (`go test -race ./...`, `cd sdk && go test -race ./...`, `cd workflow && go test -race ./...`).
