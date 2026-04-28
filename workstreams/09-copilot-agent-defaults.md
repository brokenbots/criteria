# Workstream 9 — Copilot agent-level system prompt and reasoning effort

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md) · **Unblocks:** users currently working around the agent-level config gap by setting per-step config or by patching the Copilot adapter.

## Context

User feedback (raised in the planning conversation; new
user-story file authored as part of this workstream — see
"Tasks") flags that **system_prompt** and **reasoning_effort**
cannot reliably be set when defining a Copilot-backed agent, and
the workarounds are intrusive: copy the system prompt into every
prompt template, or hand-edit the adapter. The fields exist in
the schema and the agent-level `config { }` block accepts them,
but two specific gaps make them unreliable:

### Gap 1: `reasoning_effort` is silently dropped without `model`

In [cmd/criteria-adapter-copilot/copilot.go:173–181](../cmd/criteria-adapter-copilot/copilot.go),
`OpenSession` only invokes `SetModel` when `cfg["model"]` is
non-empty:

```go
if model := strings.TrimSpace(cfg["model"]); model != "" {
    var opts *copilot.SetModelOptions
    if effort := strings.TrimSpace(cfg["reasoning_effort"]); effort != "" {
        opts = &copilot.SetModelOptions{ReasoningEffort: &effort}
    }
    if err := s.session.SetModel(ctx, model, opts); err != nil {
        return nil, fmt.Errorf("copilot: set model at open: %w", err)
    }
}
```

If the agent is configured with `reasoning_effort = "high"` but
no explicit `model`, the `reasoning_effort` is read into `cfg`
and then silently discarded. The user sees no error and no
behavior change. Same issue at the per-request site
([copilot.go:305–313](../cmd/criteria-adapter-copilot/copilot.go)).

### Gap 2: per-step overrides are not accepted

The Copilot adapter's `InputSchema`
([copilot.go:130–133](../cmd/criteria-adapter-copilot/copilot.go))
declares only `prompt` and `max_turns` as accepted step-level
input fields. Authors who want a different `system_prompt` or
`reasoning_effort` for a single step (e.g. a planning step at
`reasoning_effort = "high"` followed by execution steps at
`"medium"`) cannot express that without defining a second agent
with a separate `config { }` block — which forces a separate
session, separate context, and the inability to share
conversation history.

### Gap 3: error surfaces lie

A workflow that sets `system_prompt` in the **step input** block
(rather than the agent config block) gets rejected with the
generic "unknown input field" error. The diagnostic does not
suggest moving the field to the agent config, which is the
correct fix.

This workstream closes all three gaps. The result: a workflow
author who writes `agent "bot" { config { system_prompt = "...",
reasoning_effort = "high", model = "claude-sonnet-4.6" } }` gets
exactly that behavior, and a workflow author who tries to
override per-step gets either accepted-and-applied or a clear
"move this to agent config" diagnostic.

## Prerequisites

- [W03](03-god-function-refactor.md) merged. The Copilot adapter
  `Execute` is refactored; in particular `applyRequestModel`
  (W03-extracted) is the helper this workstream fixes.
- `make ci` green on `main`.

## In scope

### Step 1 — Author the user-story file

This is a user-reported issue without an existing feedback file
yet. As the first task of this workstream, author:

**`user_feedback/09-copilot-agent-defaults-user-story.txt`**

Format follows the existing files in `user_feedback/`. Content:

```
User Story: Set system prompt and reasoning effort when defining
a Copilot-backed agent
Date: 2026-04-27

As a workflow author using the Copilot adapter,
I want to set system_prompt, reasoning_effort, and model directly
on the agent definition,
so that all sessions opened against that agent inherit the
configuration without per-step boilerplate.

Current pain:
- reasoning_effort silently does nothing if model is not also set.
- system_prompt and reasoning_effort cannot be overridden per
  step; the only escape is defining a duplicate agent.
- Setting these fields under "input" instead of "config" yields a
  generic "unknown field" error rather than guidance.

Acceptance criteria:
- reasoning_effort applies even when model is omitted at the
  agent level (uses the session's default model).
- system_prompt applied at agent open time persists for the life
  of the session.
- Per-step overrides for system_prompt and reasoning_effort are
  either accepted (with the documented scoping rule) or rejected
  with a diagnostic suggesting the agent config block.
- Validation surfaces a clear error when these fields appear in
  the wrong block.
```

This file is referenced by the rest of the workstream and by
[W10](10-phase1-cleanup-gate.md)'s archive accounting.

### Step 2 — Fix the silent `reasoning_effort` drop

In [cmd/criteria-adapter-copilot/copilot.go](../cmd/criteria-adapter-copilot/copilot.go):

