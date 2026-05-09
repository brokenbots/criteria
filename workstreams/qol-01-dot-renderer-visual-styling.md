# QoL Workstream QoL-01 — DOT renderer: per-adapter fill colors, border styles by target kind, and distinct node shapes

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-05 (complementary; independent).

> **Note on BF-05 coordination.** BF-05 adds text annotations (`[for_each]`, `[→ subwf_name]`)
> and changes the subworkflow step shape to `shape=component`. This workstream adds fill colors,
> border styles, and refines the same shape taxonomy. The executor **must** read BF-05 before
> starting. If BF-05 is already merged, the `dotStepAttrs` helper introduced there is the right
> place to inject the new attributes. If BF-05 is not yet merged, implement shape/color/style
> in a parallel `dotStepAttrs` helper and ensure the two workstreams' changes compose cleanly
> when merged (same function, additive attributes).

## Context

The current DOT renderer ([internal/cli/compile.go:218](../internal/cli/compile.go#L218)) emits
every step node as an unstyled `[shape=box]`. All steps look identical regardless of which
adapter they use, whether they iterate, or whether they delegate to a subworkflow. Switches
already use `shape=diamond`, but are otherwise unstyled.

A workflow with a mix of shell steps, copilot steps, for_each fan-outs, and subworkflow
delegations produces a monochrome graph that requires reading every label to understand
structure. Adding fill color, border style, and distinct shapes makes the graph immediately
interpretable.

### Proposed visual vocabulary

#### Node shapes by target kind

| Node type | Shape | Notes |
|---|---|---|
| Plain adapter step | `box` | Unchanged |
| Subworkflow step | `component` | Graphviz built-in; conveys "external module" |
| Iterating step (`for_each` / `count`) | `box` + dashed border | Shape unchanged; border signals "fan-out" |
| Parallel step | `box` + double border (`peripheries=2`) | Conveys concurrent fan-out |
| Switch | `diamond` | Unchanged from current |
| Non-terminal state | `ellipse` | Unchanged |
| Terminal success state | `doublecircle` + green fill | Currently unstyled doublecircle |
| Terminal failure state | `doublecircle` + red fill | Currently unstyled doublecircle |

A step that is both iterating and subworkflow-targeted inherits `shape=component` with the
dashed or double-border style.

#### Fill colors by adapter type (requires `style=filled`)

Adapter type is read from `graph.Adapters[st.AdapterRef].Type` (the `<type>` segment of the
`"<type>.<name>"` reference). For subworkflow steps `AdapterRef` is empty; use the subworkflow
color instead.

Colors are **assigned dynamically at render time** from a fixed palette, not hard-coded per
adapter name. `renderDOT` walks `graph.AdapterOrder` once before emitting any nodes and builds
a `map[string]string` (adapter type → color) by assigning palette entries in order. Any adapter
type present in the compiled graph gets a unique color; adapter types not seen get none.
This means a new adapter (`llm`, `webhook`, etc.) added later automatically receives a color
without any code change.

The palette is a fixed ordered slice of low-saturation pastels chosen for legibility in both
light and dark Graphviz viewers and when printed. Eight entries are sufficient; if a workflow
declares more distinct adapter types than palette entries, colors wrap around (modulo):

```go
var dotAdapterPalette = []string{
    "#D6EAF8", // light blue
    "#E8DAEF", // light purple
    "#FDEBD0", // light orange
    "#EAECEE", // light gray
    "#D5F5E3", // light green (note: also used for subworkflow)
    "#FDFEFE", // near-white
    "#FEF9E7", // light yellow (note: also used for switches)
    "#FDEDEC", // light rose
}
```

Assignment helper (called once per `renderDOT` invocation):

```go
func buildAdapterColorMap(graph *workflow.FSMGraph) map[string]string {
    colors := make(map[string]string, len(graph.AdapterOrder))
    i := 0
    for _, ref := range graph.AdapterOrder {
        ad := graph.Adapters[ref]
        if _, seen := colors[ad.Type]; !seen {
            colors[ad.Type] = dotAdapterPalette[i%len(dotAdapterPalette)]
            i++
        }
    }
    return colors
}
```

Fixed semantic colors (not drawn from the palette — always the same regardless of adapter count):

| Use | Color |
|---|---|
| Subworkflow step | `#D5F5E3` (light green) |
| Adapter type not in map (should not occur) | `#FFFFFF` (white fallback) |
| Switch nodes | `#FEF9E7` (light yellow) |
| Terminal success state | `#D5F5E3` (light green) |
| Terminal failure state | `#FADBD8` (light pink) |

Non-terminal states: no fill.
|---|---|
| None | `filled` |
| `for_each` or `count` | `filled,dashed` |
| `parallel` | `filled` + `peripheries=2` |

For subworkflow steps (shape=component), the same border rules apply.

## Prerequisites

- Read BF-05 ([workstreams/bugfix-05-dot-renderer-step-annotations.md](bugfix-05-dot-renderer-step-annotations.md))
  before starting. If BF-05 is merged, extend its `dotStepAttrs` helper. If not, implement
  independently and coordinate merge.
- Familiarity with:
  - [internal/cli/compile.go:218](../internal/cli/compile.go#L218) — `renderDOT`.
  - [workflow/schema.go:451](../workflow/schema.go#L451) — `StepNode`: `AdapterRef`,
    `SubworkflowRef`, `TargetKind`, `ForEach`, `Count`, `Parallel`.
  - [workflow/schema.go:371](../workflow/schema.go#L371) — `FSMGraph.Adapters map[string]*AdapterNode`;
    `AdapterNode.Type` for the color lookup.
  - Graphviz DOT attribute syntax: `fillcolor`, `style`, `peripheries`.
- `make build` green on `main`.

## In scope

### Step 1 — Palette and semantic color constants

Add to `internal/cli/compile.go`:

```go
// dotAdapterPalette is an ordered set of low-saturation pastel fill colors assigned
// to adapter types in declaration order at render time. Colors wrap if more distinct
// adapter types exist than palette entries.
var dotAdapterPalette = []string{
    "#D6EAF8", // light blue
    "#E8DAEF", // light purple
    "#FDEBD0", // light orange
    "#EAECEE", // light gray
    "#D5F5E3", // light green
    "#FDFEFE", // near-white
    "#FEF9E7", // light yellow
    "#FDEDEC", // light rose
}

const (
    dotSubworkflowFill = "#D5F5E3"
    dotUnknownFill     = "#FFFFFF"
    dotSwitchFill      = "#FEF9E7"
    dotSuccessFill     = "#D5F5E3"
    dotFailureFill     = "#FADBD8"
)
```

Add `buildAdapterColorMap`:

```go
// buildAdapterColorMap assigns a palette color to each distinct adapter type
// present in graph.AdapterOrder. New adapter types receive colors automatically;
// no per-type hard-coding is required.
func buildAdapterColorMap(graph *workflow.FSMGraph) map[string]string {
    colors := make(map[string]string, len(graph.AdapterOrder))
    i := 0
    for _, ref := range graph.AdapterOrder {
        ad := graph.Adapters[ref]
        if _, seen := colors[ad.Type]; !seen {
            colors[ad.Type] = dotAdapterPalette[i%len(dotAdapterPalette)]
            i++
        }
    }
    return colors
}
```

### Step 2 — Step node attribute builder

Extend `dotStepAttrs` (from BF-05) or introduce it here. The function signature is:

```go
func dotStepAttrs(name string, st *workflow.StepNode, adapterColors map[string]string) string
```

`adapterColors` is the map returned by `buildAdapterColorMap`, built once at the top of
`renderDOT` before the node loops. Logic:

1. **Shape**: `component` if `st.SubworkflowRef != ""`, else `box`.
2. **Fill color**:
   - If `st.SubworkflowRef != ""` → `dotSubworkflowFill`.
   - Otherwise look up the adapter type via `adapterColors[adapterTypeOf(st.AdapterRef)]`;
     fall back to `dotUnknownFill` if the type is absent (should not occur for a valid graph).
3. **Style + peripheries**:
   - `parallel` non-nil → `style="filled"`, `peripheries=2`
   - `for_each` or `count` non-nil → `style="filled,dashed"`
   - otherwise → `style="filled"`
4. Build the `[shape=..., style=..., fillcolor="...", peripheries=N]` attribute string.
   Omit `peripheries` when it is 1 (default).

`adapterTypeOf` extracts the `<type>` prefix from a `"<type>.<name>"` ref string (split on
first `.`). This is a two-line helper; do not reach into `graph.Adapters` inside `dotStepAttrs`
to keep the function unit-testable without a full graph.

Update `renderDOT` to build the color map once and pass it down:

```go
func renderDOT(graph *workflow.FSMGraph) string {
    adapterColors := buildAdapterColorMap(graph)
    // ...
    for _, name := range graph.StepOrder() {
        st := graph.Steps[name]
        b.WriteString(fmt.Sprintf("  %q [%s];\n", name, dotStepAttrs(name, st, adapterColors)))
    }
    // ...
}
```

### Step 3 — Switch node coloring

Replace the current unconditional `shape=diamond` emission for switches with one that also
sets fill:

```go
for _, name := range sortedSwitchNames(graph) {
    b.WriteString(fmt.Sprintf("  %q [shape=diamond, style=filled, fillcolor=%q];\n", name, dotSwitchFill))
}
```

### Step 4 — Terminal state coloring

Replace the current state node loop with one that adds fill for terminal nodes:

```go
for _, name := range sortedStateNames(graph) {
    st := graph.States[name]
    shape := "ellipse"
    if st.Terminal {
        shape = "doublecircle"
    }
    fill := ""
    if st.Terminal && st.Success {
        fill = fmt.Sprintf(", style=filled, fillcolor=%q", dotSuccessFill)
    } else if st.Terminal && !st.Success {
        fill = fmt.Sprintf(", style=filled, fillcolor=%q", dotFailureFill)
    }
    b.WriteString(fmt.Sprintf("  %q [shape=%s%s];\n", name, shape, fill))
}
```

### Step 5 — Tests

Add to `internal/cli/compile_test.go` (or a new `internal/cli/compile_dot_styling_test.go`):

1. **`TestBuildAdapterColorMap_AssignsPaletteInOrder`** — graph with two distinct adapter types
   (e.g. `shell` and `noop`); assert each gets a different non-empty hex color and the colors
   match `dotAdapterPalette[0]` and `dotAdapterPalette[1]` respectively.

2. **`TestBuildAdapterColorMap_WrapsAtPaletteEnd`** — graph with more distinct adapter types
   than palette entries (construct `graph.AdapterOrder` + `graph.Adapters` manually); assert
   color at index `len(dotAdapterPalette)` equals `dotAdapterPalette[0]` (wraps).

3. **`TestBuildAdapterColorMap_SameTypeMultipleInstances`** — two adapters of the same type
   (e.g. `shell.default` and `shell.alt`); assert they share the same color and only one
   palette slot is consumed.

4. **`TestDOT_StepHasFillColor`** — compile a single-adapter workflow; assert the step node
   line contains `style=filled` and a `fillcolor=` attribute. Do **not** assert a specific hex
   value — assert only that the value is a non-empty string matching `#[0-9A-Fa-f]{6}`.

5. **`TestDOT_TwoAdapterTypesDifferentColors`** — compile a workflow with two steps targeting
   two different adapter types; assert the two step node lines have different `fillcolor` values.

6. **`TestDOT_SubworkflowStepColor`** — subworkflow-targeted step; assert `fillcolor="#D5F5E3"`
   (fixed semantic color, not from palette) and `shape=component`.

7. **`TestDOT_ForEachStepDashedBorder`** — for_each step; `style=filled,dashed`.

8. **`TestDOT_ParallelStepDoublePeripheries`** — parallel step; `peripheries=2`.

9. **`TestDOT_SwitchFillColor`** — switch node; `fillcolor="#FEF9E7"` (fixed semantic color).

10. **`TestDOT_TerminalSuccessStateFill`** — terminal success state; `fillcolor="#D5F5E3"`.

11. **`TestDOT_TerminalFailureStateFill`** — terminal failure state; `fillcolor="#FADBD8"`.

12. **`TestDOT_NonTerminalStateNoFill`** — non-terminal state; no `fillcolor` attribute.

Test 1–3 call `buildAdapterColorMap` directly with hand-built `*workflow.FSMGraph` values
(no HCL compilation needed). Tests 4–12 use `renderDOT` directly or `compileWorkflowOutput`
with `format="dot"`. For subworkflow and for_each tests, compile from HCL fixtures with
`t.TempDir()` (see `compile_subworkflows_test.go` for the pattern).

## Behavior change

**Yes — DOT output attribute changes.**

- All step nodes gain `style=filled` and `fillcolor=...`.
- Iterating steps gain `style=filled,dashed` or `peripheries=2` as appropriate.
- Subworkflow steps gain `shape=component` and a green fill.
- Switch nodes gain `style=filled` and `fillcolor="#FEF9E7"`.
- Terminal states gain `style=filled` and a green or red fill.
- The graph remains structurally identical (no edges or labels change); only visual attributes
  are added.
- Consumers that assert exact DOT strings (e.g. `[shape=box]` without fill) will need
  updating — tests should cover this regression.
- No change to `--format json`, the wire contract, engine runtime, or the `workflow/` package.

## Reuse

- `sortedSwitchNames`, `sortedStateNames` — already called in `renderDOT`; no change.
- `graph.Adapters[st.AdapterRef]` — already used in `buildCompileJSON`; same access pattern.
- BF-05's `dotStepAttrs` helper — extend rather than replace if BF-05 is already merged.

## Out of scope

- Wait and approval nodes — currently not rendered in DOT at all; visual styling is moot
  until they are included (separate workstream).
- Custom color schemes or user-configurable palettes.
- HTML-like (`<table>`) labels or embedded icons.
- Any change to `--format json`, the wire contract, or the `workflow/` package.

## Files this workstream may modify

- `internal/cli/compile.go` — `renderDOT`, `dotStepAttrs` (new or extended), color constants/map.
- `internal/cli/compile_test.go` (or new `internal/cli/compile_dot_styling_test.go`) — 10 new tests.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `dotAdapterPalette` slice and semantic fill color constants to `internal/cli/compile.go`.
- [x] Add `buildAdapterColorMap(graph *workflow.FSMGraph) map[string]string` helper.
- [x] Add `adapterTypeOf(ref string) string` helper (splits `"<type>.<name>"` on first `.`).
- [x] Implement/extend `dotStepAttrs` to accept `adapterColors map[string]string` and emit shape, fillcolor, style, and peripheries.
- [x] Call `buildAdapterColorMap` once at the top of `renderDOT`; pass result into step node loop.
- [x] Update switch node loop to add `style=filled` and `fillcolor`.
- [x] Update state node loop to add fill for terminal success/failure states.
- [x] Add 12 tests (3 unit tests for `buildAdapterColorMap`, 9 render tests).
- [x] `make build` clean.
- [x] `make test` clean.

## Implementation notes

### Changes made

**`internal/cli/compile.go`**
- Added `dotAdapterPalette` (8-entry pastel slice) and semantic color constants (`dotSubworkflowFill`, `dotUnknownFill`, `dotSwitchFill`, `dotSuccessFill`, `dotFailureFill`).
- Added `buildAdapterColorMap(graph *workflow.FSMGraph) map[string]string` — iterates `graph.AdapterOrder`, assigns palette entries to distinct adapter types with wrap-around.
- Added `adapterTypeOf(ref string) string` — two-line helper that splits `"<type>.<name>"` on the first `.`.
- Extended `dotStepAttrs` signature from `(name, st)` to `(name, st, adapterColors)`. Now emits `shape=`, `style=`, `fillcolor=`, optionally `peripheries=2`, and optionally `label=`.
- Updated `renderDOT` to call `buildAdapterColorMap` once and pass `adapterColors` through `dotWriteNodes` → `dotWriteNodeDecls` and `dotWriteClusterBody`.
- Updated `dotWriteNodes`, `dotWriteNodeDecls`, `dotWriteClusterBody` to accept and thread `adapterColors`.
- Updated switch node loop: `[shape=diamond, style=filled, fillcolor="#FEF9E7"]`.
- Updated state node loop: terminal-success gets green fill, terminal-failure gets pink fill, non-terminal gets no fill.

**`internal/cli/compile_dot_test.go`** (updated for behavioral changes)
- `TestRenderDOT_PlainStepNoAnnotation` — updated to check `style="filled"` and `fillcolor=`; node-level no-label check tightened to match only the node declaration line (not edge lines).
- `TestDotStepAttrs_PlainAdapter` — updated to pass `adapterColors`; asserts fill color and style.
- `TestDotStepAttrs_SubworkflowOnly` — updated to verify `dotSubworkflowFill` fill color.

**`internal/cli/compile_dot_styling_test.go`** (new, 12 tests)
- `TestBuildAdapterColorMap_AssignsPaletteInOrder` — unit test, direct `buildAdapterColorMap` call.
- `TestBuildAdapterColorMap_WrapsAtPaletteEnd` — unit test, wrap-around verified.
- `TestBuildAdapterColorMap_SameTypeMultipleInstances` — unit test, shared type → single slot.
- `TestDOT_StepHasFillColor` — compile HCL; assert hex fillcolor on step node line.
- `TestDOT_TwoAdapterTypesDifferentColors` — compile HCL with noop + shell; different fill colors.
- `TestDOT_SubworkflowStepColor` — `dotStepAttrs` direct call; `shape=component`, `#D5F5E3`.
- `TestDOT_ForEachStepDashedBorder` — compile HCL; `style="filled,dashed"`.
- `TestDOT_ParallelStepDoublePeripheries` — compile HCL; `peripheries=2`.
- `TestDOT_SwitchFillColor` — compile HCL; `fillcolor="#FEF9E7"`.
- `TestDOT_TerminalSuccessStateFill` — compile HCL; `fillcolor="#D5F5E3"`.
- `TestDOT_TerminalFailureStateFill` — compile HCL; `fillcolor="#FADBD8"`.
- `TestDOT_NonTerminalStateNoFill` — compile HCL; no `fillcolor` on non-terminal state.

**Golden files regenerated** (all 30+ `.dot.golden` files in `internal/cli/testdata/compile/` now contain the new styled attributes).

### Design decision: adapterColors threading to subworkflow clusters (updated)

The design decision in the previous iteration was incorrect: `adapterColors` built from the root graph only caused subworkflow-local adapter types to fall back to white. The fix (`collectAdapterTypes` + depth-first traversal) builds the map from the entire reachable graph tree so every adapter type gets a palette color. The root-first traversal also ensures root adapter types retain lower palette indices.

### Security review

No user-controlled input reaches DOT attribute values. Step names and adapter types come from the compiler. Colors are fixed literals. No new dependencies introduced.

## Exit criteria

- `criteria compile --format dot` on a workflow with two different adapter types produces step
  nodes with distinct, non-empty `fillcolor` values drawn from `dotAdapterPalette`.
- Adding a new adapter type to a workflow (without any code change) produces a new color
  automatically — verified by the wrap and multi-type unit tests.
- Subworkflow steps always use the fixed `#D5F5E3` semantic color regardless of palette
  assignment order.
- for_each/count steps have dashed borders; parallel steps have double borders.
- Terminal success states are green-filled; terminal failure states are pink-filled.
- Plain adapter steps render with `style=filled` and a palette-assigned color.
- `make test` clean.

## Reviewer Notes

### Review 2026-05-08 — changes-requested

#### Summary

The root-step, switch, terminal-state, and palette helper portions are implemented and the repository build/tests are green, but the actual compiled subworkflow render path still misses the workstream's visual semantics. Inlined subworkflow bodies can render valid adapter steps with the white unknown fallback, and compiled subworkflow clusters are still emitted with a hard-coded dashed border and no semantic subworkflow color, so the user-visible DOT output does not yet satisfy the acceptance bar.

#### Plan Adherence

- Steps 1, 3, and 4 are implemented as described for root graph adapter steps, switches, and terminal states.
- Step 2 is only partially implemented. `dotStepAttrs` handles the fallback placeholder path, but compiled subworkflow bodies render through the cluster path in `renderDOT`, and that path does not apply the required subworkflow/fan-out styling semantics.
- Step 5 is incomplete at the contract boundary that matters here: the new tests cover palette mapping, plain steps, switches, and terminal states, but they do not prove the styling of compiled subworkflow output produced by `renderDOT`.

#### Required Remediations

- **blocker** — `internal/cli/compile.go:303-308,338,396,545-552`: valid adapter steps inside compiled subworkflow bodies can fall back to `dotUnknownFill` (`#FFFFFF`) because the color map is built from the root graph only and then reused for nested bodies. Reproduction: a root workflow delegating to a subworkflow that contains a `shell` step renders `"delegate/shell_step" [shape=box, style="filled", fillcolor="#FFFFFF"]`. This violates the workstream's dynamic adapter-color assignment and the exit criterion that adding a new adapter type to a workflow automatically receives a color. **Acceptance criteria:** ensure every real adapter type reachable in the rendered workflow, including subworkflow-local adapter types, gets a palette color instead of the unknown fallback; add a regression test that compiles a workflow with a subworkflow-only adapter type and asserts a non-white palette color on the nested step node.
- **blocker** — `internal/cli/compile.go:335-338,393-396`: every compiled subworkflow cluster is still emitted with `style=dashed` and no semantic subworkflow color, so plain delegated subworkflows render as iterating/fan-out nodes and compiled subworkflow output never shows the required fixed subworkflow styling. This misses the workstream's stated visual vocabulary (`subworkflow` semantic styling, dashed only for `for_each`/`count`, double border for `parallel`). **Acceptance criteria:** apply the workstream's target-kind and fan-out styling rules to the actual compiled subworkflow render path, not just the placeholder path, and add render tests that assert the compiled subworkflow output for plain, iterating, and parallel delegation cases.

#### Test Intent Assessment

The direct `buildAdapterColorMap` tests are strong for palette order, wrapping, and repeated adapter types, and the plain-step/switch/terminal-state render tests assert user-visible DOT attributes rather than implementation details. The weak spot is compiled subworkflow rendering: `internal/cli/compile_dot_styling_test.go` only checks subworkflow styling via the fallback `dotStepAttrs` path, while the real `renderDOT` contract for compiled subworkflows still routes through cluster rendering. As written, the suite would stay green while compiled subworkflow nodes render white nested adapter steps or the wrong border semantics. Add contract-level assertions against compiled DOT output for those cases.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- Manual reproduction with `./bin/criteria compile --format dot <temp workflow>` using a root workflow that delegates to a subworkflow containing a `shell` adapter step — reproduced nested step output with `fillcolor="#FFFFFF"` and a plain delegated cluster rendered with unconditional `style=dashed`.

### Remediation 2 (this session) — blockers addressed

#### Changes made

**`internal/cli/compile.go`**
- Replaced `buildAdapterColorMap` with a two-pass approach: `buildAdapterColorMap` now calls `collectAdapterTypes`, a new depth-first recursive helper that walks `graph.AdapterOrder` and then recurses into each subworkflow body via `graph.SubworkflowOrder`. This ensures every adapter type reachable in the compiled tree gets a palette color; root types retain lower palette indices; shared types across parent/child consume one slot.
- Added `dotWriteClusterStyle` — emits the Graphviz style attributes for a compiled subworkflow cluster based on the delegation step's fan-out kind: `peripheries=2` for parallel, `style="filled,dashed"` for for_each/count, `style=filled` for plain. All cluster kinds receive `fillcolor="#D5F5E3"` (the semantic subworkflow fill) as a visual indicator.
- Replaced both hardcoded `style=dashed` calls in `dotWriteNodes` and `dotWriteClusterBody` with calls to `dotWriteClusterStyle`.
- Removed the now-incorrect design decision note that rationalized the white fallback as acceptable.

**`internal/cli/compile_dot_styling_test.go`** (4 new tests, total now 16)
- `TestBuildAdapterColorMap_SubworkflowLocalType` — compiles a parent+subworkflow workflow where the subworkflow uses a `shell` adapter not declared in the parent; asserts the nested `delegate/do_shell` step has a non-white palette color.
- `TestDOT_PlainSubworkflowClusterStyle` — compiles a plain delegation; asserts `fillcolor="#D5F5E3"`, no `style=filled,dashed`, no `peripheries=2`.
- `TestDOT_IteratingSubworkflowClusterStyle` — compiles a for_each delegation; asserts `style="filled,dashed"` and `fillcolor="#D5F5E3"` in cluster header.
- `TestDOT_ParallelSubworkflowClusterStyle` — compiles a parallel delegation; asserts `peripheries=2` and `fillcolor="#D5F5E3"`, no `style=filled,dashed`.

#### Validation

- `make test` — all 16 styling tests + full suite passes.
- Golden files: no regeneration needed (no example workflows use compiled subworkflow clusters).
- Security: no change to threat surface. All cluster attributes are fixed constants or step metadata from the compiler.

### Review 2026-05-08-02 — changes-requested

#### Summary

The two prior implementation blockers are fixed: compiled subworkflow-local adapter types now receive palette colors, and compiled subworkflow clusters render with the intended plain/iterating/parallel border semantics. However, the new regression tests still do not fully prove the cluster-level styling contract, so this pass remains blocked on test intent rather than implementation behavior.

#### Plan Adherence

- Step 2 is now implemented on the actual compiled-subworkflow render path: manual DOT output shows semantic subworkflow fill, solid border for plain delegation, dashed border for iterating delegation, and double border for parallel delegation.
- Step 5 improved materially with new compiled-subworkflow coverage, but the cluster-style assertions are still too broad to guarantee the intended cluster attributes themselves.

#### Required Remediations

- **blocker** — `internal/cli/compile_dot_styling_test.go:453-570,573-635`: the new compiled-subworkflow cluster tests search the full DOT output for `fillcolor="#D5F5E3"`, `style="filled,dashed"`, and `peripheries=2`, but they do not isolate the cluster header lines they are supposed to verify. A faulty implementation that drops the cluster `fillcolor` or `style=filled` while leaving nested terminal states green-filled could still pass these tests. Under the test-intent rubric, this is not regression-sensitive enough for the cluster-rendering contract. **Acceptance criteria:** tighten the plain/iterating/parallel compiled-subworkflow tests so they assert the attributes on the cluster declaration block itself (for example by extracting the `subgraph cluster_<name> { ... }` header lines or matching line-by-line within that block), including an explicit assertion for plain-cluster `style=filled`.

#### Test Intent Assessment

`buildAdapterColorMap` coverage is now strong, and the manual compiled DOT output demonstrates the implementation behavior is correct. The remaining weakness is precision: the cluster-style tests currently prove that the rendered graph contains those attribute strings somewhere, not that the cluster contract carries them. That means at least one plausible regression would still pass.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- Manual `./bin/criteria compile --format dot <temp workflow>` reproduction confirmed:
  - nested subworkflow adapter step rendered with a palette color instead of `#FFFFFF`
  - plain compiled subworkflow cluster rendered with `fillcolor="#D5F5E3"` and `style=filled`
  - parallel compiled subworkflow cluster rendered with `peripheries=2`

### Remediation 3 (this session) — test precision

#### Changes made

**`internal/cli/compile_dot_styling_test.go`**
- Added `clusterAttrLines(dot, stepName string) ([]string, bool)` helper: uses brace-depth tracking to locate the named `subgraph cluster_<id>` block, then extracts only the cluster-level attribute lines (skipping node declarations that start with `"`, edges containing `->`, nested subgraph openers, and blank/closing-brace-only lines). This scopes test assertions to the cluster contract and not the full graph.
- Updated `TestDOT_PlainSubworkflowClusterStyle`: now calls `clusterAttrLines(dot, "delegate")` and asserts `fillcolor`, `style=filled`, absence of `style="filled,dashed"` and `peripheries=2` all against the extracted cluster attrs — a faulty implementation that omits cluster `fillcolor` or `style=filled` while leaving terminal-state styling intact will now fail.
- Updated `TestDOT_IteratingSubworkflowClusterStyle`: now calls `clusterAttrLines(dot, "process_all")` and asserts `style="filled,dashed"` and `fillcolor` within the cluster header.
- Updated `TestDOT_ParallelSubworkflowClusterStyle`: now calls `clusterAttrLines(dot, "run_tasks")` and asserts `peripheries=2`, `style=filled`, and `fillcolor` within the cluster header; also explicitly checks absence of `style="filled,dashed"`.

#### Validation

- `make test` — all 16 tests pass.
- `make lint-go` — clean.

### Review 2026-05-08-03 — changes-requested

#### Summary

The cluster-style assertions are now scoped much more tightly, and the implementation plus repository validation are clean. The remaining blocker is in the new `clusterAttrLines` helper itself: despite the comment and intended contract, it still captures nested cluster attribute lines, so the cluster-style tests are not yet reliably isolated to the cluster under test.

#### Plan Adherence

- The implementation path remains correct for the workstream's styling semantics.
- Step 5 is still not fully closed because the new helper intended to enforce cluster-level precision does not actually restrict results to depth-1 cluster attributes.

#### Required Remediations

- **blocker** — `internal/cli/compile_dot_styling_test.go:453-499`: `clusterAttrLines` claims to return only top-level attribute lines from the named cluster, but the implementation never checks `depth == 1` before appending lines. It skips the nested `subgraph ... {` opener, yet still collects nested cluster attributes like `label=`, `fillcolor=`, and `style=`. A quick probe with a parent cluster containing a nested cluster returned both the parent attrs and the nested child's attrs, which reintroduces false-positive risk for exactly the contract these tests were added to protect. **Acceptance criteria:** update `clusterAttrLines` so it only records lines belonging to the named cluster's top level (excluding nested cluster contents), and add a focused regression test proving nested cluster attributes are excluded from the extracted attribute set.

#### Test Intent Assessment

This is very close now: the plain/iterating/parallel tests no longer scan the whole DOT blob. But because the extractor still leaks nested cluster attrs, the assertions are not yet fully regression-sensitive for recursive subworkflow rendering, which this renderer already supports.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- `make lint-go` — passed.
- Manual probe of the new `clusterAttrLines` logic with a parent cluster containing a nested child cluster showed the helper returning both parent and child attribute lines, confirming the isolation bug.

### Remediation 4 (this session) — clusterAttrLines depth guard

#### Changes made

**`internal/cli/compile_dot_styling_test.go`**
- Fixed `clusterAttrLines`: added `if depth != 1 { continue }` guard after the `depth == 0` break check. Lines at depth > 1 (inside nested sub-clusters) are now skipped entirely, so nested cluster attributes (fillcolor, style, peripheries) are never included in the result set.
- Added `TestClusterAttrLines_ExcludesNestedCluster`: synthesises a DOT string with a parent `cluster_outer` (fillcolor `#AAAAAA`, `style=filled`) containing a nested `cluster_inner` (fillcolor `#BBBBBB`, `style="filled,dashed"`, `peripheries=2`); asserts that only the parent attrs appear in the extracted set and none of the nested attrs are present.

#### Validation

- `make test` — all 17 tests pass.
- `make lint-go` — clean.
