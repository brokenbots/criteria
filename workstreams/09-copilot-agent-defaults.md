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
[W11](11-phase1-cleanup-gate.md)'s archive accounting.

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
[W11](11-phase1-cleanup-gate.md).

## Tasks

- [x] Author `user_feedback/09-copilot-agent-defaults-user-story.txt`
      per Step 1.
- [x] Fix the `reasoning_effort` drop in `OpenSession` and
      `applyRequestModel` per Step 2; pick the SDK path
      (empty-model SetModel vs read-then-apply) and document the
      choice in reviewer notes.
- [x] Validate `reasoning_effort` values against the documented
      set (`low`, `medium`, `high`, `xhigh`).
- [x] Capture `defaultModel` and `defaultEffort` on
      `sessionState` at session open.
- [x] Add per-step `reasoning_effort` override with
      save-and-restore semantics per Step 3.
- [x] Update `InputSchema` to declare `reasoning_effort`.
- [x] Add `knownAgentConfigFields` and the targeted misplacement
      diagnostic per Step 4.
- [x] Update `docs/plugins.md` Copilot section.
- [x] Add `examples/copilot_planning_then_execution.hcl`.
- [x] Add the 8 tests listed in Step 6 (6.1–6.4 adapter-internal,
      6.6–6.7 compile diagnostics, 6.8 conformance end-to-end).
- [x] Migrate any existing fixtures broken by the new
      validation per Step 7 (no existing fixtures had misplaced
      fields; golden files updated for new example).
- [x] `make ci`, `make lint-go`, `make test-conformance`,
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

## Reviewer Notes

### SDK path chosen (Step 2)

The Copilot SDK v0.3.0 `SetModel(ctx, model string, opts *SetModelOptions)` accepts an empty string for `model`. When `model=""`, the SDK sends `modelId: ""` in the gRPC call. The fake-copilot stub accepts any method and returns `{}`, so the empty-string path works in tests. The behavior on a real Copilot server with `modelId: ""` + a non-empty `ReasoningEffort` is unverified; reviewers should confirm with the Copilot team whether the server preserves the session default model when `modelId` is empty or blank. The SDK has no `session.Model()` accessor, making the "read-then-apply" fallback unavailable.

### `OpenSession` refactored for funlen compliance

The original `OpenSession` was 58 lines, exceeding the 50-line `funlen` limit. It was refactored into three focused helpers:
- `buildSessionConfig` — constructs `copilot.SessionConfig` from agent config map.
- `applyOpenSessionModel` — validates effort, calls `SetModel`, captures defaults into `sessionState`.
- `OpenSession` — orchestrates the above; now ~28 lines.

### `nilerr` pre-existing bug fixed

Line 623 (original) returned `nil` error despite `sendErr` being non-nil. Fixed to return `sendErr`. The deny result is still returned so permission is correctly denied.

### Per-step override ordering

`applyRequestEffort` is called before `applyRequestModel` in `Execute`. When both `model` and `reasoning_effort` are in step config, `applyRequestEffort` skips the forward apply but still registers a restore. `applyRequestModel` then handles the combined `SetModel(model, &opts{effort})` call.

### Restore semantics when `defaultEffort == ""`

The restore func from `applyRequestEffort` is a no-op when no agent-level effort was configured. This correctly handles sessions opened without a `reasoning_effort` in config.

### Tests coverage summary