`OpenSession` and `applyRequestModel` (the W03-extracted helper)
both currently gate the `SetModel` call on a non-empty `model`.
Change both sites so:

- If **either** `model` **or** `reasoning_effort` is set, call
  `SetModel`. When `model` is empty, pass an empty string and
  let the underlying SDK preserve its default model while
  applying the effort.
- If the underlying `copilot.SetModel` cannot accept an empty
  model + non-empty effort (verify against the SDK signature in
  the existing imports — likely
  `github.com/github/...copilot-sdk-go` or similar), implement
  the agent-side equivalent:
  - Open the session normally.
  - Read the session's current model from the SDK (whatever
    accessor exists — `session.Model()` or equivalent).
  - Call `SetModel(ctx, currentModel, &SetModelOptions{ReasoningEffort: &effort})`.

Do **not** silently swallow the case. If the SDK genuinely
cannot apply effort without a model, fail loudly at session
open with the exact message:

```
copilot: reasoning_effort %q requires an explicit model; either
set model in agent config or omit reasoning_effort
```

The reviewer must verify — by reading the SDK source vendored in
`go.mod` — which of the two paths is available, and document
the choice in reviewer notes.

### Step 3 — Decide and implement per-step override scope

Per-step overrides for `system_prompt` and `reasoning_effort` are
useful (the planning-vs-execution use case is real) but
introduce session-state ambiguity: changing `system_prompt`
mid-session means future turns see a different prompt, which is
not always what authors intend. The chosen rule:

- **`reasoning_effort`** can be overridden per step. The override
  applies only to that step's `Execute` call; the session's
  default effort restores at the end of the call. Implementation:
  read the current effort from the session before the override,
  apply the new value, and reset on `defer`.
- **`system_prompt`** **cannot** be overridden per step. The
  Copilot SDK's session model treats the system prompt as
  session-lifetime; mid-session reassignment is not supported
  cleanly. Authors who want a different system prompt define a
  second agent. Per-step `system_prompt` in the input block is
  rejected with a diagnostic naming agent config as the fix.

Update `InputSchema` accordingly:

```go
InputSchema: &pb.AdapterSchemaProto{Fields: map[string]*pb.ConfigFieldProto{
    "prompt":           {Required: true, Type: "string", Doc: "User prompt to send to the assistant."},
    "max_turns":        {Type: "number", Doc: "Per-step override for max assistant turns."},
    "reasoning_effort": {Type: "string", Doc: "Per-step override for reasoning effort. Resets to the session default after this step. Valid: low, medium, high, xhigh."},
}},
```

In `Execute` (post-W03 layout), wrap the existing
`applyRequestModel` call with a save-and-restore for the
session's effort. The save-and-restore lives in a new helper
`applyRequestEffort(ctx, session, cfg) (restore func(), err
error)` so the lifecycle is unambiguous.

If the underlying SDK does not expose "read current effort,"
fall back to "apply override; restore by re-applying the
agent-config effort recorded at OpenSession time." The
agent-config effort is captured into `sessionState` at session
open for exactly this purpose:

```go
type sessionState struct {
    // existing fields ...
    defaultModel  string
    defaultEffort string
}
```

### Step 4 — Better diagnostics for misplaced fields

The compile-time validator in
`workflow/compile_steps.go` (post-W04 location) already emits
"unknown field" diagnostics for unrecognized step-input fields.
Extend the diagnostic generator to recognize a known list of
**adapter-level** field names that authors commonly misplace:

```go
var knownAgentConfigFields = map[string][]string{
    "copilot": {"model", "reasoning_effort", "system_prompt", "max_turns", "working_directory"},
    // future adapters extend this list
}
```

When an unknown step-input field matches an entry in
`knownAgentConfigFields[adapterName]`, the diagnostic becomes:

```
field %q is not valid in step input for adapter %q; it belongs
in the agent config block:

  agent "<name>" {
    adapter = "%s"
    config {
      %s = ...
    }
  }
```

The list is wired through whatever existing schema/diagnostic
machinery the compiler already has; the goal is a string
substitution, not a new validation pass.

### Step 5 — Document and example

Update **`docs/plugins.md`** Copilot section:

- Lists the agent-level config fields with their default
  behavior.
- Lists the per-step overrideable fields explicitly.
- Includes a worked example of an agent with `system_prompt`,
  `reasoning_effort`, and `model` set, plus a step that
  overrides `reasoning_effort`.

Add a new example: `examples/copilot_planning_then_execution.hcl`.
The example:

