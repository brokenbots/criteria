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

- [ ] Add `dotAdapterPalette` slice and semantic fill color constants to `internal/cli/compile.go`.
- [ ] Add `buildAdapterColorMap(graph *workflow.FSMGraph) map[string]string` helper.
- [ ] Add `adapterTypeOf(ref string) string` helper (splits `"<type>.<name>"` on first `.`).
- [ ] Implement/extend `dotStepAttrs` to accept `adapterColors map[string]string` and emit shape, fillcolor, style, and peripheries.
- [ ] Call `buildAdapterColorMap` once at the top of `renderDOT`; pass result into step node loop.
- [ ] Update switch node loop to add `style=filled` and `fillcolor`.
- [ ] Update state node loop to add fill for terminal success/failure states.
- [ ] Add 12 tests (3 unit tests for `buildAdapterColorMap`, 9 render tests).
- [ ] `make build` clean.
- [ ] `make test` clean.

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
