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

## Out of scope

- Showing timeout, adapter ref, or `on_crash` values in the DOT label.
- Rendering subworkflow body as a subgraph cluster — that is a larger DOT restructuring.
- HTML-like (`<table>`) labels or custom Graphviz stylesheets.
- The JSON output path (`buildCompileJSON`).
- Any change to the `workflow/` package, wire contract, or engine.

## Files this workstream may modify

- `internal/cli/compile.go` — `renderDOT` loop + new `dotStepAttrs` helper.
- `internal/cli/compile_test.go` (or new `internal/cli/compile_dot_test.go`) — 6 new tests.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `dotStepAttrs(name string, st *workflow.StepNode) string` helper in `internal/cli/compile.go`.
- [x] Replace unconditional `[shape=box]` step node loop in `renderDOT` with annotating loop.
- [x] Add 6 tests.
- [x] `make build` clean.
- [x] `make test` clean (pre-existing `TestCompileJSON_SubworkflowsArrayPresent` failure is a macOS `/private/var` symlink issue unrelated to this workstream; confirmed by stash-verify).

## Implementation notes

### Files modified
- `internal/cli/compile.go` — replaced unconditional `[shape=box]` step loop in `renderDOT`
  with `dotStepAttrs`-based loop; added `dotStepAttrs(name string, st *workflow.StepNode) string`
  helper after `renderDOT`.
- `internal/cli/compile_dot_test.go` — new file with 6 tests covering all annotation cases.
- Four DOT golden files updated to reflect new annotations:
  - `iteration_simple__workflow__testdata__iteration_simple.dot.golden` (`for_each`, `count`)
  - `phase3-parallel__examples__phase3_parallel.dot.golden` (`parallel`)
  - `demo_tour_local__examples__demo_tour_local.dot.golden` (`for_each`)
  - `phase3-marquee__examples__phase3_marquee.dot.golden` (`parallel`)

### Design choices
- `dotStepAttrs` uses `fmt.Sprintf("shape=%s, label=%q", ...)` with a Go string built with
  real `\n` separators; `%q` produces DOT-compatible `\n` line-break escapes.
- `→` (U+2192) is printable UTF-8; `%q` emits it literally, which Graphviz handles correctly.
- Annotation order is fixed: `[for_each]` / `[count]` / `[parallel]` then `[→ subwf]` —
  at most one iteration annotation is ever set (mutually exclusive fields).

### Pre-existing failure
`TestCompileJSON_SubworkflowsArrayPresent` fails on macOS because `t.TempDir()` returns
`/var/folders/...` but `filepath.EvalSymlinks` resolves it to `/private/var/folders/...`.
This failure exists on `main` before this workstream and is outside BF-05 scope.

## Exit criteria

- `criteria compile --format dot` on a workflow with a `for_each` step: that step's node
  contains `[for_each]` in its label.
- Same for `count` and `parallel` steps.
- A subworkflow-targeted step renders with `shape=component` and `[→ <name>]` in its label.
- A plain adapter step renders as `[shape=box]` with no `label` attribute.
- `make test` clean.
