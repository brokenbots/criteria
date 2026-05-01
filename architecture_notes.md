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

---

## Tool-First Copilot Outcome Finalization (planned, not yet implemented)

Working design notes for replacing the Copilot adapter's free-text outcome
parsing with a structured tool-call finalization. Captured here so the design
context is not lost between workstreams; no code on this has landed yet.

### Why

Today the Copilot adapter derives the step outcome by scanning the final
assistant message for a `result:` prefix in
[cmd/criteria-adapter-copilot/copilot_turn.go](cmd/criteria-adapter-copilot/copilot_turn.go)
(see `parseOutcome`, default `needs_review`). This is brittle:

1. Models drift from the convention; outcomes silently become `needs_review`.
2. Allowed outcomes are not communicated to the model in any structured way —
   the engine validates the result against `StepNode.Outcomes` only after the
   adapter has already committed to a string (see
   [internal/engine/node_step.go](internal/engine/node_step.go) around the
   "produced unmapped outcome" guard).
3. There is no explicit wire contract between the engine's compiled outcome
   set and the adapter — only HCL-side knowledge.

### Direction

Move finalization to a structured tool call (`submit_outcome`) backed by an
explicit wire contract. The engine sends the step's allowed outcomes to the
adapter; the adapter exposes a custom tool whose handler validates and
records the chosen outcome; the adapter returns that outcome via
`ExecuteResult` instead of parsing prose.

Validated against `github.com/github/copilot-sdk/go v0.3.0` (latest tag, Apr
24, 2026):

1. `SessionConfig.Tools` + `copilot.DefineTool` support custom tools at session
   creation.
2. `Tool.SkipPermission` lets the internal `submit_outcome` tool bypass
   permission prompts (covered by the new `"custom-tool"` permission kind in
   v0.3.0 scoped permissions).
3. There is **no public API in the Go SDK for live tool mutation on an
   existing Session** — `Session.registerTools` is unexported. The only
   public way to swap tools while preserving conversation history is
   `Client.ResumeSessionWithOptions(ctx, sessionID, &ResumeSessionConfig{Tools: ...})`,
   which issues an extra RPC and returns a new `*Session` pointer.
4. With adapter isolation on the roadmap, recreating sessions per step would
   be expensive, so the design avoids both `CreateSession`-per-step and
   `ResumeSessionWithOptions`-per-step.

### Plan: Tool-First Copilot Outcome Finalization

Move outcome selection from fragile free-text parsing to a structured
finalization tool call. The adapter registers an internal `submit_outcome`
tool **once at OpenSession** and finalizes from validated tool-call arguments
rather than from assistant prose. Per-step scoping is handled by the adapter
holding the active step's allowed outcomes on `sessionState` and validating
in the tool handler at call time.

#### Phase 1 — Wire contract for allowed outcomes

1. Extend `ExecuteRequest` in
   [proto/criteria/v1/adapter_plugin.proto](proto/criteria/v1/adapter_plugin.proto)
   with a `repeated string allowed_outcomes` field.
2. Regenerate Go bindings via `make proto` (this is a breaking SDK change per
   [CONTRIBUTING.md](CONTRIBUTING.md) bump policy — bump accordingly).
3. Populate `allowed_outcomes` deterministically from `StepNode.Outcomes` map
   keys, sorted, when the host issues `Execute` in
   [internal/plugin/loader.go](internal/plugin/loader.go) (`rpcPlugin.Execute`,
   currently around L204 where it builds `ExecuteRequest`).
4. Engine continues to enforce the unmapped-outcome guard in
   [internal/engine/node_step.go](internal/engine/node_step.go) as
   defense-in-depth.

#### Phase 2 — Per-step `submit_outcome` semantics with one-time tool registration

1. Define a typed parameter struct with `Outcome string` (required) and
   `Reason string` (optional). The schema **does not** encode an enum for
   `Outcome` — Go SDK v0.3.0 has no public live-tool mutation, and refreshing
   the enum would require `ResumeSessionWithOptions` per step, which violates
   the no-recreate constraint.
2. Register `submit_outcome` exactly once at `OpenSession` via
   `SessionConfig.Tools` in
   [cmd/criteria-adapter-copilot/copilot_session.go](cmd/criteria-adapter-copilot/copilot_session.go)
   (`buildSessionConfig`), with `SkipPermission = true` so the internal tool
   never prompts the user.
3. Per `Execute`, write the request's `allowed_outcomes` (and an attempt
   counter) onto `sessionState` **before** sending the prompt. The handler
   uses this state to enforce allowed values at call time, scoping
   enforcement per step without touching session lifecycle.
4. Tool handler behavior:
   - Valid `Outcome` (member of active allowed set): record on the per-execute
     turn state and return a small success payload to the model.
   - Invalid `Outcome`: return a tool-error `ToolResultObject` that nudges the
     model toward the allowed set without ending the turn (so the model can
     retry within the same turn before the reprompt loop kicks in).
5. Future-compat: if a future SDK exposes live tool injection (or we accept
   `ResumeSessionWithOptions` cost), swap to true per-step schema-enum tools
   without changing the validation contract.

#### Phase 3 — Finalize from tool-call result, with adapter-level reprompt up to 3 attempts

1. Track whether `submit_outcome` was invoked exactly once with a valid
   argument during the current turn.