- Defines one Copilot agent with `reasoning_effort = "medium"`.
- Has a planning step that overrides `reasoning_effort = "high"`.
- Has follow-up execution steps that inherit the agent default.

The example needs a real Copilot binary to run end-to-end; it
gates `make validate` for compile validation but is excluded
from the CLI smoke that runs in CI (which uses
`examples/hello.hcl`). Document this skip in the example file's
header comment so contributors know not to try
`./bin/criteria apply` on it without a Copilot installation.

### Step 6 — Tests

Tests live in three files:

`cmd/criteria-adapter-copilot/copilot_internal_test.go` (extend):

1. `OpenSession` with `reasoning_effort = "high"` and no `model`
   succeeds and applies the effort. Assert via the fake SDK
   session that `SetModel` was called with the expected effort
   (or the documented loud-failure path produces the expected
   error message — match the implementation chosen in Step 2).
2. `OpenSession` with both `reasoning_effort` and `model` set
   succeeds (regression guard).
3. `OpenSession` with `reasoning_effort = "invalid"` fails with
   a clear "valid values: low, medium, high, xhigh" error. The
   adapter validates the value against the documented set
   before calling the SDK.
4. `Execute` with per-step `reasoning_effort = "high"` applies
   the override for that step and restores the agent default
   on exit. Assert the SDK call sequence: `SetModel("high")`
   pre-Send, `SetModel(<agent_default>)` post-Send.
5. `Execute` with per-step `system_prompt` is **not** in scope
   here because `InputSchema` no longer accepts the field. The
   compile-time validator catches it; the adapter never sees it.

`workflow/compile_steps_diagnostics_test.go` (new):

6. A workflow with `step "x" { agent = "bot" input {
   system_prompt = "..." } }` (Copilot agent) fails compile with
   the new diagnostic naming agent config as the fix.
7. A workflow with the same misplacement but a different
   adapter (e.g. shell) keeps the existing generic
   "unknown field" diagnostic — the targeted message is only
   for adapter-known agent-level fields.

`cmd/criteria-adapter-copilot/conformance_test.go` (extend the
existing fixture):

8. The Copilot conformance fixture exercises the full agent →
   step → override flow with `reasoning_effort` to lock in the
   contract end-to-end. Run by `make test-conformance`.

### Step 7 — Migration of existing workflows

Audit `examples/` and `internal/cli/testdata/`:

- Any HCL fixture that currently sets `reasoning_effort` without
  `model` was previously a no-op; under the new behavior the
  effort actually applies. The semantic change is the bug fix —
  no migration needed beyond verifying the example still
  produces the intended output.
- Any HCL fixture that currently sets `system_prompt` in step
  input (instead of agent config) now fails compile. Update the
  fixture to use the agent config block. If a fixture was
  asserting the old "unknown field" diagnostic, update its
  golden output.

Run `make validate` and `make test`; address any breakage in
this workstream rather than punting.

## Out of scope

- Adding more Copilot config fields beyond what the SDK already
  supports (e.g. temperature, top_p). The schema can grow
  later; this workstream fixes what's documented.
- Implementing per-step `system_prompt` override semantics. The
  rule is "no" with a clear diagnostic.
- Changing other adapters' input schemas. The
  `knownAgentConfigFields` map is structured to accept future
  adapters but the only entry this workstream populates is
  `copilot`.
- Re-architecting how the Copilot SDK manages sessions.
- Adding observability for which model/effort was actually used
  on each turn (a future workstream may add this to the event
  stream).

## Files this workstream may modify

**Created:**

- `user_feedback/09-copilot-agent-defaults-user-story.txt`
- `workflow/compile_steps_diagnostics_test.go`
- `examples/copilot_planning_then_execution.hcl`

**Modified:**

- `cmd/criteria-adapter-copilot/copilot.go`
- `cmd/criteria-adapter-copilot/copilot_internal_test.go`
- `cmd/criteria-adapter-copilot/conformance_test.go`
- `cmd/criteria-adapter-copilot/testfixtures/` (extend with new
  fixture if needed for tests 1–4; keep the fixture small and
  focused)
- `workflow/compile_steps.go` (post-W04 location; targeted
  diagnostic for misplaced agent-config fields)
- `workflow/schema.go` (only if `InputSchema` registration
  surfaces; otherwise leave unchanged)
- `internal/cli/testdata/` (golden updates for any plan/compile
  outputs whose diagnostics now read differently)
- `docs/plugins.md`
- `examples/` (update any existing fixture that misplaces these
  fields)
- `.golangci.baseline.yml` (delete entries pointed at this
  workstream, if any)

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any
other workstream file. CHANGELOG entries are deferred to
[W10](10-phase1-cleanup-gate.md).

