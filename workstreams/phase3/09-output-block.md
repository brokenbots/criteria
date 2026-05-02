# Workstream 09 — Top-level `output "<name>"` block

**Phase:** 3 · **Track:** B · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md). · **Unblocks:** [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) (a `subworkflow` callee returns its `output` blocks back to the caller; the surface must exist), [15-outcome-block-and-return.md](15-outcome-block-and-return.md) (`return` outcome bubbles outputs upward).

## Context

[architecture_notes.md §3](../../architecture_notes.md) and [proposed_hcl.hcl](../../proposed_hcl.hcl) introduce `output "<name>" { ... }` as a top-level block. Today, top-level workflows have no first-class output declaration — values "leak" via implicit reading of `var.*` after the run. Inline `workflow { ... }` bodies have a body-scoped `output` block (per [workflow/schema.go:117 OutputSpec](../../workflow/schema.go#L117), [workflow/schema.go:125](../../workflow/schema.go#L125)) used to project iteration outputs. The two surfaces are different shapes today; they unify here.

After this workstream:

- A workflow's outputs are an explicit, named, runtime-evaluated set of cty values produced when the workflow reaches a terminal state.
- The shape is **identical** at top-level and inside an inline `step.workflow { }` body. This is a direct consequence of [08-schema-unification.md](08-schema-unification.md) (sub-workflow IS a Spec).
- For [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md), the caller of a `subworkflow` reads `subworkflow.<name>.output.<output_name>` to consume the callee's declarations.

## Prerequisites

- [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md) merged.
- `make ci` green on `main`.

## In scope

### Step 1 — Schema unification

[workflow/schema.go](../../workflow/schema.go) currently has `OutputSpec` (line 125) used only by inline bodies. Promote it to top-level:

```go
// Spec
type Spec struct {
    Name         string           `hcl:"name,label"`
    Version      string           `hcl:"version"`
    InitialState string           `hcl:"initial_state"`
    TargetState  string           `hcl:"target_state"`
    Variables    []VariableSpec   `hcl:"variable,block"`
    Locals       []LocalSpec      `hcl:"local,block"`     // from [07]
    Outputs      []OutputSpec     `hcl:"output,block"`    // <-- NEW
    Agents       []AgentSpec      `hcl:"agent,block"`
    Steps        []StepSpec       `hcl:"step,block"`
    States       []StateSpec      `hcl:"state,block"`
    Waits        []WaitSpec       `hcl:"wait,block"`
    Approvals    []ApprovalSpec   `hcl:"approval,block"`
    Branches     []BranchSpec     `hcl:"branch,block"`
    Policy       *PolicySpec      `hcl:"policy,block"`
    Permissions  *PermissionsSpec `hcl:"permissions,block"`
    SourceBytes  []byte
}
```

Extend `OutputSpec` to allow optional `description` and `type` declarations:

```go
type OutputSpec struct {
    Name        string   `hcl:"name,label"`
    Description string   `hcl:"description,optional"`
    TypeStr     string   `hcl:"type,optional"`   // optional explicit type for compile-check
    Remain      hcl.Body `hcl:",remain"`         // captures the "value" expression
}
```

Rule: exactly one attribute named `value` is required in `Remain`. Anything else is a compile error.

The `[]*OutputSpec` form on `WorkflowBodySpec` is gone (Step 1 of [08](08-schema-unification.md) deleted that struct). Inline bodies pick up the same `Spec.Outputs []OutputSpec` field automatically.

### Step 2 — Compile output declarations

New file `workflow/compile_outputs.go`:

```go
// compileOutputs decodes each output{ value=... } block, validates the value
// expression's free variables (must be in var/local/each/steps/shared_variable
// — all valid), folds-or-defers the value via FoldExpr, and stores the compiled
// output in g.Outputs.
//
// description and type are compile-resolved.
// The value expression is captured as hcl.Expression for runtime evaluation
// (it may reference steps.* which is runtime-only).
func compileOutputs(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

The compiled type:

```go
// FSMGraph
type FSMGraph struct {
    ...
    Outputs map[string]*OutputNode
    OutputOrder []string  // declaration order for stable iteration
}

type OutputNode struct {
    Name        string
    Description string
    DeclaredType cty.Type   // cty.NilType if unset
    Value       hcl.Expression
}
```

Validation passes:

1. Duplicate `output "name"` declarations → compile error.
2. The `value` attribute is required (the `description` and `type` attributes are optional).
3. `validateFoldableAttrs` is invoked on the `value` expression (per [07](07-local-block-and-fold-pass.md)). If the expression folds, the resulting value's type must match `DeclaredType` if it is set; otherwise `DeclaredType` is informational. If it doesn't fold (references runtime namespaces), defer.
4. If `TypeStr` is set, parse it via the existing variable-type parser (`workflow/types.go` or whatever resolves `string`/`number`/`bool`/`list(...)`/`map(...)`); store as `DeclaredType`.

### Step 3 — Runtime evaluation at terminal state

In [internal/engine/](../../internal/engine/), the engine's terminal-state handler currently has no output-evaluation pass for top-level workflows (only inline bodies). Add one.

Find the terminal-state handling site (likely in [internal/engine/engine.go](../../internal/engine/engine.go) or [internal/engine/node.go](../../internal/engine/node.go)). Before the engine returns "run finished" to the caller, evaluate every entry in `g.Outputs`:

```go
// evalRunOutputs evaluates each declared output expression against the final
// run state and returns the resolved values keyed by output name in
// declaration order.
func evalRunOutputs(g *workflow.FSMGraph, st *RunState) (map[string]cty.Value, error)
```

The evaluation context: `BuildEvalContextWithOpts(st.Vars, st.Locals, EvalOpts{...})` — same context the engine builds for step input expressions, which reads `var.*`, `local.*`, `steps.*`, and `each.*` (runtime-bound). If the eval errors, the run terminates with an output-evaluation error (`Status: failure`, descriptive event).

If a declared output's `DeclaredType` is set and the resolved value's type does not match, emit an error.

### Step 4 — Surface outputs in the run result

The current run-end signal (events / CLI output) emits a "run finished" event but not output values. After this workstream, the run-finished event payload includes the resolved outputs:

- Add a new event type: `run.outputs` (in [events/](../../events/) — find the canonical event-emit location). Payload: ordered list of `(name, value, declared_type)`.
- Local-mode console output prints outputs in concise mode after the terminal state line. JSON mode includes them in the `run.finished` envelope.
- Server-mode events stream the same `run.outputs` envelope to the orchestrator.

Proto change required if the wire envelope needs a new field. Coordinate with [proto/criteria/v1/](../../proto/criteria/v1/) — likely an additive field on `RunFinished` (or whatever envelope finalizes a run). Bump the SDK changelog.

### Step 5 — Update inline body output flow

Inline `step.workflow { ... output "x" { value = ... } }` blocks already exist (today's `WorkflowBodySpec.Outputs`). After this workstream, they go through the same `compileOutputs` path because the body IS a `Spec` ([08](08-schema-unification.md)). The body's `output` blocks are populated into the body's `g.Outputs`. The iteration finalizer reads those values and stores them as the step's per-iteration output (existing path in [internal/engine/node_step.go](../../internal/engine/node_step.go)).

The shape consolidation collapses two code paths into one. Confirm by removing any `OutputSpec`-on-body specific compile code that survived [08](08-schema-unification.md).

### Step 6 — Update CLI compile JSON output

`criteria compile --output json` produces a JSON representation of the compiled graph (see [internal/cli/compile.go](../../internal/cli/compile.go)). Add the outputs section:

```json
{
  "name": "...",
  "outputs": [
    { "name": "result_count", "type": "number", "description": "..." },
    ...
  ]
}
```

Goldens under [internal/cli/testdata/compile/](../../internal/cli/testdata/compile/) — regenerate for any example that adds an output.

### Step 7 — Examples

- Update at least three existing examples to declare `output` blocks. Pick examples where outputs are user-relevant (e.g. final summary count, generated artifact path).
- New example [examples/phase3-output/](../../examples/phase3-output/) demonstrating typed outputs and runtime-resolved expressions.

### Step 8 — Tests

- `workflow/compile_outputs_test.go`:
  - `TestCompileOutputs_Simple`.
  - `TestCompileOutputs_DuplicateName` — error.
  - `TestCompileOutputs_MissingValueAttr` — error.
  - `TestCompileOutputs_TypedOutput_FoldedMatch` — declared `type = "number"`, value folds to a number, success.
  - `TestCompileOutputs_TypedOutput_FoldedMismatch` → compile error.
  - `TestCompileOutputs_TypedOutput_DeferredValueFromSteps` — deferred to runtime; declared type stored.
  - `TestCompileOutputs_DependsOnLocal` — folds.
  - `TestCompileOutputs_OnlyValueAttr` — `description` and `type` are optional.

- `internal/engine/run_outputs_test.go`:
  - `TestEvalRunOutputs_StepOutputAccessible`.
  - `TestEvalRunOutputs_TypeMismatch` — declared `type = "string"`, runtime value is a number → run failure.
  - `TestEvalRunOutputs_EmptyOutputs` — graph with no outputs runs successfully.

- End-to-end CLI test: a workflow with two outputs runs and the JSON event stream includes a `run.outputs` envelope with both values.

### Step 9 — SDK conformance

If a proto field was added in Step 4, add a conformance assertion: a subject that finishes a run with declared outputs sees the `run.outputs` envelope and the values match. See [sdk/conformance/](../../sdk/conformance/) for the conformance harness pattern.

### Step 10 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/... ./internal/engine/... ./internal/cli/... ./sdk/...
make validate
make proto-check-drift   # if a proto field was added
make test-conformance
make lint-go
make lint-baseline-check
make ci
```

All exit 0.

## Behavior change

**Behavior change: yes — additive at the HCL surface; emits new events.**

Observable differences:

1. New top-level block `output "<name>" { value = ..., description = ..., type = ... }` is parseable. Existing workflows do not use it; no migration impact for that surface.
2. New event `run.outputs` is emitted at terminal state. SDK consumers MUST tolerate the new envelope (additive); the wire contract change is reviewed in Step 4.
3. CLI concise output prints outputs after the terminal-state line.
4. CLI JSON compile output includes an `outputs: [...]` section in graph dumps.

Inline bodies' existing `output` blocks keep working — internal compile path consolidates but surface is unchanged.

If a proto field was added in Step 4, increment the SDK CHANGELOG (deferred-edit note for [21](21-phase3-cleanup-gate.md) — this workstream may not edit `sdk/CHANGELOG.md`? Verify the workstream allowlist; if `sdk/CHANGELOG.md` is part of the SDK surface, this workstream may edit it because it's the additive-proto requirement, not a coordination-set file. Edit it.).

## Reuse

- [`OutputSpec`](../../workflow/schema.go#L125) — already present, just promoted to top level and extended.
- The body's existing output-evaluation site in [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — generalize, do not duplicate.
- `BuildEvalContextWithOpts` (extended in [07](07-local-block-and-fold-pass.md)).
- `validateFoldableAttrs` — for the value-expression compile validation.
- The variable-type parser used by `VariableSpec.TypeStr`.
- Existing event-emission infrastructure in [events/](../../events/).
- Existing CLI compile JSON serialization in [internal/cli/compile.go](../../internal/cli/compile.go).

## Out of scope

- `subworkflow.<name>.output.<output_name>` namespace. Owned by [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) — this workstream lays the production side; the consumption side ships with the subworkflow block.
- Rewriting CHANGELOG.md release notes (coordination set; owned by [21](21-phase3-cleanup-gate.md)).
- The `return` outcome bubbling outputs to caller. Owned by [15-outcome-block-and-return.md](15-outcome-block-and-return.md).
- Streaming partial outputs during the run. Outputs are emitted at terminal state only.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — extend `OutputSpec`, add `Spec.Outputs`, add `FSMGraph.Outputs` + `FSMGraph.OutputOrder`, add `OutputNode`.
- New: `workflow/compile_outputs.go`.
- The top-level compile entry in [`workflow/compile.go`](../../workflow/compile.go) (or wherever `Compile` / `compileSpec` lives) — invoke `compileOutputs`.
- `workflow/compile_steps_workflow.go` — confirm body outputs feed through the unified path; remove any duplicated body-output compile code.
- [`internal/engine/`](../../internal/engine/) — terminal-state output-evaluation pass; new `evalRunOutputs` helper.
- [`internal/engine/node_step.go`](../../internal/engine/node_step.go) — body-iteration output evaluation pass aligned with the new engine helper.
- [`events/`](../../events/) — new `run.outputs` event type.
- [`internal/cli/compile.go`](../../internal/cli/compile.go) — add `outputs` section to JSON dump.
- [`internal/cli/testdata/compile/`](../../internal/cli/testdata/compile/) and [`internal/cli/testdata/plan/`](../../internal/cli/testdata/plan/) — regenerate goldens.
- [`proto/criteria/v1/`](../../proto/criteria/v1/) — additive field on `RunFinished` (or equivalent envelope) if Step 4 requires.
- [`sdk/CHANGELOG.md`](../../sdk/CHANGELOG.md) — additive change entry, since the proto bump is part of the SDK contract.
- [`sdk/conformance/`](../../sdk/conformance/) — new conformance assertion (Step 9) if proto field was added.
- New tests under [`workflow/`](../../workflow/) and [`internal/engine/`](../../internal/engine/).
- New: [`examples/phase3-output/`](../../examples/) plus updates to existing examples.

This workstream may **not** edit:

- [`PLAN.md`](../../PLAN.md), [`README.md`](../../README.md), [`AGENTS.md`](../../AGENTS.md), [`CHANGELOG.md`](../../CHANGELOG.md), [`workstreams/README.md`](../README.md), or any other workstream file.
- `agent` block / `AgentSpec` — owned by [11](11-agent-to-adapter-rename.md).
- `WorkflowBodySpec` — already deleted by [08](08-schema-unification.md).

## Tasks

- [ ] Promote `OutputSpec` to top-level; extend with `description` and `type` (Step 1).
- [ ] Implement `compileOutputs` (Step 2).
- [ ] Add terminal-state output evaluation pass (Step 3).
- [ ] Add `run.outputs` event; wire CLI concise/JSON output (Step 4).
- [ ] Consolidate body-output compile path (Step 5).
- [ ] Update CLI compile JSON output (Step 6).
- [ ] Update three existing examples; add new `examples/phase3-output/` (Step 7).
- [ ] Author all required tests (Step 8).
- [ ] Add conformance assertion if proto field landed (Step 9).
- [ ] `make ci`, `make proto-check-drift`, `make test-conformance` green (Step 10).

## Exit criteria

- `output "<name>" { value = ... }` parses and compiles at top level.
- `description` and `type` attributes are optional and validated.
- Duplicate names error at compile.
- A workflow with declared outputs emits a `run.outputs` event at terminal state.
- CLI concise output prints outputs; JSON output includes them.
- Inline body `output` blocks consolidate through the same code path (no duplicated output-compile code).
- All required tests pass.
- `make validate` green for every example.
- `make proto-check-drift` green if a proto change was made.
- `make ci` exits 0.

## Tests

The Step 8 test list is the deliverable. Coverage targets:

- `workflow/compile_outputs.go` ≥ 90% line coverage.
- The new `evalRunOutputs` helper ≥ 90% line coverage.

## Risks

| Risk | Mitigation |
|---|---|
| Adding a proto field on `RunFinished` breaks orchestrators that pin to v0.2.0 SDK | The field is additive and protobuf-tolerant. Bump the SDK CHANGELOG with a clear "additive — clients can ignore" note. |
| `run.outputs` event ordering relative to `run.finished` matters for downstream consumers | Decide explicitly in Step 4: outputs MUST be emitted before `run.finished`. Document in event reference docs. Add a conformance test that asserts the order. |
| `DeclaredType` validation is too strict and rejects values that cty would normally widen (e.g. `int → number`) | Use cty's existing `Convert` with type assertion (not raw `.Type() != DeclaredType`); same logic as `VariableSpec` type check. |
| The engine terminal-state path is reached from multiple sites and the output-eval call is missed in one | Search for every "run finished" emission point (likely 2–3 sites: terminal state, error path, cancellation); cancellation does NOT evaluate outputs (terminal state only). Document in reviewer notes. |
| Output expressions referencing `steps.foo.bar` where `steps.foo` did not run produce a confusing error | Make the error specific: "output X references step Y which did not execute in this run". Add a test for this case. |
