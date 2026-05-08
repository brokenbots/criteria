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

- [ ] Add `Subworkflow string` to `compileStep`; populate from `st.SubworkflowRef` in `buildCompileJSON`.
- [ ] Replace `sortedMapKeys(st.Input)` with the union of `st.Input` + `st.InputExprs`.
- [ ] Add `compileSubworkflow` type; add `Subworkflows` field to `compileJSON`.
- [ ] Populate `Subworkflows` in `buildCompileJSON` by iterating `graph.SubworkflowOrder`.
- [ ] Add 5 tests covering gaps 1–3 and regressions.
- [ ] `make build` clean.
- [ ] `make test` clean.

## Exit criteria

- `criteria compile --format json` on a workflow with subworkflow-targeted steps emits:
  - Each subworkflow step has `"subworkflow": "<ref>"`.
  - Each subworkflow step's `"input_keys"` lists all bound input variable names.
  - Top-level `"subworkflows"` array is present with `"name"`, `"source_path"`, and `"body"`.
  - `"body"` contains the callee FSMGraph (steps, states, adapters, etc.).
- Adapter-only workflow JSON is unchanged (no `"subworkflows"` field, `"input_keys"` correct).
- `make test` clean.