## Tasks

- [ ] Author `user_feedback/09-copilot-agent-defaults-user-story.txt`
      per Step 1.
- [ ] Fix the `reasoning_effort` drop in `OpenSession` and
      `applyRequestModel` per Step 2; pick the SDK path
      (empty-model SetModel vs read-then-apply) and document the
      choice in reviewer notes.
- [ ] Validate `reasoning_effort` values against the documented
      set (`low`, `medium`, `high`, `xhigh`).
- [ ] Capture `defaultModel` and `defaultEffort` on
      `sessionState` at session open.
- [ ] Add per-step `reasoning_effort` override with
      save-and-restore semantics per Step 3.
- [ ] Update `InputSchema` to declare `reasoning_effort`.
- [ ] Add `knownAgentConfigFields` and the targeted misplacement
      diagnostic per Step 4.
- [ ] Update `docs/plugins.md` Copilot section.
- [ ] Add `examples/copilot_planning_then_execution.hcl`.
- [ ] Add the 8 tests listed in Step 6.
- [ ] Migrate any existing fixtures broken by the new
      validation per Step 7.
- [ ] `make ci`, `make lint-go`, `make test-conformance`,
      `make validate` all green.

## Exit criteria

- A workflow with `agent "bot" { adapter = "copilot" config {
  reasoning_effort = "high" } }` (no model) actually applies
  high effort, verified by the test in Step 6.1.
- A workflow with per-step `reasoning_effort` override applies
  the override for that step and restores the agent default
  afterwards (test 6.4).
- A workflow that places `system_prompt` in step input fails
  compile with the targeted diagnostic naming agent config
  (test 6.6).
- The Copilot conformance fixture (test 6.8) exercises the
  full agent + per-step override path and passes
  `make test-conformance`.
- Invalid `reasoning_effort` values are rejected with a clear
  message listing the valid set.
- `docs/plugins.md` documents the agent-level fields and the
  per-step override scope rule.
- `examples/copilot_planning_then_execution.hcl` validates
  successfully.
- No new entries in `.golangci.baseline.yml`.
- The new user-story file lives at the correct path with the
  correct numbering.

## Tests

8 tests listed verbatim in Step 6. Test 6.8 is the conformance-level
gate; tests 6.1–6.5 are adapter-internal; tests 6.6–6.7 are
compile-level. All must run in `make test` /
`make test-conformance` and gate CI.

## Risks

| Risk | Mitigation |
|---|---|
| The Copilot SDK does not support `SetModel` with an empty model | Step 2 lists the read-then-apply fallback. The reviewer verifies the SDK signature and documents which path was chosen. The loud-failure path is the third option if neither approach works; that turns the silent drop into an explicit error, which is still strictly better than today. |
| Per-step `reasoning_effort` override creates a session-state-restoration bug | The save-and-restore is `defer`-based and the restored value is captured at `OpenSession` time, not read from the live session (which could have been mutated by a prior override). Test 6.4 asserts the exact SDK call sequence. |
| Updating `InputSchema` to add `reasoning_effort` breaks the Copilot conformance suite | The conformance suite exercises the documented contract; adding an optional field is backward-compatible. Test 6.8 exercises the full path. |
| The targeted-diagnostic message becomes a maintenance burden as more adapters get fields | The list is a static map keyed by adapter name. New adapters that want this treatment add an entry; adapters that don't continue to emit the generic diagnostic. The cost scales linearly. |
| The user-story file numbering collides with another workstream's numbering | This workstream owns the `09-` prefix in `user_feedback/` (the existing files are 01–08; 09 is the next). The numbering matches this workstream's number, which is incidental but convenient. |
| Migration of existing fixtures requires updates to many golden files | `internal/cli/testdata/` golden output is regenerated via the existing test infrastructure; the diff is mechanical. Reviewer enforces that diffs are limited to the diagnostic message line and not other fields. |
| The example workflow `copilot_planning_then_execution.hcl` cannot run in CI without a Copilot binary | Documented in the file header; `make validate` does compile validation only. End-to-end execution is a manual smoke. The Copilot conformance suite (existing) provides automated coverage of the runtime path. |
| Captured `defaultEffort` becomes stale if a future feature dynamically updates the agent default mid-run | No such feature exists; if added later, it must update the captured value. Document the invariant in `sessionState`'s comment. |
| Authors interpret "system_prompt is not per-step overrideable" as a bug rather than a deliberate choice | The diagnostic and the docs both name the constraint as deliberate (session-lifetime semantics from the SDK). If the constraint becomes a hot user complaint after release, follow up with explicit "named system prompts" or multi-agent patterns in Phase 2. |