2. On `SessionIdle`, if a valid finalize was recorded, return that outcome
   via `resultEvent`.
3. If no valid finalize was recorded, send a corrective reminder prompt
   instructing the model to call `submit_outcome` with one of the allowed
   outcomes, and wait for the next idle. Repeat up to **3 total attempts**
   (initial + 2 reprompts).
4. Each reprompt counts toward `max_turns`; if `max_turns` is reached first,
   treat as the existing `needs_review` path **only if** `needs_review` is in
   the allowed set, otherwise fall back to `failure`.
5. After 3 unsuccessful attempts, return `failure` with a structured
   diagnostic that includes the declared outcomes and the reason (missing
   call, invalid enum, duplicate calls, conflicting calls).
6. Permission-denied paths remain failure-terminating as today;
   `submit_outcome` itself is permission-skipped so it cannot trigger a
   permission-denial.

#### Phase 4 — Tests and conformance

1. Update the fake Copilot fixture used by adapter tests to optionally
   simulate tool calls to `submit_outcome` (valid, invalid, missing, and
   duplicate variants).
2. Adapter unit tests covering: happy-path single finalize; reprompt then
   success on second attempt; reprompt twice then success on third; three
   failures then `failure` outcome; invalid enum; duplicate finalize calls;
   permission-denied unrelated tool during finalize attempt.
3. Transport-level tests verifying `allowed_outcomes` propagation from step
   declarations through `internal/plugin/loader.go`.
4. Conformance: deterministic outcome via tool path under happy and
   reprompt-recovered scenarios; `failure` under exhausted reprompts.

#### Phase 5 — Docs and rollout

1. Document the `submit_outcome` contract, per-step scope, permission-skip
   behavior, and the 3-attempt reprompt policy in
   [docs/plugins.md](docs/plugins.md).
2. Document the removal/deprecation of `result:` prose parsing and the
   strict `failure` policy when reprompts are exhausted.
3. Note in [CHANGELOG.md](CHANGELOG.md) that this is a breaking SDK change
   (proto field on `ExecuteRequest`) and that downstream orchestrators must
   forward `allowed_outcomes` per step.

### Decisions (locked)

1. Tool-call finalization replaces prose parsing; do not keep the prose path
   as a silent fallback.
2. Enforcement is strict: invalid finalization after reprompts returns
   `failure`, not `needs_review`.
3. Wire contract change is mandatory regardless of which session-lifecycle
   path is chosen — the adapter must know the allowed set.
4. Tool registration is **per session, once** with per-step state-driven
   validation; do **not** recreate the session per step and do **not** call
   `ResumeSessionWithOptions` per step (cost concern under future adapter
   isolation).
5. `submit_outcome` is registered with `SkipPermission = true` so the
   internal finalization tool never prompts the user.
6. The 3-attempt reprompt logic lives in the adapter, not the engine.
7. Engine's unmapped-outcome guard stays as defense-in-depth.

### Open questions / further considerations

1. Whether to allow optional metadata on `submit_outcome` (e.g. `confidence`,
   structured `reason`) or keep the schema minimal for reliability. Current
   plan: `Outcome` required, `Reason` optional string.
2. Whether to file an upstream SDK enhancement request for a public
   `Session.SetTools` / `AddTools` API so we can adopt true per-step
   schema-enum tools without `ResumeSessionWithOptions` overhead.
3. Tool name collision policy if other adapters or sub-agents expose tools —
   `submit_outcome` is adapter-private; confirm Copilot Go SDK v0.3.0
   `defaultAgent.excludedTools` semantics do not interfere when we move to
   the orchestrator pattern.

### PR sizing

Estimated total ~750–900 LOC across proto, plugin loader, adapter session/turn
code, fake Copilot fixture, adapter unit tests, transport tests, conformance,
and docs. Recommended split:

1. **PR-A (small, mechanical):** Proto field + regen + loader population +
   transport test. No behavior change in the adapter yet.
2. **PR-B (behavior + tests):** Register `submit_outcome`, per-step state,
   handler, 3-attempt reprompt, remove prose parsing, fake harness, full unit
   + conformance matrix, docs, CHANGELOG.

If shipping as a single PR, structure commits by phase so review can proceed
phase-by-phase.

### Relevant files

1. [cmd/criteria-adapter-copilot/copilot_session.go](cmd/criteria-adapter-copilot/copilot_session.go)
   — capability insertion point for session tool registration.
2. [cmd/criteria-adapter-copilot/copilot_turn.go](cmd/criteria-adapter-copilot/copilot_turn.go)
   — finalization acceptance logic (tool-first or strict fallback).
3. [proto/criteria/v1/adapter_plugin.proto](proto/criteria/v1/adapter_plugin.proto)
   — `allowed_outcomes` contract extension.
4. [internal/plugin/loader.go](internal/plugin/loader.go) — populate
   `Execute` request with `allowed_outcomes` from step outcomes.
5. [internal/engine/node_step.go](internal/engine/node_step.go) —
   defense-in-depth unmapped-outcome guard (unchanged).
6. [docs/plugins.md](docs/plugins.md) — behavior docs for finalization
   contract.
7. [CHANGELOG.md](CHANGELOG.md) — release notes for behavior/contract change.
