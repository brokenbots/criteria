# Architecture Notes — Workflow Scope, Variable Resolution, Sub-Workflows

Working notes for a planned rework of the workflow syntax / execution model.
Captures the current behavior of the FSM compiler + engine, the gaps against
the proposed direction, and where the mechanical groundwork already exists.

## Proposed direction (summary)

1. The execution graph should be **validated at compile time** to maximise
   determinism. Variables and locals must be resolvable at compile.
2. **Steps stay runtime** — step outputs are runtime values.
3. If we need to pass data between steps (or hold scope that mutates across
   steps), it should be a **dedicated block / data structure**, not implicit
   read/write of `var.*`.
4. A `workflow` step is a **new scope** in the execution graph and should
   support **all the same blocks** as a top-level workflow (agent, variable,
   etc.), not the current subset.
5. Inner scopes should not implicitly read the outer scope. Prefer explicit
   **input variables** passed into the sub-workflow. A sub-workflow could be a
   valid top-level workflow that was imported, so treat it identically.
6. As a consequence, **top-level workflows should themselves be invocable with
   `for_each` / `count`**.

---

## How variable & step resolution works today

### Variables — compile-time bound, runtime-evaluated

- `variable "name" { type, default, description }` is parsed into
  `VariableSpec` ([workflow/schema.go:35](workflow/schema.go#L35)) and compiled
  into a `VariableNode { Type, Default cty.Value }` keyed by name in
  `FSMGraph.Variables`
  ([compile_variables.go:51](workflow/compile_variables.go#L51)).
- The default expression is evaluated at compile with a `nil` context (no
  functions, no refs), then coerced to the declared type. So defaults must be
  pure literals.
- At run start, `SeedVarsFromGraph` builds `vars["var"]` as a cty object from
  the defaults; CLI `--var k=v` overrides are merged via `ApplyVarOverrides`
  ([eval.go:160](workflow/eval.go#L160),
  [eval.go:185](workflow/eval.go#L185)). Variables with no default and no
  override end up as `cty.NullVal(typ)` (silent — not a compile error).
- **There are no `local`s.** Nothing is ever folded — variables live as
  runtime cty values in `RunState.Vars`.

### Step inputs — deferred to runtime

- `compileSteps` decodes each `step.input { }` attribute by calling
  `attr.Expr.Value(nil)`
  ([compile_validation.go:26](workflow/compile_validation.go#L26)). If the
  expression has *any* HCL traversal (`var.x`, `each.value`,
  `steps.foo.bar`) or any function call, that nil-context evaluation errors
  and the value is silently stored as `""`. The raw `hcl.Expression` is then
  captured in `StepNode.InputExprs`.
- At step entry, `node_step.resolveInput`
  ([node_step.go:343](internal/engine/node_step.go#L343)) calls
  `ResolveInputExprsWithOpts(InputExprs, st.Vars, opts)` to evaluate the
  expressions against the current `var/steps/each` cty objects, with
  `file/fileexists/trimfrontmatter` registered
  ([eval.go:79](workflow/eval.go#L79)).
- The compiler does **no** validation that a referenced variable exists, that
  a `steps.foo.bar` path is reachable in the graph, or that types line up —
  those are all runtime errors.

### `file()` at compile (the reported bug)

- `validateFileFunctionCalls`
  ([compile_validation.go:62](workflow/compile_validation.go#L62)) walks
  `step.input` attributes and evaluates expressions through a
  `fileValidateFunction` that does stat-only checks. **It explicitly skips
  any expression containing variable references**
  (`if len(attr.Expr.Variables()) > 0 { continue }`), so `file(var.path)` is
  never validated even when `var.path` has a known constant default.
- It is only wired for `step.input`. `agent.config { }`, branch `when`
  expressions, `for_each` / `count` expressions, and `output { value = ... }`
  blocks are not validated at compile.
- Worse: `agent.config` evaluates with `nil` ctx and stores `""` on any error
  ([compile_agents.go:30-43](workflow/compile_agents.go#L30)). It also
  doesn't capture `inputExprs`, so there is **no runtime evaluation either**.
  `file(...)` inside `agent.config` is silently dropped to `""` at compile
  and never re-evaluated. This is almost certainly the user-reported bug.

### Sub-workflow scope (the second issue)

- `WorkflowBodySpec` ([schema.go:108](workflow/schema.go#L108)) only allows
  `step`, `state`, `wait`, `approval`, `branch`, `output`, `entry`. **No
  `agent`, no `variable`, no `policy`, no `permissions`.** `buildBodySpec`
  ([compile_steps.go:418](workflow/compile_steps.go#L418)) carries those
  forward verbatim into the synthetic Spec, so the body's `g.Agents` is
  empty at compile — referencing an agent fails with "unknown agent".
- At runtime, `runWorkflowBody`
  ([node_workflow.go:42](internal/engine/node_workflow.go#L42)) shares the
  parent's `Vars` map with the child (`childSt.Vars = st.Vars`). So `var.*`
  and `steps.*` from the outer scope are accessible inside the body **at
  runtime**, but the body's compile-time graph has zero variables — meaning
  the asymmetry is real and unchecked.
- `workflow_file = "..."` does compile via the full Spec path with
  variables/agents (`compileWorkflowBodyFromFile`), but the resolver isn't
  wired into the CLI yet (Phase 1 carry-over). So today only inline
  `workflow { }` bodies ship, and those are the structurally deficient ones.
- Top-level `for_each` / `count` does not exist. Iteration is a step
  attribute only; there is no way to iterate a whole workflow.

---

## Gap table (current vs proposed)

| Goal | Today | Gap |
|------|-------|-----|
| Variables fully resolved at compile | Defaults compiled, but stored as runtime cty values; references unchecked; no `local` | Add `local { }`, fold `var.*`/`local.*` to constants where possible, validate referenced names at compile |
| `file()` resolves at compile | Only when args are pure literals, only inside `step.input` | Extend folding to any compile-resolvable expression; cover `agent.config`, `branch.when`, `output.value`, `for_each`, `count` |
| Step outputs runtime-only | True | Already correct |
| Explicit step-to-step data block | Implicit via `var.*` and `steps.*` mixed together | Need a dedicated block (e.g. `result` / `scope` / `state`) so step writes don't pollute "variables" semantics |
| Sub-workflow = full workflow scope | `WorkflowBodySpec` is a subset; body shares parent's `Vars` map at runtime, has zero variables/agents at compile | Make body schema identical to top-level Spec; require explicit `input { }` to the sub-workflow; drop implicit parent-scope read |
| Sub-workflows treated as importable workflows | `workflow_file` exists in schema but unwired; inline form is structurally different from a real workflow | Unify on one form: a sub-workflow IS a Spec; the `workflow` step takes either a path or an inline Spec, plus inputs |
| Top-level `for_each` / `count` | Step-level only | Lift iteration semantics to the workflow header; reuse the same cursor / each-binding plumbing |

---

## What to keep — mechanical groundwork already in place

The engine is closer than the schema. The pieces below already treat a
workflow body as an independently runnable graph that produces outputs:

- Iteration cursor (`IterCursor`), `WithEachBinding`, `EachBinding`,
  `routeIteratingStepInGraph`, `finishIterationInGraph` — graph-agnostic;
  reused by both the engine main loop and the body sub-loop.
- `runWorkflowBody` ([internal/engine/node_workflow.go](internal/engine/node_workflow.go))
  already runs a body to a terminal state with its own `RunState` and shared
  deps; only the `Vars` aliasing needs to flip to explicit-inputs.
- `BuildEvalContextWithOpts` and `ResolveInputExprsWithOpts` already handle
  scoped evaluation against an arbitrary cty object map.
- Compile-time validation infrastructure (`validateFileFunctionCalls`,
  `validateSchemaAttrs`, schema-aware decode) exists; the rework is mostly
  **broadening where it runs** rather than inventing new machinery.

The biggest design call: whether sub-workflow scope inherits from the outer.
The runtime currently inherits (shared `Vars`), but the compile-time graph
doesn't know about that inheritance — which is the worst of both worlds.
Picking the **explicit-inputs-only** model and removing the runtime sharing
would simplify the engine (no cross-scope `Vars` aliasing) and make the
compile-time graph truthful.

---

## Suggested rework outline (rough)

1. **Schema unification.** Drop `WorkflowBodySpec` as a distinct type. A
   sub-workflow IS a `Spec`. The `workflow` step takes either an inline Spec
   or a path (`workflow_file`), plus an `input { }` block to bind values to
   the child's declared `variable`s.
2. **Compile-time fold pass.** Introduce a small constant-folding evaluator
   that, given declared `variable` defaults and `local` definitions, resolves
   any expression whose free variables are entirely in the
   `var ∪ local ∪ literal` set. Use that to:
   - Validate `file()` / `fileexists()` arguments wherever they appear.
   - Validate that all referenced variable names exist.
   - Pre-compute attributes that don't depend on runtime values (steps,
     each).
3. **Iteration lifted to header.** `workflow { for_each = ..., count = ... }`
   reuses the existing cursor plumbing; engine's outer loop becomes a thin
   wrapper that runs the workflow once per iteration, with `each.*` bound.
4. **Explicit step-to-step data block.** Decide whether step outputs live in
   `steps.<name>.<key>` (current) or move to a named scope block; either way,
   make the namespace distinct from `var.*` so reads/writes don't conflate
   "input parameter" with "transient state".
5. **Drop runtime `Vars` aliasing across scopes.** Each sub-workflow gets its
   own seeded `Vars` from its declared variables + the parent's `input { }`
   bindings. Outputs flow back via `output { }` blocks, as today.
