# Workstream 20 — Implicit input chaining (default `step.input` to previous step output)

**Phase:** 3 · **Track:** D · **Owner:** Workstream executor · **Depends on:** [14-universal-step-target.md](14-universal-step-target.md), [15-outcome-block-and-return.md](15-outcome-block-and-return.md). · **Unblocks:** none.

## Context

[proposed_hcl.hcl §4](../../proposed_hcl.hcl):

> **Implicit Input Chaining:** If the `input` block is omitted, the engine defaults to passing the exact `output` of the previous step as the input to the current step, creating clean functional pipelines.

This is purely ergonomic. A workflow that today must write:

```hcl
step "fetch" { target = adapter.shell.default; input = { url = var.url } }
step "process" {
    target = subworkflow.processor
    input  = { data = step.fetch.output }
}
```

can write:

```hcl
step "fetch" { target = adapter.shell.default; input = { url = var.url } }
step "process" { target = subworkflow.processor }   // input = step.fetch.output (implicit)
```

Conditions for implicit chaining:

1. `step.input` is omitted.
2. The step has exactly one inbound transition from another step (i.e. it's not a join point with multiple predecessors).
3. The previous step's output type is compatible with the current step's expected input shape (when the target is a subworkflow with declared variable types, type-checked at compile; when target is an adapter with a declared input schema, schema-validated).

When ambiguous (multiple predecessors, no obvious "previous"), the implicit chain is a compile error — the author must specify input explicitly.

## Prerequisites

- [14-universal-step-target.md](14-universal-step-target.md): universal target.
- [15-outcome-block-and-return.md](15-outcome-block-and-return.md): outcome output projection (the "previous step's output" can be the projected output, not the raw adapter output).
- `make ci` green.

## In scope

### Step 1 — Compile-time chain inference

In `workflow/compile_steps.go` (the dispatcher), after all steps are compiled with their explicit inputs:

```go
// inferImplicitInputs walks the graph, identifies steps with no input
// declaration, finds their unique inbound predecessor, and synthesizes
// an InputExprs map equivalent to { (predecessor's output keys) }.
// Steps with multiple inbound predecessors and no explicit input are
// errors. Steps with no inbound predecessors (entry points) and no
// explicit input default to an empty input map.
func inferImplicitInputs(g *FSMGraph) hcl.Diagnostics
```

Algorithm:

1. Build the predecessor map: for each step S, the set of nodes whose outcome `next` resolves to S.
2. For each step S where `S.InputExprs == nil` AND `S.HasExplicitInputDecl == false` (a flag set by the original input compile to distinguish "absent" from "empty"):
   - If exactly one predecessor P, synthesize `S.InputExprs = predecessorOutputExprs(P)`. The synthesized map references `step.<P.name>.output.<key>` for each key in P's declared output schema (or the projected `outcome.output` if P routes to S via a specific outcome with `output = ...`).
   - If S is the workflow's `InitialState`, leave `InputExprs` as empty (it has no predecessor).
   - Otherwise (zero or multiple predecessors), error: "step X has no explicit input and cannot infer chaining (X has Y predecessors)".

### Step 2 — Outcome projection awareness

If the predecessor's outgoing outcome to S has an `output = { ... }` projection ([15-outcome-block-and-return.md](15-outcome-block-and-return.md)), the implicit input is the **projected** output, not the predecessor's raw output. Use the outcome's projection map keys.

This is the consistent semantic: `step.<predecessor>.output` always refers to the effective output flowing out of the predecessor toward S. When no projection, that's the raw output; when projected, that's the projected map.

### Step 3 — Type-compatibility check

For subworkflow targets, the callee's `variable` declarations have explicit types ([13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md)). If the implicit chain produces a value whose shape doesn't match (missing required keys, type mismatch on declared keys), error at compile.

For adapter targets, the adapter's declared `InputSchema` ([workflow/schema.go AdapterInfo](../../workflow/schema.go#L151)) gives the expected shape. Validate at compile.

### Step 4 — Diagnostic clarity

When a step has zero or multiple predecessors and no input, the error must clearly indicate WHY. Format:

```
step "process" requires explicit input: it has 2 inbound transitions
(from "fetch_a" and "fetch_b") and implicit input chaining is ambiguous.
Add: input = { ... } to specify the merge.
```

For the no-predecessor case (entry-point step):

```
step "first" has no predecessor; implicit input chaining defaults to
an empty map. If this step requires input, declare it explicitly.
```

(The latter is a warning, not an error — a step with no predecessor can validly receive empty input. But surface the warning so authors aren't surprised.)

### Step 5 — Examples and docs

Update at least three existing examples to drop redundant `input = { x = step.foo.output.x, y = step.foo.output.y }` blocks where the chain is obvious. Document the inference rules in [docs/workflow.md](../../docs/workflow.md).

Add [examples/phase3-input-chaining/](../../examples/) demonstrating both the implicit chain and the explicit override.

### Step 6 — Tests

- `workflow/compile_input_chain_test.go`:
  - `TestImplicitChain_SinglePredecessor_Inferred`.
  - `TestImplicitChain_MultiplePredecessors_Error`.
  - `TestImplicitChain_NoPredecessor_EntryPoint_EmptyInput`.
  - `TestImplicitChain_OutcomeProjection_UsedAsInput`.
  - `TestImplicitChain_TypeMismatchSubworkflow_CompileError`.
  - `TestImplicitChain_TypeMismatchAdapter_CompileError`.
  - `TestImplicitChain_ExplicitInputOverridesImplicit`.

- End-to-end: examples updated.

### Step 7 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make ci
```

## Behavior change

**Behavior change: yes — additive but interacting with explicit-input case.**

Observable differences:

1. A step without `input = { ... }` and with exactly one inbound predecessor now compile-resolves to the predecessor's output (implicit).
2. A step without `input` and with multiple predecessors errors at compile (was "empty input" silently before).
3. Type mismatches that were silent runtime errors before are now compile errors.

Existing workflows with explicit `input = { ... }` are unaffected.

The "multiple predecessors" case is a behavior change for any v0.2.0 workflow that relied on the silent empty-input behavior; the migration message points to the explicit-input fix.

## Reuse

- Existing predecessor-graph computation in `compile_steps_*.go` (used for `warnBackEdges`).
- The output-schema lookup in [`workflow/compile_steps_workflow.go`](../../workflow/compile_steps_workflow.go) and [`workflow/compile_subworkflows.go`](../../workflow/compile_subworkflows.go) (from [13](13-subworkflow-block-and-resolver.md)).
- The adapter `InputSchema` lookup in [`workflow/schema.go`](../../workflow/schema.go) (`AdapterInfo`).
- Outcome projection from [15](15-outcome-block-and-return.md).

## Out of scope

- Implicit input from non-step predecessors (switch, wait, approval). Only step-to-step chaining.
- Auto-coercion or shape-flattening when types don't match exactly. Strict equality (with cty's `Convert`).
- Implicit input that flows through multiple hops (transitive chaining). Single-hop only.
- A `chain = false` opt-out for steps that want to make the empty-input behavior explicit. The explicit `input = {}` declaration is the opt-out.

## Files this workstream may modify

- New: `workflow/compile_input_chain.go`.
- The top-level compile entry — invoke `inferImplicitInputs` after all per-step inputs compile.
- [`workflow/schema.go`](../../workflow/schema.go) — possibly add `HasExplicitInputDecl bool` flag on `StepNode` to distinguish "absent" from "empty".
- Affected example HCL files in [`examples/`](../../examples/).
- Goldens.
- New: [`examples/phase3-input-chaining/`](../../examples/).
- New tests.
- [`docs/workflow.md`](../../docs/workflow.md) — implicit-chaining section.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.
- The runtime input-resolution path in [internal/engine/node_step.go](../../internal/engine/node_step.go) — the inference is compile-time only, so the runtime sees an explicit (synthesized) `InputExprs` map and behaves identically to an authored one.

## Tasks

- [ ] Implement `inferImplicitInputs` (Step 1).
- [ ] Outcome-projection awareness (Step 2).
- [ ] Type-compatibility checks (Step 3).
- [ ] Clear diagnostics (Step 4).
- [ ] Update examples; add new example (Step 5).
- [ ] Tests (Step 6).
- [ ] `make ci` green (Step 7).

## Exit criteria

- Single-predecessor steps without `input` compile-resolve to predecessor output.
- Multi-predecessor steps without `input` error at compile.
- Type mismatches surface at compile.
- Outcome projections feed implicit chains.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 6 list. Coverage: ≥ 90% on `compile_input_chain.go`.

## Risks

| Risk | Mitigation |
|---|---|
| Inferred chains produce a confusing diff in compile JSON output (synthesized inputs that didn't appear in the source) | The compile JSON output should distinguish "explicit" vs "inferred" inputs. Add a `_inferred: true` marker in the JSON dump for synthesized maps. Optional UX improvement. |
| Existing workflows that relied on silent empty-input compile-error after this lands | This is the documented behavior change. Surface the migration in the error message. |
| Type compatibility check is too strict and rejects valid coercions (e.g. `number` → `string`) | Use cty's `Convert` — it handles standard widening. The tests for `TypeMismatchSubworkflow` and `TypeMismatchAdapter` lock the strict cases. |
| Outcome-projection awareness creates subtle differences between "outcome with projection" and "outcome without projection" | The semantic is clear: the effective output is what flows toward the successor. Document with one example of each in [docs/workflow.md](../../docs/workflow.md). |
| Inferred input map shape changes when a workflow's predecessor changes (e.g. an outcome projection added later) | This is intentional — chains follow the actual graph. Workflow authors who want stable input shape declare it explicitly. |
