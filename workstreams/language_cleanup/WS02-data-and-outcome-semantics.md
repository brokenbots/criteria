# WS02 — `data` block and outcome semantics

**Phase:** Language Cleanup · **Track:** Language · **Owner:** Workstream executor · **Depends on:** [WS01](WS01-mechanical-schema-cleanup.md) (cleaned schema, type expressions, `outcome "default"` form). · **Unblocks:** future remote-data sources (`data "http"`, `data "remote_state"`, etc.). · **Base branch:** `main`

## Context

Two semantic warts in the workflow HCL share enough underlying code (outcome compilation, runtime store, eval context shape) that they're best landed together:

1. **Outcome `next` is a magic string.** `next = "step_name"`, `next = "return"`, `next = "_continue"`. Switch conditions already accept the traversal form (`next = step.foo`) via [`resolveNextAttr`](../../workflow/compile_switches.go#L211-L269). Outcomes should match — plus the `"return"` and `"_continue"` sentinels become bare keywords (`return`, `continue`). The big win is renaming: if a new target type (e.g. an `inline_workflow` step) appears later, callers don't have to worry about naming conflicts because the kind is part of the reference.

2. **`shared_variable` is not Terraform-shaped.** Terraform has `resource` (read-write) and `data` (read-only-from-the-perspective-of-Terraform). There's no clean Terraform parallel to a workflow-scoped mutable variable, but `data` is the closer of the two and people already use that terminology to describe it. Renaming `shared_variable` to `data "internal" "name"` (with `data.internal.name.value` reads) opens the door to future remote `data` sources (`data "http"`, `data "remote_state"`, etc.) without re-shaping the surface. Internal-kind values are mutable; remote-kind values would be read-only — same as Terraform's `data` semantics.

Bundling these two changes is correct because:
- Both touch [workflow/compile_steps_graph.go](../../workflow/compile_steps_graph.go) (`compileOutcomeBlock`, `compileOutcomeRemain`).
- Both touch the engine runtime: outcome sentinel handling in iteration logic, and the shared-variable store rename to a data store.
- Both ripple through the same examples and `.criteria/workflows/` files.

Migration strategy is **hard break with helpful errors**, same as WS01.

## Prerequisites

- WS01 merged. This WS edits `OutcomeSpec`, which lives next to many of the structs WS01 touches.
- `cty.Object` nested-namespace eval contexts (already used by `each.*` and `step.*` references).

## In scope

### Step 1 — Outcome `next` traversals

**Today:** `next = "step_name"`, `next = "return"`, `next = "_continue"`
**Target:** `next = step.step_name`, `next = state.done`, `next = return`, `next = continue`

- [workflow/schema.go](../../workflow/schema.go) `OutcomeSpec.Next string hcl:"next"` (line 292) → `Next hcl.Expression hcl:"next"`. The expression is resolved by the compiler, not stored as a string.
- Extend [`resolveNextAttr`](../../workflow/compile_switches.go#L211-L269) to accept:
  - Two-segment traversals `step.<name>`, `state.<name>`, `switch.<name>`, `wait.<name>`, `approval.<name>` (already supported).
  - **Single-segment bare keywords** `return` and `continue` (TraverseRoot with no attribute). These lower to the existing internal sentinels (`ReturnSentinel = "return"` and `"_continue"`).
- Wire [`compileOutcomeBlock`](../../workflow/compile_steps_graph.go#L32) to call `resolveNextAttr` for every outcome.
- Internal sentinels (`ReturnSentinel`, the `"_continue"` string compared against in [compile_steps_graph.go:293](../../workflow/compile_steps_graph.go#L293) and in the engine iteration path) become **internal-only** representations — surface syntax never quotes them. Keep the constants for now; just don't let them appear in user-authored HCL.
- Legacy rejection: detect a string-literal `next = "..."` in an outcome and emit `next is now a node reference: write next = step.foo, next = state.done, next = return, or next = continue.`

### Step 2 — `data "internal" "name"` block

**Today:**
```hcl
shared_variable "cycle_count" {
  type  = number   # WS01 has already converted to type expression
  value = 0
}
# read:  shared.cycle_count
```

**Target:**
```hcl
data "internal" "cycle_count" {
  type  = number
  value = 0
}
# read:  data.internal.cycle_count.value
```

- New schema type `DataSpec` in [workflow/schema.go](../../workflow/schema.go):
  ```go
  type DataSpec struct {
      Kind   string         `hcl:"kind,label"` // first label, e.g. "internal"
      Name   string         `hcl:"name,label"` // second label
      Type   hcl.Expression `hcl:"type"`       // required; WS01-style type expression
      Remain hcl.Body       `hcl:",remain"`    // optional "value" + "description"
  }
  ```
- New compiled node `DataNode` mirroring [`SharedVariableNode`](../../workflow/schema.go#L34-L40):
  ```go
  type DataNode struct {
      Kind         string
      Name         string
      Type         cty.Type
      InitialValue cty.Value
      Description  string
  }
  ```
- [workflow/schema.go](../../workflow/schema.go) `Spec` and `SpecContent`: replace `SharedVariables []SharedVariableSpec` with `Data []DataSpec`. The `FSMGraph.SharedVariables` map and `SharedVariableOrder` slice are replaced by `Data map[string]map[string]*DataNode` (keyed by `kind` then `name`) and `DataOrder []DataRef` for stable iteration. *(Implementation note: choose whichever flat shape compiles cleanly — the surface contract is `data.<kind>.<name>.value`.)*
- New file `workflow/compile_data.go` paralleling [compile_shared_variables.go](../../workflow/compile_shared_variables.go). Initially only `kind = "internal"` is supported — emit a clear `unsupported data kind %q; only "internal" is currently supported` for anything else, paving a clean extension point for future kinds.
- Delete [workflow/compile_shared_variables.go](../../workflow/compile_shared_variables.go) and its test file; the new compile_data.go replaces them.

### Step 3 — Eval context: nested `data` namespace

- [workflow/eval.go](../../workflow/eval.go) `BuildEvalContext` (and `BuildEvalContextWithOpts`): replace the flat `shared = cty.ObjectVal{...}` entry with a nested `data = cty.ObjectVal{ internal = cty.ObjectVal{ <name> = cty.ObjectVal{ value = <current value>, type = <type cty representation> } } }`.
- Reads like `data.internal.cycle_count.value` work via standard cty traversal — no special parsing needed.
- If future kinds are added (e.g. `data "http"`), they slot in as additional keys under `data` with the same `value`-bearing shape.

### Step 4 — `write` block (replaces `shared_writes`)

**HCL grammar constraint:** attribute LHSs must be a single bareword identifier; `data.internal.x.value = expr` will not parse. Use a block-per-write shape — matches Terraform's `provisioner` block pattern. The block is singular (`write`) because each block updates exactly one target.

**Target:**
```hcl
outcome "success" {
  next = step.next

  write {
    target = data.internal.cycle_count.value
    value  = output.stdout
  }
  write {
    target = data.internal.last_msg.value
    value  = output.reason
  }
}
```

- New schema type `WriteSpec` in [workflow/schema.go](../../workflow/schema.go):
  ```go
  type WriteSpec struct {
      Target hcl.Expression `hcl:"target"` // traversal: data.<kind>.<name>.value
      Value  hcl.Expression `hcl:"value"`  // runtime-evaluated expression
  }
  ```
- Add `Writes []WriteSpec hcl:"write,block"` to `OutcomeSpec`.
- Replace [`CompiledOutcome.SharedWrites`](../../workflow/schema.go#L431-L453) (map[string]string) with `Writes []CompiledWrite`:
  ```go
  type CompiledWrite struct {
      DataKind  string         // resolved from target traversal
      DataName  string         // resolved from target traversal
      ValueExpr hcl.Expression // runtime-evaluated against the step's output scope
  }
  ```
- Compile-time validation:
  - `target` must be a four-segment traversal `data.<kind>.<name>.value` whose `<kind>.<name>` resolves to a declared data block. Anything else is a compile error with a clear message.
  - `value` references are validated the same way today's `shared_writes` keys are — must reference an output key that exists in the outcome's projected output (if `output = { ... }` is declared) or the adapter's output schema (if no projection).
  - Aggregate-iteration rule (currently in [compile_steps_graph.go:48-52](../../workflow/compile_steps_graph.go#L48-L52)) carries over: writes on aggregate iterating outcomes must reference a projected `output = { ... }`, never raw adapter output.
- Legacy rejection: detect `shared_writes = { ... }` in an outcome and emit `shared_writes has been replaced by per-target write blocks: write { target = data.internal.<name>.value, value = output.<key> }.`

### Step 5 — Engine runtime

- [internal/engine/](../../internal/engine/) — rename `SharedVarStore` → `DataStore`, keyed by `(kind, name)`. The runtime state machine treats only `kind == "internal"` as mutable; other kinds are read-only (locked at compile time for now, but the lock point lives here so future kinds slot in cleanly).
- [internal/engine/node_step.go](../../internal/engine/node_step.go) — replace `applySharedWrites` with `applyDataWrites`:
  - Iterate `CompiledOutcome.Writes`.
  - For each entry, evaluate `ValueExpr` against the post-projection output scope.
  - Apply the write to `DataStore[kind][name]` under the existing per-data lock (atomic across all writes from a single outcome — same guarantee `shared_writes` had).
- Outcome sentinel handling: ensure the iteration code paths that compare `co.Next == "_continue"` and `co.Next == ReturnSentinel` still work. Surface form changed (`continue` / `return` keywords); the compiled `Next` string is unchanged.

### Step 6 — VSCode grammar updates

Coordinated single update to [criteria-vscode-extension-v1/syntaxes/criteria-hcl.tmLanguage.json](../../../criteria-vscode-extension-v1/syntaxes/criteria-hcl.tmLanguage.json):

- Add `data` block matcher: `^(data)\s+("[^"]*")\s+("[^"]*")\s*\{` with `kind`/`name` capture groups.
- Drop the `shared_variable` matcher.
- Outcome body: `next = ` value highlighting — recognize `step.x`, `state.x`, `switch.x`, `wait.x`, `approval.x` (traversals) and bare `return` / `continue` (keywords); demote string-form `next = "..."` to a legacy-error class so users see the mismatch.
- Add `write` block inside outcome with `target` / `value` attributes.
- Drop `shared_writes` from the outcome attribute list.

### Step 7 — Migration rewrites

Workflows that use `shared_variable`/`shared_writes` and need full rewriting:

- `examples/phase3-shared-variable/main.hcl`
- `examples/llm-pack/07-shared-variable/main.hcl`
- `examples/archived/workstream_review_loop/**/*.hcl` (heavy user)
- `.criteria/workflows/develop/main.hcl`
- `.criteria/workflows/pr_review/main.hcl`
- `proposed_hcl.hcl`

Workflows that need `next` migrated (string → traversal):
- All `.hcl` files under `examples/`, `.criteria/workflows/`. Mechanical sed for `next = "x"` → `next = step.x`/`state.x` driven by graph context; `next = "return"` → `next = return`; `next = "_continue"` → `next = continue`. Spot-verify by compile.

Consider renaming `examples/phase3-shared-variable/` to `examples/phase3-data/` for consistency.

### Step 8 — Tests

- [workflow/parse_legacy_reject_test.go](../../workflow/parse_legacy_reject_test.go): add cases for `shared_variable` block, `shared_writes` attribute, and string-form `next` — assert that each migration hint appears.
- Positive tests:
  - `data "internal" "x" { type = number, value = 0 }` compiles; eval context exposes `data.internal.x.value`.
  - `write { target = data.internal.x.value, value = ... }` applies at runtime; concurrent writes use the same atomic lock semantics today's `shared_writes` does.
  - `next = step.foo`, `next = state.done`, `next = return`, `next = continue` all resolve to the correct compiled target.
  - Negative: `next = "step.foo"` (quoted) is rejected with the migration message.
  - Negative: `write { target = data.unknown_kind.x.value, ... }` is rejected with `unsupported data kind`.
- End-to-end:
  - `examples/phase3-shared-variable/main.hcl` (rewritten as `data "internal" {}`) — runs through with writes applied across steps.
  - `.criteria/workflows/develop/main.hcl` — exercises data reads in switch `match` and writes in outcomes. Confirm runtime semantics are byte-equivalent to the pre-migration run.
- Final sweep: `rg 'shared_variable|shared_writes|next = "[^"]+"' workflow/ examples/ .criteria/` returns zero hits in `.hcl` files.

## Out of scope

- Adding new `data` kinds beyond `internal`. The compile and eval paths are designed to make adding `data "http"`, `data "remote_state"`, etc. straightforward; the actual integrations are future work.
- Type-narrowing / write-side type-checking against the data's declared `type` (current `shared_writes` doesn't type-check writes against the variable's declared type either; preserve parity).
- Loop primitives, error handlers, structured concurrency surface — all future work.
- Adapter v2 work — separate track.

## Reuse pointers

- Existing [`resolveNextAttr`](../../workflow/compile_switches.go#L211-L269) — extend it; don't rewrite.
- Existing [compile_shared_variables.go](../../workflow/compile_shared_variables.go) — fork its compile shape into the new compile_data.go, then delete.
- Existing engine shared-store locking — preserve the atomicity semantics in the renamed `DataStore`.
- Existing aggregate-iteration write rule in [compile_steps_graph.go:48-52](../../workflow/compile_steps_graph.go#L48-L52) — keep the rule; apply it to `Writes` instead of `SharedWrites`.

## Behavior change

**User-facing surface:** every workflow file using `shared_variable` or any outcome with a string-form `next` changes shape.

**Runtime semantics:** unchanged. Same atomicity guarantees on writes, same iteration semantics for `return`/`continue`, same eval order for outcome projections vs writes.

**Future extensibility:** `data "<kind>" "<name>"` is the extension point. The legacy `shared_variable` and `shared_writes` constructs are gone permanently.

## Tests required

- All existing tests pass after fixture migration.
- New tests in Step 8 pass.
- `go vet ./...` clean.
- Manual: VSCode extension highlights migrated workflows correctly (data block, write block, traversal-form next, bare return/continue).
- Manual: `criteria run examples/phase3-shared-variable/main.hcl` (rewritten) executes successfully.
- Final grep sweep is clean (Step 8).