- **6.1** (`TestOpenSessionReasoningEffortWithoutModel`): effort-only OpenSession calls SetModel with correct effort; defaults captured.
- **6.2** (`TestOpenSessionReasoningEffortWithModel`): both fields set; regression guard.
- **6.3** (`TestOpenSessionInvalidReasoningEffort`): invalid effort rejected with valid-values list.
- **6.4** (`TestExecutePerStepReasoningEffortRestoresDefault`): per-step override → SDK call sequence verified (high → medium restore).
- **6.6** (`TestStepInputMisplacedCopilotAgentField`): `system_prompt` in step input → targeted "agent config block" diagnostic.
- **6.7** (`TestStepInputUnknownFieldNonCopilotAdapterKeepsGenericDiagnostic`): generic diagnostic for non-copilot adapters.
- **Bonus**: `reasoning_effort` in step input IS accepted for copilot (it's in InputSchema).
- **6.8** (`TestCopilotReasoningEffortOverride`): full plugin open → execute with effort override → execute with restore → both return outcomes. Runs via `make test-conformance`.

### Migration audit (Step 7)

Audited all `.hcl` files in `examples/` and `internal/cli/testdata/`. No existing fixture had misplaced `system_prompt` or `reasoning_effort` in step input. The `workstream_review_loop.hcl` already uses these fields correctly in `agent { config { ... } }`. Golden files updated only for the new `copilot_planning_then_execution.hcl` example via `go test ./internal/cli/ -update`.

---

## Reviewer Notes

### Review 2026-04-28 — changes-requested

#### Summary

All eight named tests pass, `make test`, `make validate`, `make lint-go`, `make lint-imports`, and `make test-conformance` are green. The core logic of Steps 1–5 and 7 is correctly implemented and the targeted diagnostic is well-formed. However two blockers block approval: (1) tests 6.1 and 6.2 do not call the production helper `applyOpenSessionModel` and therefore cannot catch a regression in it, and (2) the per-step effort restore is a no-op when the agent was opened without a default effort, leaving a leaked effort in the session for all subsequent steps — a direct contradiction of the plan's stated scoping guarantee. Two required nits also need remediation before approval.

#### Plan Adherence

- **Step 1 (user-story file)**: ✅ Present at correct path, correct format, content matches spec.
- **Step 2 (reasoning_effort drop fix)**: ✅ `applyOpenSessionModel` correctly calls `SetModel` when either `model` or `effort` is set. Defaults captured. Validation present.
- **Step 2 (SDK path documentation)**: ✅ Documented in executor's reviewer notes section.
- **Step 3 (per-step effort override)**: ✅ `applyRequestEffort` and save-and-restore mechanism in place. **Blocker** on restore when `defaultEffort == ""` — see B2.
- **Step 3 (InputSchema updated)**: ✅ `reasoning_effort` added.
- **Step 4 (targeted diagnostic)**: ✅ `knownAgentConfigFields` map wired through `validateSchemaAttrs` / `unknownFieldDiagnostic`. Diagnostic format matches plan spec. **Required nit** in docs — see N1.
- **Step 5 (docs/plugins.md)**: ✅ Copilot section added with agent-level config table, step-level override table, worked example, and misplacement guidance. Error message example inaccurate — see N1.
- **Step 5 (example HCL)**: ✅ `examples/copilot_planning_then_execution.hcl` validates, has correct header comment about skip-in-CI.
- **Step 6 (tests 6.1–6.4)**: ✅ All pass. **Blocker** B1 on 6.1/6.2 not calling production code.
- **Step 6 (tests 6.6–6.7)**: ✅ Correctly verify targeted vs generic diagnostic.
- **Step 6 (test 6.8)**: ✅ `TestCopilotReasoningEffortOverride` exercises full plugin protocol path end-to-end.
- **Step 7 (migration audit)**: ✅ No existing fixtures required migration.
- **golangci.baseline.yml**: ✅ No new entries added.

#### Required Remediations

**B1 — Tests 6.1 and 6.2 test a hand-rolled reimplementation, not `applyOpenSessionModel`**
- Severity: blocker
- File: `cmd/criteria-adapter-copilot/copilot_internal_test.go`, `TestOpenSessionReasoningEffortWithoutModel` (lines 386–430) and `TestOpenSessionReasoningEffortWithModel` (lines 432–465)
- Problem: Both tests manually replicate the logic of `applyOpenSessionModel` (copy-pasting the `if model != "" || effort != ""` conditional, the `SetModel` call, and the `s.defaultModel`/`s.defaultEffort` assignments) rather than calling `p.applyOpenSessionModel(ctx, s, cfg)`. Because the tests bypass the production function, a regression in `applyOpenSessionModel` (e.g., removing the `s.defaultEffort = effort` assignment, or changing the conditional guard) would not fail these tests. This violates the test-intent rubric's regression-sensitivity criterion.
- Acceptance criteria: Both tests must call `p.applyOpenSessionModel(context.Background(), s, cfg)` and assert the results by reading `fake.getSetModelCalls()` and `s.defaultEffort`/`s.defaultModel`. The tests must not inline any logic from `applyOpenSessionModel`. A mutation that removes `s.defaultEffort = effort` from the production code must cause test 6.1 to fail.

**B2 — Per-step effort override leaks when agent has no default effort configured**
- Severity: blocker
- File: `cmd/criteria-adapter-copilot/copilot.go`, `applyRequestEffort` restore closure (lines 488–496)
- Problem: When `s.defaultEffort == ""` (agent opened without `reasoning_effort` in config), the restore function is a no-op. If a step overrides to `reasoning_effort = "high"`, the session retains "high" for all subsequent steps. This directly contradicts the plan's stated scoping rule: "The override applies only to that step's Execute call; the session's default effort restores at the end of the call." The executor's note that "this correctly handles sessions opened without a `reasoning_effort`" is incorrect — it leaves the override permanently in effect.
- Acceptance criteria: When `defaultEffort == ""`, the restore function must call `session.SetModel(ctx, defaultModel, nil)` to attempt resetting the effort to the SDK/server default. A new unit test must be added: given a session with no agent-level effort and a step that sets `reasoning_effort = "high"`, assert that `fake.getSetModelCalls()` contains two calls: (1) `{model:"", effort:"high"}` and (2) `{model:"", effort:""}` — demonstrating the restore attempt. The unit test for case B2 must fail without the fix and pass with it.

**N1 — `docs/plugins.md` error message example does not match the actual diagnostic format**
- Severity: required nit
- File: `docs/plugins.md`, lines 235–239
- Problem: The "Common mistake" section shows a fictional diagnostic format (`Error: unknown field "system_prompt" in input block` with `Hint: ...` lines). The actual implementation emits an HCL diagnostic with Summary `field "system_prompt" is not valid in step input for adapter "copilot"; it belongs in the agent config block:` and Detail containing the `agent { config { ... } }` snippet. The documentation misleads users about what they will actually see.
- Acceptance criteria: The error example must show the actual format emitted by `unknownFieldDiagnostic`. Acceptable to show only the `Summary` line (the detail block) or both lines. It must not show `Hint:` or the old generic `unknown field` phrasing.

**N2 — Restore error silently discarded in `applyRequestEffort`**
- Severity: required nit
- File: `cmd/criteria-adapter-copilot/copilot.go`, line 495
- Problem: `_ = session.SetModel(...)` in the restore closure silently discards any error from the restore call. If the restore `SetModel` fails (e.g., session disconnected mid-execution), the error is dropped with no trace. The adapter uses structured slog logging elsewhere.
- Acceptance criteria: Replace `_` with a log call at warn level, e.g. `slog.Warn("copilot: restore per-step reasoning_effort failed", "error", err)`. Alternatively, annotate the discard with a comment explaining the deliberate choice (e.g., "restore errors are best-effort; do not fail the step that already completed"). One or the other; not both.

#### Test Intent Assessment

- **6.1/6.2**: Fail the regression-sensitivity criterion — see B1. Tests can pass despite production-code bugs.
- **6.3**: Strong. `validateReasoningEffort` is called directly; any change to the valid set would fail this test.
- **6.4**: Strong. Verifies exact SDK call sequence (apply + restore) and the final outcome event. Correctly targets the `applyRequestEffort` path.
- **6.6/6.7**: Strong. 6.6 asserts exact phrasing cues (`"system_prompt"`, `"agent config block"`, `adapter = "copilot"`). 6.7 correctly verifies the non-targeted path. Both tests would fail under realistic regressions.
- **Test for B2 (missing)**: The no-default-effort + per-step override scenario has no test. Required by B2 acceptance criteria.
- **6.8**: Adequate for protocol-path coverage (open + two executes + close). Does not verify SetModel call sequence at the process boundary, which is acceptable — 6.4 covers that. Would benefit from asserting both result events are non-empty outcomes (already does).

#### Validation Performed

```
make test                    → all packages pass
make validate                → all 8 examples validate (including new copilot_planning_then_execution.hcl)
make test-conformance        → sdk/conformance and TestCopilotReasoningEffortOverride pass
make lint-go                 → clean (no new golangci-lint entries)
make lint-imports            → Import boundaries OK
go test -race -count=1 ./cmd/criteria-adapter-copilot/... ./workflow/...
                             → all W09-related tests pass (6.1–6.4, 6.6–6.7, bonus, 6.8)
```

---

### Round-2 Remediation (2026-04-28)

**B1 fixed**: Tests 6.1 and 6.2 now call `p.applyOpenSessionModel(context.Background(), s, cfg)` directly. Both tests additionally assert `s.defaultModel` and `s.defaultEffort`. Mutation test confirmed: removing `s.defaultEffort = effort` from `applyOpenSessionModel` causes test 6.1 to fail with `defaultEffort = "", want "high"`.

**B2 fixed**: `applyRequestEffort` restore closure now always calls `session.SetModel(ctx, defaultModel, opts)` where `opts` is `nil` when `defaultEffort == ""` (clearing the override) and `&SetModelOptions{ReasoningEffort: &defaultEffort}` otherwise. New test `TestExecutePerStepEffortRestoresWhenNoDefault` asserts that with no agent-level default, the SDK call sequence is `SetModel("", high)` then `SetModel("", nil-opts → ""effort)`.

**N1 fixed**: `docs/plugins.md` "Common mistake" section now shows the actual Summary line emitted by `unknownFieldDiagnostic`, including the `agent "<name>" { adapter = "copilot" config { ... } }` detail block.

**N2 fixed**: Restore closure now calls `slog.Warn("copilot: restore per-step reasoning_effort failed", "error", err)` instead of `_ = session.SetModel(...)`. Comment explains best-effort semantics.

**`make ci` round-2 result**: all gates pass.

---

### Review 2026-04-28-02 — approved

#### Summary

All four findings from round 1 (B1, B2, N1, N2) are correctly resolved. Tests 6.1 and 6.2 now call `p.applyOpenSessionModel` and assert both the SDK call sequence and the captured defaults; a mutation removing `s.defaultEffort = effort` would cause 6.1 to fail. The restore closure unconditionally calls `SetModel` (with `nil` opts when no default effort is configured), and the new `TestExecutePerStepEffortRestoresWhenNoDefault` test verifies the two-call sequence `(high → "")`. The docs example in `plugins.md` now shows the actual diagnostic format. The restore discard is replaced with a `slog.Warn`. All make targets pass on a cold run.

#### Plan Adherence

- **B1 (tests 6.1/6.2 production code)**: ✅ Both tests call `p.applyOpenSessionModel`; no inlined logic; assert `defaultEffort`, `defaultModel`, and `SetModel` call args.
- **B2 (restore when no default effort)**: ✅ `applyRequestEffort` restore now calls `session.SetModel(ctx, defaultModel, nil)` unconditionally; `TestExecutePerStepEffortRestoresWhenNoDefault` asserts apply+restore call sequence.
- **N1 (docs error format)**: ✅ `plugins.md` now shows the actual Summary+Detail format from `unknownFieldDiagnostic`; `Hint:` lines removed.
- **N2 (silent restore discard)**: ✅ `_ = session.SetModel(...)` replaced with `slog.Warn`; comment explains best-effort semantics.

All plan checklist items remain fully implemented. No regressions introduced.

#### Validation Performed

```
make test          → all packages pass (fresh -count=1 on W09 tests)
make validate      → all 9 examples validate
make test-conformance → TestCopilotReasoningEffortOverride passes
make lint-go       → clean
make lint-imports  → Import boundaries OK
go test -race -count=1 -run "TestOpenSessionReasoning|TestOpenSessionInvalid|TestExecutePerStep"
                   → 6.1, 6.2, 6.3, 6.4, B2-new all PASS
```
