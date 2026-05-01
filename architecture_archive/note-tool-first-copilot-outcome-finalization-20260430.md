# Tool-First Copilot Outcome Finalization (planned, not yet implemented)

> **Archived 2026-04-30 from `architecture_notes.md`.** This design has
> been promoted into Phase 2 workstreams
> [W14](../workstreams/14-copilot-tool-call-wire-contract.md) (wire
> contract — `pb.ExecuteRequest.AllowedOutcomes`) and
> [W15](../workstreams/15-copilot-submit-outcome-adapter.md) (Copilot
> `submit_outcome` adapter implementation, 3-attempt reprompt loop,
> removal of prose parsing). Treat this archive as the **source of
> truth for design intent and locked decisions**; the workstream files
> are the source of truth for the implementation contract.
>
> Replaces the cancelled W11 (host-side `outcome_aliases`) approach to
> UF#03; UF#03 is now satisfied at the source.

Working design notes for replacing the Copilot adapter's free-text outcome
parsing with a structured tool-call finalization. Captured here so the design
context is not lost between workstreams; no code on this has landed yet.

## Why

Today the Copilot adapter derives the step outcome by scanning the final
assistant message for a `result:` prefix in
[cmd/criteria-adapter-copilot/copilot_turn.go](../cmd/criteria-adapter-copilot/copilot_turn.go)
(see `parseOutcome`, default `needs_review`). This is brittle:

1. Models drift from the convention; outcomes silently become `needs_review`.
2. Allowed outcomes are not communicated to the model in any structured way —
   the engine validates the result against `StepNode.Outcomes` only after the
   adapter has already committed to a string (see
   [internal/engine/node_step.go](../internal/engine/node_step.go) around the
   "produced unmapped outcome" guard).
3. There is no explicit wire contract between the engine's compiled outcome
   set and the adapter — only HCL-side knowledge.

## Direction

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

## Plan: Tool-First Copilot Outcome Finalization

Move outcome selection from fragile free-text parsing to a structured
finalization tool call. The adapter registers an internal `submit_outcome`
tool **once at OpenSession** and finalizes from validated tool-call arguments
rather than from assistant prose. Per-step scoping is handled by the adapter
holding the active step's allowed outcomes on `sessionState` and validating
in the tool handler at call time.

### Phase 1 — Wire contract for allowed outcomes

> Implemented in [W14](../workstreams/14-copilot-tool-call-wire-contract.md).

1. Extend `ExecuteRequest` in
   [proto/criteria/v1/adapter_plugin.proto](../proto/criteria/v1/adapter_plugin.proto)
   with a `repeated string allowed_outcomes` field.
2. Regenerate Go bindings via `make proto` (this is a breaking SDK change per
   [CONTRIBUTING.md](../CONTRIBUTING.md) bump policy — bump accordingly).
3. Populate `allowed_outcomes` deterministically from `StepNode.Outcomes` map
   keys, sorted, when the host issues `Execute` in
   [internal/plugin/loader.go](../internal/plugin/loader.go) (`rpcPlugin.Execute`,
   currently around L204 where it builds `ExecuteRequest`).
4. Engine continues to enforce the unmapped-outcome guard in
   [internal/engine/node_step.go](../internal/engine/node_step.go) as
   defense-in-depth.

### Phase 2 — Per-step `submit_outcome` semantics with one-time tool registration

> Implemented in [W15](../workstreams/15-copilot-submit-outcome-adapter.md).

1. Define a typed parameter struct with `Outcome string` (required) and
   `Reason string` (optional). The schema **does not** encode an enum for
   `Outcome` — Go SDK v0.3.0 has no public live-tool mutation, and refreshing
   the enum would require `ResumeSessionWithOptions` per step, which violates
   the no-recreate constraint.
2. Register `submit_outcome` exactly once at `OpenSession` via
   `SessionConfig.Tools` in
   [cmd/criteria-adapter-copilot/copilot_session.go](../cmd/criteria-adapter-copilot/copilot_session.go)
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

### Phase 3 — Finalize from tool-call result, with adapter-level reprompt up to 3 attempts

> Implemented in [W15](../workstreams/15-copilot-submit-outcome-adapter.md).

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

### Phase 4 — Tests and conformance

> Implemented in [W15](../workstreams/15-copilot-submit-outcome-adapter.md).

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

### Phase 5 — Docs and rollout

> Implemented across W14 (`docs/plugins.md` field doc), W15 (`docs/plugins.md`
> outcome-finalization section), and W16 (CHANGELOG entry).

1. Document the `submit_outcome` contract, per-step scope, permission-skip
   behavior, and the 3-attempt reprompt policy in
   [docs/plugins.md](../docs/plugins.md).
2. Document the removal/deprecation of `result:` prose parsing and the
   strict `failure` policy when reprompts are exhausted.
3. Note in [CHANGELOG.md](../CHANGELOG.md) that this is a breaking SDK change
   (proto field on `ExecuteRequest`) and that downstream orchestrators must
   forward `allowed_outcomes` per step.

## Decisions (locked)

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

## Open questions / further considerations

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

## PR sizing

Estimated total ~750–900 LOC across proto, plugin loader, adapter session/turn
code, fake Copilot fixture, adapter unit tests, transport tests, conformance,
and docs. Recommended split:

1. **PR-A (small, mechanical):** Proto field + regen + loader population +
   transport test. No behavior change in the adapter yet. → [W14](../workstreams/14-copilot-tool-call-wire-contract.md).
2. **PR-B (behavior + tests):** Register `submit_outcome`, per-step state,
   handler, 3-attempt reprompt, remove prose parsing, fake harness, full unit
   + conformance matrix, docs, CHANGELOG. → [W15](../workstreams/15-copilot-submit-outcome-adapter.md).

If shipping as a single PR, structure commits by phase so review can proceed
phase-by-phase.

## Relevant files

1. [cmd/criteria-adapter-copilot/copilot_session.go](../cmd/criteria-adapter-copilot/copilot_session.go)
   — capability insertion point for session tool registration.
2. [cmd/criteria-adapter-copilot/copilot_turn.go](../cmd/criteria-adapter-copilot/copilot_turn.go)
   — finalization acceptance logic (tool-first or strict fallback).
3. [proto/criteria/v1/adapter_plugin.proto](../proto/criteria/v1/adapter_plugin.proto)
   — `allowed_outcomes` contract extension.
4. [internal/plugin/loader.go](../internal/plugin/loader.go) — populate
   `Execute` request with `allowed_outcomes` from step outcomes.
5. [internal/engine/node_step.go](../internal/engine/node_step.go) —
   defense-in-depth unmapped-outcome guard (unchanged).
6. [docs/plugins.md](../docs/plugins.md) — behavior docs for finalization
   contract.
7. [CHANGELOG.md](../CHANGELOG.md) — release notes for behavior/contract change.
