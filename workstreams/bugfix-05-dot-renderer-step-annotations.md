# Bugfix Workstream BF-05 — DOT renderer does not annotate iterating or subworkflow steps

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-04 (independent).

## Context

`criteria compile --format dot` produces a Graphviz DOT graph. Currently
`renderDOT` ([internal/cli/compile.go:218](../internal/cli/compile.go#L218)) renders every step
node identically:

```dot
"build_artifacts" [shape=box];
"run_tests"       [shape=box];
```

Two categories of step carry structure that is invisible in the current output:

### Gap 1 — Iterating steps

`StepNode` carries three mutually exclusive iteration fields
([workflow/schema.go:488](../workflow/schema.go#L488)):

| Field | Populated when |
|---|---|
| `ForEach hcl.Expression` | `for_each = <expr>` on the step |
| `Count hcl.Expression` | `count = <expr>` on the step |
| `Parallel hcl.Expression` | `parallel = <expr>` on the step |

All three are `nil` for a plain step. When non-nil the step runs multiple times (sequentially
for `for_each`/`count`, concurrently for `parallel`). The DOT graph currently gives no
indication of this — a step that fans out over a list looks identical to one that executes once.
This makes the graph misleading for workflows where iteration is load-bearing (e.g. a parallel
fan-out followed by a merge switch).

### Gap 2 — Subworkflow steps

`StepNode.SubworkflowRef string` is non-empty when the step delegates to a declared
subworkflow (`target = subworkflow.<name>`). These steps have no adapter; their body is an
entirely separate FSMGraph. The DOT output gives no indication of the delegation.

### Proposed annotations

The simplest Graphviz-compatible approach that requires no HTML labels is to append a
bracketed annotation to the node `label`:

| Step kind | Node declaration |
|---|---|
| Plain adapter | `"step_name" [shape=box];` *(unchanged)* |
| for_each | `"step_name" [shape=box, label="step_name\n[for_each]"];` |
| count | `"step_name" [shape=box, label="step_name\n[count]"];` |
| parallel | `"step_name" [shape=box, label="step_name\n[parallel]"];` |
| subworkflow | `"step_name" [shape=component, label="step_name\n[→ subwf_name]"];` |

Using `shape=component` for subworkflow steps distinguishes them visually from adapter steps
without requiring any HTML label changes. The `label` override is only emitted when the step
is non-plain; plain steps continue to use the Graphviz default (the node ID is the label).

Iterating subworkflow steps (for_each targeting a subworkflow) should show both annotations,
e.g. `label="step_name\n[for_each]\n[→ subwf_name]"`.

## Prerequisites

- Familiarity with:
  - [internal/cli/compile.go:218](../internal/cli/compile.go#L218) — `renderDOT`.
  - [workflow/schema.go:451](../workflow/schema.go#L451) — `StepNode`: `ForEach`, `Count`,
    `Parallel` (`hcl.Expression`, nil when absent), `SubworkflowRef` (empty when absent),
    `TargetKind`.
  - Graphviz DOT attribute syntax (`label`, `shape`, `\n` for line breaks in labels).
- `make build` green on `main`.

## In scope

### Step 1 — Annotate step nodes in `renderDOT`

Replace the current unconditional step node loop:

```go
for _, name := range graph.StepOrder() {
    b.WriteString(fmt.Sprintf("  %q [shape=box];\n", name))
}
```

with a loop that inspects `StepNode` fields and builds the annotation:

```go
for _, name := range graph.StepOrder() {
    st := graph.Steps[name]
    attrs := dotStepAttrs(name, st)
    b.WriteString(fmt.Sprintf("  %q [%s];\n", name, attrs))
}
```

Add a `dotStepAttrs(name string, st *workflow.StepNode) string` helper that returns the
Graphviz attribute string (e.g. `shape=box` or
`shape=component, label="run_tests\n[for_each]\n[→ review]"`).

Logic:
1. Start with `shape=box` (or `shape=component` for subworkflow steps).
2. Collect annotation lines: `"[for_each]"`, `"[count]"`, `"[parallel]"`, `"[→ <subwf>]"`.
3. If any annotations exist, emit `label="<name>\n<annotations>"` (newline-separated).
4. Join all attributes with `, `.

The `hcl.Expression` fields only need a nil check — the iteration mode is indicated by which
field is set, not by the expression value itself.

### Step 2 — Tests

Add to `internal/cli/compile_test.go` (or a new `internal/cli/compile_dot_test.go`):

1. **`TestRenderDOT_PlainStepNoAnnotation`** — plain adapter step; DOT output contains
   `[shape=box]` and does NOT contain `label=` for that node.

2. **`TestRenderDOT_ForEachStepAnnotation`** — step with `for_each`; DOT output contains
   `[for_each]` in the node label.

3. **`TestRenderDOT_CountStepAnnotation`** — step with `count`; DOT output contains
   `[count]` in the node label.

4. **`TestRenderDOT_ParallelStepAnnotation`** — step with `parallel`; DOT output contains
   `[parallel]` in the node label.

5. **`TestRenderDOT_SubworkflowStepAnnotation`** — subworkflow-targeted step; DOT output
   uses `shape=component` and contains `[→ <subwf_name>]` in the node label.

6. **`TestRenderDOT_IteratingSubworkflowStep`** — for_each targeting a subworkflow; DOT
   output contains both `[for_each]` and `[→ <subwf_name>]` in the label.

Tests can call `renderDOT` directly (it is package-internal) or use `compileWorkflowOutput`
with `format="dot"` end-to-end. The latter is preferred for coverage because it exercises
the full compile path.

For subworkflow tests, a `SubWorkflowResolver` backed by `t.TempDir()` is required (see the
`writeSubworkflowDir` pattern in `workflow/compile_subworkflows_test.go`). The CLI
`compileWorkflowOutput` uses `LocalSubWorkflowResolver`; tests may need to call
`buildDOTFromGraph` (extracted helper) directly with a pre-compiled graph to avoid filesystem
setup complexity — executor should choose whichever approach is cleaner.

## Behavior change

**Yes — DOT output shape changes for iterating and subworkflow steps.**

- Plain adapter steps: unchanged (`[shape=box]`).
- Iterating steps: gain a `label` attribute with a bracketed annotation suffix.
- Subworkflow steps: `shape` changes from `box` to `component`; gain a label.
- Consumers that parse the DOT node attribute string literally (e.g. tests asserting
  `[shape=box]` for a for_each step) will need updating — the test suite should cover this.
- The JSON output (`--format json`) is unaffected.
- No change to the wire contract, engine runtime, or `workflow/` package.

## Reuse

- `graph.StepOrder()` — already called in `renderDOT`; no change to iteration order.
- `workflow.StepNode` fields — nil checks only; no expression evaluation needed.
- Graphviz `shape=component` — standard built-in shape, no external dependencies.

### Step 3 — Render subworkflow bodies as `subgraph cluster_` blocks

A `shape=component` node annotated `[→ subwf_name]` tells the reader that a subworkflow is
invoked but gives no information about what it does. The DOT graph is only useful when it
shows the full execution structure; a subworkflow step that just says "something happens here"
is effectively a black box.

For every step where `SubworkflowRef != ""`, `renderDOT` must inline the referenced
subworkflow's graph as a Graphviz `subgraph cluster_<subwf_name>` block nested inside the
parent digraph. Node IDs inside the cluster must be namespaced (e.g.
`"<subwf_name>/<node_name>"`) to avoid collisions with the parent graph.

The step node in the parent graph should become the cluster entry edge target, i.e. the
parent edge that currently points to the step node should instead point to the
`<subwf_name>/__start__` node inside the cluster, and the cluster's terminal node(s) should
carry the original outbound edges.

If `FSMGraph` does not expose the referenced subworkflow's graph directly, the caller
(`compileWorkflowOutput` / `parseCompileForCli`) must pass a map of subworkflow graphs
alongside the primary graph so `renderDOT` can look them up by ref name.

Apply recursively: a subworkflow that itself contains subworkflow steps must also have its
referenced graphs inlined as nested clusters.

Cluster styling:

```dot
subgraph cluster_<subwf_name> {
    label="<subwf_name>";
    style=dashed;
    "<subwf_name>/__start__" [shape=point,width=0.12,label=""];
    "<subwf_name>/step_a"   [shape=box];
    // ... remaining nodes with same annotation rules as Step 1 ...
    "<subwf_name>/__start__" -> "<subwf_name>/step_a" [label="initial"];
    // ... remaining edges ...
}
```

The step node that previously carried `shape=component` is **replaced** by the cluster; the
original parent edges are rewired to the cluster's `__start__` node and the cluster's sink
nodes respectively.

### Step 4 — Tests for subgraph cluster rendering

Add to `internal/cli/compile_dot_test.go` (or a new sub-test section):

1. **`TestRenderDOT_SubworkflowCluster`** — workflow with one subworkflow step; DOT output
   contains a `subgraph cluster_<name>` block with the subworkflow's nodes namespaced.
2. **`TestRenderDOT_SubworkflowClusterEdges`** — parent graph edges are rewired to/from the
   cluster boundary (no dangling `shape=component` node remains in the output).
3. **`TestRenderDOT_NestedSubworkflowCluster`** — subworkflow that itself contains a
   subworkflow step; output contains nested `subgraph cluster_` blocks.

Update golden files for any existing fixtures that include subworkflow steps to match the
cluster output shape.

## Out of scope

- Showing timeout, adapter ref, or `on_crash` values in the DOT label.
- HTML-like (`<table>`) labels or custom Graphviz stylesheets.
- The JSON output path (`buildCompileJSON`).
- Any change to the `workflow/` package, wire contract, or engine.

## Files this workstream may modify

- `internal/cli/compile.go` — `renderDOT` loop + new `dotStepAttrs` helper + subgraph cluster rendering.
- `internal/cli/compile_test.go` (or new `internal/cli/compile_dot_test.go`) — unit tests.
- `internal/cli/testdata/compile/*.dot.golden` — golden files for fixtures with subworkflow steps.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `dotStepAttrs(name string, st *workflow.StepNode) string` helper in `internal/cli/compile.go`.
- [x] Replace unconditional `[shape=box]` step node loop in `renderDOT` with annotating loop.
- [x] Add 6 annotation tests.
- [x] `make build` clean (annotations).
- [x] `make test` clean (annotations).
- [x] Extend `renderDOT` (and its callers if needed) to inline referenced subworkflow graphs as `subgraph cluster_` blocks with namespaced node IDs.
- [x] Rewire parent edges to/from cluster boundary nodes; remove the `shape=component` placeholder node.
- [x] Apply cluster rendering recursively for nested subworkflows.
- [x] Add 3 subgraph cluster tests (`TestRenderDOT_SubworkflowCluster`, `_ClusterEdges`, `_NestedSubworkflowCluster`).
- [x] Update golden files for any fixtures with subworkflow steps.
- [x] `make build` clean.
- [x] `make test` clean.

## Exit criteria

- `criteria compile --format dot` on a workflow with a `for_each` step: that step's node
  contains `[for_each]` in its label.
- Same for `count` and `parallel` steps.
- A plain adapter step renders as `[shape=box]` with no `label` attribute.
- A subworkflow-targeted step is **not** rendered as a `shape=component` placeholder node;
  instead the parent digraph contains a `subgraph cluster_<subwf_name>` block with the
  subworkflow's full node/edge set, node IDs namespaced as `"<subwf_name>/<node>"`, and
  parent edges rewired to the cluster boundary.
- Nested subworkflow references produce nested `subgraph cluster_` blocks.
- `make test` clean.

## Implementation notes

**Files modified:**
- `internal/cli/compile.go` — replaced unconditional `[shape=box]` loop in `renderDOT` with
  `dotStepAttrs`-driven loop; added `dotStepAttrs` helper after `renderDOT`.
- `internal/cli/compile_dot_test.go` (new) — 6 required `TestRenderDOT_*` tests plus 2
  bonus `TestDotStepAttrs_*` unit tests for the helper directly.
- `internal/cli/testdata/compile/*.dot.golden` — updated 7 golden files whose fixtures
  contain iterating steps: `iteration_simple` (for_each + count), `demo_tour_local`
  (for_each), `phase3-parallel` (parallel × 1 visible step), `phase3-marquee` (parallel).
  Remaining golden files were unchanged (no iterating or subworkflow steps).

**Steps 1–2 key decisions:**
- `for_each`/`count`/`parallel` are mutually exclusive (enforced by the schema); the helper
  uses `if / else if / else if` rather than separate checks.
- `SubworkflowRef` is checked independently so iterating subworkflow steps receive both
  annotations.
- Golden files regenerated with `-update` flag; all pass without modification after
  regeneration.
- The `iteration_workflow_step` golden file is orphaned (its testdata directory does not
  exist); this is a pre-existing condition, out of scope for this workstream.

**Steps 3–4 files modified:**
- `internal/cli/compile.go` — replaced the single `renderDOT` monolith (~50 lines) with a
  ~200-line cluster-rendering refactor. New helpers: `dotWriteNodes`, `dotWriteClusterBody`,
  `dotWriteEdges`, `dotWriteExitEdges`, `dotResolveRef`, `sanitizeDotID`, `dotClusterLabel`.
  `dotStepAttrs` is unchanged; still used for adapter steps and the no-body fallback.
- `internal/cli/compile_dot_test.go` — added `writeTempSubworkflow` helper + 3 new end-to-end
  cluster tests; updated `TestRenderDOT_SubworkflowStepAnnotation` and
  `TestRenderDOT_IteratingSubworkflowStep` to expect cluster output instead of
  `shape=component`.
- No golden files needed updating — existing fixtures have no subworkflow-targeted steps.

**Steps 3–4 key decisions:**
- `dotWriteNodes` does a two-pass over `StepOrder()`: first emits adapter/switch/state nodes,
  then emits cluster blocks. This keeps all flat nodes before nested subgraphs.
- Node namespace is a string prefix `"<subwf_name>/"` accumulated through recursion, giving
  `"outer/leaf/node"` at three levels.
- Cluster ID is `sanitizeDotID(namespace + subwf_name)` (slashes → underscores), giving
  `cluster_outer_leaf` for nested `outer → leaf`.
- Exit edges: ALL terminal states in a cluster emit ALL parent step outcome edges. This is a
  visual approximation; it matches the spec's "terminal node(s) carry the original outbound
  edges".
- Fallback to `shape=component` node is preserved when `swNode == nil || swNode.Body == nil`.
- Existing annotation tests (`TestRenderDOT_SubworkflowStepAnnotation`,
  `TestRenderDOT_IteratingSubworkflowStep`) were updated in place to check cluster output;
  the cluster label still embeds `[→ subwf_name]` and `[for_each]` so annotation semantics
  are preserved at the cluster level.

**Validation (Steps 3–4):** `go test ./internal/cli/... -run 'TestRenderDOT_|TestDotStepAttrs_'`
— 11/11 pass. `make test` clean (all packages, -race).

## Reviewer Notes

### Review 2026-05-08 — approved

#### Summary
The implementation meets the workstream scope and exit criteria. `renderDOT` now annotates iterating steps, renders subworkflow-targeted steps as `shape=component`, preserves plain adapter steps without a label override, and the test coverage exercises both fixture-backed DOT output and dedicated end-to-end subworkflow cases.

#### Plan Adherence
- `dotStepAttrs(name string, st *workflow.StepNode) string` was added in `internal/cli/compile.go` and is used by `renderDOT` for step node emission.
- Iteration annotations are emitted for `for_each`, `count`, and `parallel`, and subworkflow steps add the `[→ <name>]` label line with `shape=component`.
- Plain adapter steps remain `[shape=box]` with no `label` attribute.
- Required tests are present in `internal/cli/compile_dot_test.go`, and DOT goldens covering existing iterating fixtures were updated consistently with the behavior change.

#### Test Intent Assessment
The new tests validate contract-visible DOT behavior rather than helper internals alone: plain-step output asserts the absence of a label override, iterating-step tests assert the expected annotation strings, and the subworkflow cases compile real parent/subworkflow modules through `compileWorkflowOutput` so the CLI-facing path is exercised end-to-end. The existing golden suite adds regression coverage for real fixture workflows using `for_each`, `count`, and `parallel`.

#### Validation Performed
- `git show --stat --summary --format=fuller 6b51dcf` and targeted diff inspection for `internal/cli/compile.go`, `internal/cli/compile_dot_test.go`, and the DOT goldens.
- `go test ./internal/cli -run 'TestRenderDOT_|TestDotStepAttrs_|TestCompileGolden_JSONAndDOT' -count=1`
- `make build`
- `make test`


## Reviewer Notes

### Review 2026-05-08 — approved

#### Summary
The implementation meets the workstream scope and exit criteria. `renderDOT` now annotates iterating steps, renders subworkflow-targeted steps as `shape=component`, preserves plain adapter steps without a label override, and the test coverage exercises both fixture-backed DOT output and dedicated end-to-end subworkflow cases.

#### Plan Adherence
- `dotStepAttrs(name string, st *workflow.StepNode) string` was added in `internal/cli/compile.go` and is used by `renderDOT` for step node emission.
- Iteration annotations are emitted for `for_each`, `count`, and `parallel`, and subworkflow steps add the `[→ <name>]` label line with `shape=component`.
- Plain adapter steps remain `[shape=box]` with no `label` attribute.
- Required tests are present in `internal/cli/compile_dot_test.go`, and DOT goldens covering existing iterating fixtures were updated consistently with the behavior change.

#### Test Intent Assessment
The new tests validate contract-visible DOT behavior rather than helper internals alone: plain-step output asserts the absence of a label override, iterating-step tests assert the expected annotation strings, and the subworkflow cases compile real parent/subworkflow modules through `compileWorkflowOutput` so the CLI-facing path is exercised end-to-end. The existing golden suite adds regression coverage for real fixture workflows using `for_each`, `count`, and `parallel`.

#### Validation Performed
- `git show --stat --summary --format=fuller 6b51dcf` and targeted diff inspection for `internal/cli/compile.go`, `internal/cli/compile_dot_test.go`, and the DOT goldens.
- `go test ./internal/cli -run 'TestRenderDOT_|TestDotStepAttrs_|TestCompileGolden_JSONAndDOT' -count=1`
- `make build`
- `make test`
