# Workstream 15 — Copilot `submit_outcome` adapter (tool-call finalization)

**Owner:** Workstream executor ·
**Depends on:** [W14](14-copilot-tool-call-wire-contract.md)
(consumes the new `AllowedOutcomes` wire field).
**Coordinates with:** [W12](12-lifecycle-log-clarity.md)
(both touch adapter session lifecycle paths — schedule the merge order
to avoid conflicts; W12 already merged, so this workstream inherits
its `OnAdapterLifecycle` plumbing).

## Context

Today the Copilot adapter derives a step's outcome by string-matching a
`result:` prefix in the model's final assistant message
([cmd/criteria-adapter-copilot/copilot_turn.go:223](../cmd/criteria-adapter-copilot/copilot_turn.go#L223)
— `parseOutcome`). On a missing or empty `result:` line it returns the
literal string `"needs_review"`. This is brittle:

1. Models drift from the convention; outcomes silently become
   `needs_review`.
2. The host's
   [StepNode.Outcomes](../workflow/schema.go#L284) set is never
   communicated to the model in any structured way.
3. There is no explicit wire contract between the engine's compiled
   outcome set and the adapter — only HCL-side knowledge.

[W14](14-copilot-tool-call-wire-contract.md) ships the wire contract
(`pb.ExecuteRequest.AllowedOutcomes`). This workstream — **Phase B** —
ships the Copilot adapter's consumer of that contract: a structured
`submit_outcome` tool call replaces prose parsing; an explicit
3-attempt reprompt loop handles model drift; missing or invalid
finalization returns `failure`, not `needs_review`.

The full design is in `architecture_archive/note-tool-first-copilot-outcome-finalization-20260430.md`
(originally captured in `architecture_notes.md`'s "Tool-First Copilot
Outcome Finalization" section). Read that file end-to-end before
starting; it covers SDK constraints (no public live-tool mutation in
`copilot-sdk/go v0.3.0`), why per-step state-driven validation is the
chosen model, and the locked design decisions.

## Prerequisites

- [W14](14-copilot-tool-call-wire-contract.md) merged on `main`
  (`pb.ExecuteRequest.AllowedOutcomes` is populated by the host).
- `make ci` green on `main`.
- `github.com/github/copilot-sdk/go v0.3.0` already pinned in
  [go.mod](../go.mod) (line 9 at time of writing). Verify before
  starting; if the version differs, audit the SDK API surface for
  `SessionConfig.Tools`, `copilot.DefineTool`, `Tool.SkipPermission`
  before proceeding.
- Familiarity with:
  - [cmd/criteria-adapter-copilot/copilot_session.go](../cmd/criteria-adapter-copilot/copilot_session.go)
    (`buildSessionConfig` at line 110, `sessionState` struct at
    line 57).
  - [cmd/criteria-adapter-copilot/copilot_turn.go](../cmd/criteria-adapter-copilot/copilot_turn.go)
    (`turnState` at line 20, `awaitOutcome` at line 120,
    `Execute` at line 142, `parseOutcome` at line 223).
  - [cmd/criteria-adapter-copilot/copilot.go](../cmd/criteria-adapter-copilot/copilot.go)
    (constants at lines 44–54, `resultPrefix` constant at line 53).
  - [cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main.go](../cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main.go)
    (the fixture used by E2E tests).
- Read the architecture archive note (see "Context" above). The
  "Decisions (locked)" list there is binding.

## Locked design decisions (from the archive note)

These are **not negotiable** for this workstream:

1. Tool-call finalization replaces prose parsing; do **not** keep the
   prose path as a silent fallback.
2. Enforcement is strict: invalid finalization after reprompts returns
   `failure`, not `needs_review`.
3. Tool registration is **per session, once** with per-step
   state-driven validation. Do **not** recreate the session per step.
   Do **not** call `ResumeSessionWithOptions` per step.
4. `submit_outcome` is registered with `SkipPermission = true` so the
   internal tool never prompts the user.
5. The 3-attempt reprompt logic lives in the adapter, not the engine.
6. The engine's unmapped-outcome guard
   ([internal/engine/node_step.go:340-342](../internal/engine/node_step.go#L340))
   stays as defense-in-depth; do not modify it.

If a constraint surfaces during implementation that conflicts with
these decisions, stop and escalate in reviewer notes — do not relax
them silently.

## In scope

### Step 1 — Per-session `submit_outcome` tool registration

Edit
[cmd/criteria-adapter-copilot/copilot_session.go](../cmd/criteria-adapter-copilot/copilot_session.go)
`buildSessionConfig` (line 110).

#### Step 1.1 — Define the tool parameter shape

Define a typed parameter struct in a new helper file
`cmd/criteria-adapter-copilot/copilot_outcome.go` (the file may live
alongside `copilot_turn.go`; do not bloat `copilot_turn.go`):

```go
package main

// SubmitOutcomeArgs is the typed parameter struct for the
// `submit_outcome` tool. The schema deliberately does NOT encode an
// enum for Outcome — the Copilot Go SDK v0.3.0 has no public live
// tool-mutation API, and refreshing the enum would require
// ResumeSessionWithOptions per step, which the design explicitly
// rejects. Validation runs in the tool handler against the active
// step's allowed_outcomes set carried on sessionState.
type SubmitOutcomeArgs struct {
    Outcome string `json:"outcome"`           // required; must be a member of the active allowed set
    Reason  string `json:"reason,omitempty"`  // optional; surfaced in events for operator visibility
}
```

Hard requirements:

- `Outcome` is required (the handler rejects empty strings).
- `Reason` is optional. Treat it as a free-form string; do not
  truncate or validate beyond presence.
- Schema is **not** enum-typed. Document the reason in a code comment
  exactly per the architecture archive note's Phase 2 §1.

#### Step 1.2 — Register the tool once per session

In `buildSessionConfig`, append a `Tools` entry to the
`copilot.SessionConfig`:

```go
sc := &copilot.SessionConfig{
    Streaming: true,
    Model:     cfg["model"],
    OnPermissionRequest: func(r copilot.PermissionRequest, _ copilot.PermissionInvocation) (copilot.PermissionRequestResult, error) {
        return p.handlePermissionRequest(pluginSessionID, &r)
    },
    Tools: []copilot.Tool{
        copilot.DefineTool(copilot.ToolDefinition[SubmitOutcomeArgs]{
            Name:           submitOutcomeToolName,
            Description:    submitOutcomeToolDescription,
            SkipPermission: true,
            Handler: func(ctx context.Context, args SubmitOutcomeArgs) (copilot.ToolResult, error) {
                return p.handleSubmitOutcome(pluginSessionID, args)
            },
        }),
    },
}
```

Hard requirements:

- `submitOutcomeToolName` constant value: `"submit_outcome"`. Place
  it in
  [copilot.go](../cmd/criteria-adapter-copilot/copilot.go) alongside
  `resultPrefix`.
- `submitOutcomeToolDescription` constant value (final wording is the
  executor's call, but it must convey the contract):

  > `Finalize the outcome for the current step. Call this exactly once with one of the allowed outcomes for the step. The list of allowed outcomes is provided in the user prompt. Failure to call this tool with a valid outcome will fail the step.`

- `SkipPermission: true` is required (locked decision §4).
- Handler signature uses the SDK's typed-tool generic; verify the
  exact API in `copilot-sdk/go v0.3.0` before writing the call. The
  pseudo-code above mirrors the archive note's Phase 2 §2 — adjust
  only to match the actual SDK signature.
- `p.handleSubmitOutcome` is implemented in Step 2.
- The exact `copilot.Tool` / `copilot.DefineTool` / `copilot.ToolResult`
  type names depend on the SDK; locate them via a quick read of the
  vendored SDK or `go doc github.com/github/copilot-sdk/go`.

### Step 2 — Per-step state and tool handler

Edit
[cmd/criteria-adapter-copilot/copilot_session.go](../cmd/criteria-adapter-copilot/copilot_session.go)
`sessionState` struct (line 57).

#### Step 2.1 — Extend `sessionState` with per-execute outcome state

Add three fields to `sessionState` (mu-guarded, alongside the existing
mu-guarded `pending`/`active`/`activeCh`/`sink`/`permissionDeny`):

```go
type sessionState struct {
    // ... existing fields ...

    // submit_outcome per-execute state (mu-guarded). Reset at every
    // beginExecution call. activeAllowedOutcomes is the set the host
    // declared via ExecuteRequest.AllowedOutcomes for the current
    // step; finalizedOutcome captures a successful tool call;
    // finalizeAttempts counts invocations (valid + invalid) for the
    // 3-attempt cap.
    activeAllowedOutcomes map[string]struct{}
    finalizedOutcome      string
    finalizedReason       string
    finalizeAttempts      int
}
```

Hard requirements:

- All three fields are mu-guarded. Locking discipline matches the
  existing `pending` / `active` fields in the same struct.
- `activeAllowedOutcomes` is a `map[string]struct{}` for O(1) lookup
  in the hot path; do not use `[]string`.
- A new `*sessionState` zero-value already has empty/zero values for
  all three; do not pre-allocate.

#### Step 2.2 — Reset state at `beginExecution`

Edit `beginExecution` (line 201 of `copilot_turn.go`) to also reset the
finalize fields:

```go
func (s *sessionState) beginExecution(sink pluginhost.ExecuteEventSender) func() {
    execDone := make(chan struct{})
    s.mu.Lock()
    s.active = true
    s.activeCh = execDone
    s.sink = sink
    s.permissionDeny = false

    // W15: reset per-execute finalize state.
    s.finalizedOutcome = ""
    s.finalizedReason = ""
    s.finalizeAttempts = 0
    // activeAllowedOutcomes is set by Execute *before* the prompt is
    // sent; do not reset it here (Execute populates it after this
    // helper returns).

    s.mu.Unlock()
    return func() {
        // ... existing cleanup ...
    }
}
```

#### Step 2.3 — Populate `activeAllowedOutcomes` from `ExecuteRequest`

Edit `Execute` (line 142 of `copilot_turn.go`). After
`beginExecution` returns and before the prompt is sent, build the
allowed set from `req.GetAllowedOutcomes()`:

```go
allowed := req.GetAllowedOutcomes()
s.mu.Lock()
s.activeAllowedOutcomes = make(map[string]struct{}, len(allowed))
for _, name := range allowed {
    s.activeAllowedOutcomes[name] = struct{}{}
}
s.mu.Unlock()
```

Hard requirements:

- The set is populated **before** the prompt is sent (the model may
  call the tool on its very first turn).
- An empty `AllowedOutcomes` slice yields an empty set; the handler
  treats every call as invalid in that case (defensive — no step
  should arrive with an empty set, but do not crash if it does).
- Do not log the allowed set at info level on every Execute; it is
  surfaced through the prompt (Step 3.1) and the error path.

#### Step 2.4 — Tool handler

Implement `handleSubmitOutcome` in
`cmd/criteria-adapter-copilot/copilot_outcome.go`:

```go
func (p *copilotPlugin) handleSubmitOutcome(pluginSessionID string, args SubmitOutcomeArgs) (copilot.ToolResult, error) {
    s := p.getSession(pluginSessionID)
    if s == nil {
        // Unknown session — surface as a tool error so the model can see it.
        return submitOutcomeError("unknown session"), nil
    }

    s.mu.Lock()
    s.finalizeAttempts++
    outcome := strings.TrimSpace(args.Outcome)
    if outcome == "" {
        s.mu.Unlock()
        return submitOutcomeError("outcome is required"), nil
    }
    if _, ok := s.activeAllowedOutcomes[outcome]; !ok {
        allowedList := sortedAllowedOutcomes(s.activeAllowedOutcomes)
        s.mu.Unlock()
        return submitOutcomeError(fmt.Sprintf(
            "outcome %q is not in the allowed set %v; choose one of: %s",
            outcome, allowedList, strings.Join(allowedList, ", "),
        )), nil
    }
    if s.finalizedOutcome != "" {
        // Duplicate finalize: the model called us twice in one turn.
        // Keep the FIRST valid outcome (do not overwrite); flag the
        // duplicate via reprompt diagnostics on the next attempt.
        existing := s.finalizedOutcome
        s.mu.Unlock()
        return submitOutcomeError(fmt.Sprintf(
            "outcome already finalized as %q in this turn; do not call submit_outcome again",
            existing,
        )), nil
    }
    s.finalizedOutcome = outcome
    s.finalizedReason = strings.TrimSpace(args.Reason)
    s.mu.Unlock()

    // Forward an adapter event so operators see the finalize call in
    // the event stream. Use the active sink captured in beginExecution.
    s.mu.Lock()
    sink := s.sink
    s.mu.Unlock()
    if sink != nil {
        _ = sink.Send(adapterEvent("outcome.finalized", map[string]any{
            "outcome": outcome,
            "reason":  args.Reason,
        }))
    }

    return submitOutcomeSuccess(outcome), nil
}
```

Helpers (same file):

```go
// submitOutcomeSuccess returns the SDK ToolResult representing a
// successful finalize. The exact ToolResult shape depends on the SDK;
// adapt to v0.3.0.
func submitOutcomeSuccess(outcome string) copilot.ToolResult { /* ... */ }

// submitOutcomeError returns the SDK ToolResult representing a
// recoverable tool error that nudges the model toward the allowed set
// without ending the turn.
func submitOutcomeError(msg string) copilot.ToolResult { /* ... */ }

// sortedAllowedOutcomes returns the active allowed-outcomes set as a
// sorted slice for deterministic error messages.
func sortedAllowedOutcomes(set map[string]struct{}) []string {
    out := make([]string, 0, len(set))
    for k := range set {
        out = append(out, k)
    }
    sort.Strings(out)
    return out
}
```

Hard requirements:

- Tool errors return `(ToolResult, nil)` not `(nil, error)` — see
  the architecture archive note Phase 2 §4 ("return a tool-error
  ToolResultObject … so the model can retry within the same turn").
  Returning a Go error from the handler ends the turn unrecoverably.
- The handler is goroutine-safe (the SDK invokes handlers from its
  own goroutines). Hold `s.mu` for every read/write of finalize
  state.
- First-write-wins on duplicate calls: do not overwrite
  `finalizedOutcome`. The reprompt path (Step 3) treats the first
  valid call as authoritative.
- Always increment `finalizeAttempts`, including on invalid calls,
  so the 3-attempt cap (Step 3) sees every attempt.

### Step 3 — Reprompt loop and finalization

Edit `awaitOutcome` (line 120 of `copilot_turn.go`) and the surrounding
turn-state machinery.

#### Step 3.1 — Inject allowed-outcomes context into the prompt

Modify `Execute` (or `prepareExecute`) to prepend a structured
allowed-outcomes preamble to the model's prompt. Wording:

```
You must finalize the outcome for this step by calling the
`submit_outcome` tool exactly once before ending the turn. The
allowed outcomes are: <comma-separated list>. If you do not call
the tool with a valid outcome, the step will fail.

<original prompt>
```

Hard requirements:

- The preamble is **always** prepended; do not gate on the model
  identity.
- The list of allowed outcomes is taken from
  `req.GetAllowedOutcomes()` (already sorted by W14's host helper).
- The preamble must not be sent if `req.GetAllowedOutcomes()` is
  empty — fall back to the original prompt and rely on the
  `submitOutcomeError` path to fail the step. (No step should
  arrive with an empty set, but be defensive.)

#### Step 3.2 — 3-attempt finalize loop

Replace the `awaitOutcome` body (line 120) with a loop:

```go
const maxFinalizeAttempts = 3

func (ts *turnState) awaitOutcome(ctx context.Context, s *sessionState, sink pluginhost.ExecuteEventSender) error {
    for attempt := 1; attempt <= maxFinalizeAttempts; attempt++ {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case err := <-ts.errCh:
            if errors.Is(err, errMaxTurnsReached) {
                return ts.handleMaxTurnsReached(s, sink)
            }
            return err
        case <-ts.turnDone:
            // Inspect finalize state.
            s.mu.Lock()
            denied := s.permissionDeny
            outcome := s.finalizedOutcome
            s.mu.Unlock()

            if denied {
                return sink.Send(resultEvent("failure"))
            }
            if outcome != "" {
                return sink.Send(resultEvent(outcome))
            }

            // No valid finalize this turn. If we have attempts left,
            // reprompt; otherwise return failure.
            if attempt == maxFinalizeAttempts {
                return ts.failExhausted(s, sink)
            }
            if err := ts.reprompt(ctx, s); err != nil {
                return err
            }
            // Loop and wait for the next SessionIdle.
        }
    }
    return ts.failExhausted(s, sink)
}
```

Where:

- `ts.reprompt(ctx, s)` sends a corrective `copilot.MessageOptions`
  with the wording from the architecture note Phase 3 §3:

  > "You must call the `submit_outcome` tool with one of the allowed
  > outcomes: \<sorted list\>. Do not return a final answer without
  > calling the tool. Allowed outcomes: \<list\>. Failure to call the
  > tool will fail the step."

- `ts.failExhausted(s, sink)` emits a structured adapter event with
  the failure reason (missing call vs. invalid enum vs. duplicate
  calls — derived from `s.finalizeAttempts` and the recorded state),
  then sends `resultEvent("failure")`.
- `ts.handleMaxTurnsReached(s, sink)` mirrors the existing
  `errMaxTurnsReached` path **but** returns `failure` rather than
  `needs_review`, **unless** `needs_review` is in the allowed set —
  in which case it preserves the historical "max-turns becomes
  needs_review" behavior. (Architecture archive note Phase 3 §4.)

Hard requirements:

- The constant `maxFinalizeAttempts = 3` includes the initial attempt
  (1 initial + 2 reprompts).
- Reprompt sends a *new* `MessageOptions` to the active SDK session;
  do not recreate the session.
- `permissionDeny` continues to terminate immediately at `failure`
  (it already did, modulo the wording change from `needs_review` to
  `failure` per locked decision §2).
- Each reprompt counts toward `max_turns`. Do not bypass the
  existing `errMaxTurnsReached` path.
- The single-success path (model calls `submit_outcome` validly on
  the first attempt) must not pay any extra latency — the loop
  short-circuits on `outcome != ""` after the first `turnDone`.

#### Step 3.3 — Remove prose parsing

Delete `parseOutcome` (line 223 of `copilot_turn.go`) and the
`resultPrefix` constant
([copilot.go:53](../cmd/criteria-adapter-copilot/copilot.go#L53)).

Update the package-level docstring in
[copilot.go](../cmd/criteria-adapter-copilot/copilot.go) (lines
17–20) to describe the new outcome semantics:

```go
// Outcome semantics:
//   - the plugin registers a `submit_outcome` tool at OpenSession.
//   - per Execute, the host's allowed outcomes are loaded onto
//     sessionState before the prompt is sent.
//   - the model MUST call submit_outcome exactly once with a valid
//     outcome; the adapter forwards that value via ExecuteResult.
//   - on missing / invalid finalize, the adapter reprompts up to 2
//     additional times. After 3 failed attempts the adapter returns
//     "failure" with a structured diagnostic event.
//   - permission denial returns "failure".
```

Hard requirements:

- `parseOutcome` is fully removed; no silent fallback per locked
  decision §1.
- `resultPrefix` is removed.
- Search the tree for any other reference to `resultPrefix` or
  `parseOutcome` (tests, docs, fixtures) and update accordingly.

### Step 4 — Update the fake-Copilot fixture

Edit
[cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main.go](../cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main.go).

The fixture today emits assistant messages and lets the adapter parse
them. The new contract requires it to emit *tool calls* to
`submit_outcome` (or deliberately misbehave to exercise reprompt
paths).

Add a small scenario-driven harness. The fixture reads a
**`FAKE_COPILOT_SCENARIO`** env var (or equivalent — pick the
ergonomically lightest knob the existing fixture already uses) and
emits one of:

- `success` — emits one `submit_outcome` tool call with a valid
  outcome on the first turn, then `SessionIdle`.
- `success-after-reprompt-1` — emits a non-call assistant message,
  then `SessionIdle`; on the next prompt, emits a valid
  `submit_outcome`.
- `success-after-reprompt-2` — same, but recovers on the third
  attempt.
- `invalid-outcome` — emits one `submit_outcome` with an outcome not
  in the allowed set, then `SessionIdle`. The handler returns a
  tool-error; verify the model can retry within the same turn (per
  the SDK semantics — see the archive note Phase 2 §4).
- `duplicate-call` — emits two `submit_outcome` calls in the same
  turn (first valid, second valid-but-different). Adapter must keep
  the first.
- `missing` — emits a non-call assistant message and `SessionIdle`
  on every prompt; adapter must exhaust attempts and return
  `failure`.

Hard requirements:

- The fixture must remain a single binary; do not split it.
- The scenario knob is environment-driven (the existing fixture
  pattern). Document scenarios in a top-of-file comment.
- The fixture must not regress the existing scenarios used by other
  tests (audit `copilot_internal_test.go` and `conformance_test.go`
  before refactoring).

### Step 5 — Tests

#### Step 5.1 — Adapter unit tests

Add to
[cmd/criteria-adapter-copilot/copilot_internal_test.go](../cmd/criteria-adapter-copilot/copilot_internal_test.go)
(or a new sibling `copilot_outcome_test.go` if that file is
already large; check before splitting):

| Test | Scenario | Assertion |
|------|----------|-----------|
| `TestSubmitOutcome_HappyPath` | fixture `success`, allowed = `{approved, changes_requested, failure}` | `ExecuteResult.Outcome == "approved"`; one `outcome.finalized` adapter event |
| `TestSubmitOutcome_RepromptOnce` | fixture `success-after-reprompt-1` | `Outcome == "approved"`; exactly one reprompt sent (assert via fixture's record-of-prompts-received) |
| `TestSubmitOutcome_RepromptTwice` | fixture `success-after-reprompt-2` | `Outcome == "approved"`; exactly two reprompts sent |
| `TestSubmitOutcome_ExhaustedFailure` | fixture `missing` | `Outcome == "failure"`; structured failure event with reason `"missing finalize"` |
| `TestSubmitOutcome_InvalidEnumThenSuccess` | fixture `invalid-outcome` followed by valid in next turn | `Outcome == "approved"`; adapter event records the invalid attempt |
| `TestSubmitOutcome_DuplicateKeepsFirst` | fixture `duplicate-call` | `Outcome` equals the FIRST valid call; second call's outcome is discarded; tool-error returned for the second call |
| `TestSubmitOutcome_PermissionDeniedFailure` | denial via existing permission path during finalize | `Outcome == "failure"` (changed from prior `needs_review`) |
| `TestSubmitOutcome_MaxTurnsReached_NoNeedsReviewInAllowed` | allowed = `{approved, failure}`, reach `max_turns` | `Outcome == "failure"` |
| `TestSubmitOutcome_MaxTurnsReached_NeedsReviewInAllowed` | allowed = `{approved, needs_review, failure}`, reach `max_turns` | `Outcome == "needs_review"` (preserves historical behavior when the workflow author wants it) |
| `TestSubmitOutcome_EmptyAllowedSetFailsClosed` | allowed = `[]` (defensive case) | adapter returns `failure` on first turn; no panic |
| `TestSubmitOutcome_PreamblePresentInPrompt` | inspect prompt sent to the SDK session | preamble substring `"allowed outcomes are: approved, changes_requested, failure"` is present |

Hard requirements:

- Each test is independent (no shared session across tests; spin up a
  fresh fixture per test where needed).
- Race-safe: run with `-race`.
- The duplicate-call test must verify *both* that the first outcome
  wins *and* that the second call returns a tool-error visible to
  the fixture.

#### Step 5.2 — Transport / conformance test

Extend
[cmd/criteria-adapter-copilot/conformance_test.go](../cmd/criteria-adapter-copilot/conformance_test.go):

- Add `TestConformance_AllowedOutcomesPropagation` — assert the
  fixture sees `AllowedOutcomes` populated on the inbound
  `ExecuteRequest` for each step (this is partially covered by W14's
  loader test, but the conformance lane verifies the whole pipe end
  to end).

#### Step 5.3 — Engine guard regression

Add to `internal/engine/engine_test.go` (or whichever file holds the
unmapped-outcome regression):

- `TestEngine_GuardRemainsForCopilotAdapterFailure` — even with W15
  in place, an adapter that returns an outcome not in the step's
  declared set still fails via the engine guard at
  [internal/engine/node_step.go:340-342](../internal/engine/node_step.go#L340).
  This ensures the adapter and engine validate independently
  (defense-in-depth per locked decision §6).

#### Step 5.4 — Existing tests must remain green

- Every existing test in `cmd/criteria-adapter-copilot/...` must
  pass without regression. Tests that asserted on prose-parsed
  outcomes need to be migrated to the tool-call fixture path.
- `make test-conformance` green.
- `make ci` green.

### Step 6 — Documentation

Update
[docs/plugins.md](../docs/plugins.md):

- Add an "Outcome finalization (Copilot adapter)" section documenting:
  - The `submit_outcome` tool: name, description, parameter shape,
    `SkipPermission` behavior.
  - Per-step scope semantics (validated against
    `ExecuteRequest.AllowedOutcomes`).
  - The 3-attempt reprompt policy (initial + 2 reprompts; failure
    after exhaustion).
  - The strict-failure policy: invalid finalization returns
    `failure`, not `needs_review`.
  - Permission-denied behavior: returns `failure`.
  - The max-turns interaction: returns `failure` unless
    `needs_review` is in the allowed set, in which case it preserves
    the historical mapping.
  - The structured failure-event payload (so operators can alert on
    it).
- Remove or supersede the prior `result:` prose-parsing
  documentation. If the section was titled "Outcome semantics" or
  similar, replace it; do not leave both descriptions live.
- Cross-reference [W14](14-copilot-tool-call-wire-contract.md) for
  the wire contract.

Provide CHANGELOG text in **reviewer notes** for
[W16](16-phase2-cleanup-gate.md) to copy:

> **Behavior change — Copilot outcome finalization:** The Copilot
> adapter now finalizes step outcomes via a structured
> `submit_outcome` tool call instead of parsing a `result:` prefix
> from the model's final assistant message. Workflows where the model
> previously emitted `result: <outcome>` prose continue to work only
> if the model also calls `submit_outcome`; the prose path has been
> removed. Failed finalization (missing call, invalid outcome,
> exhausted reprompts) now returns `failure` rather than the prior
> default of `needs_review`. Permission denial during a step also
> returns `failure`. Workflows that relied on the prior
> `needs_review` default must declare `failure` in their step's
> outcome set.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream
file.

## Behavior change

**Yes — observable, with a deprecation removal.**

- Copilot adapter outcome finalization changes from prose parsing to
  structured tool call.
- Default fallback outcome on missing/invalid finalize changes from
  `needs_review` to `failure` (locked decision §2).
- Permission-denied-during-step changes from `needs_review` to
  `failure`.
- Max-turns-reached changes from unconditional `needs_review` to
  conditional: `failure` unless `needs_review` is in the allowed set.
- New adapter event: `outcome.finalized` with `outcome` and `reason`.
- New structured failure event on exhausted reprompts.
- The `result:` prose-parsing path is **removed** entirely (no silent
  fallback per locked decision §1).
- Every Copilot Execute now sends an extended prompt preamble
  describing the allowed outcomes and the tool requirement.
- No HCL surface change. No engine semantics change. No CLI flag
  change. The proto change shipped in W14.

## Reuse

- [W14](14-copilot-tool-call-wire-contract.md)'s
  `pb.ExecuteRequest.AllowedOutcomes` field — this workstream is its
  first consumer.
- Existing `sessionState` struct, `mu` discipline, `pending`/`active`
  pattern.
- Existing `beginExecution` cleanup pattern.
- Existing `adapterEvent`/`logEvent`/`resultEvent` helpers in
  [copilot_util.go](../cmd/criteria-adapter-copilot/copilot_util.go).
- Existing fake-Copilot fixture; do not replace, extend.
- Existing W12 `OnAdapterLifecycle` plumbing — do not duplicate
  lifecycle reporting.
- The engine guard at
  [internal/engine/node_step.go:340-342](../internal/engine/node_step.go#L340)
  — do not reimplement validation in the engine.

## Out of scope

- Live tool mutation per step (would require
  `ResumeSessionWithOptions` per step). Locked decision §3 forbids
  this.
- Migrating other adapters (`shell`, `mcp`, `noop`) to a tool-call
  finalization model. Scope is Copilot only.
- Adding `confidence` or other structured metadata to
  `submit_outcome` beyond `outcome` and `reason` (architecture
  archive note open question §1; deferred).
- Filing the upstream SDK enhancement request for a public
  `Session.SetTools` API (archive open question §2; deferred).
- Removing the engine's unmapped-outcome guard (locked decision §6).
- Modifying `ExecuteRequest` further (W14 owns the wire contract).
- Verbose output mode (UF#07; Phase 3).
- Changing iteration / for_each outcome shaping
  (`all_succeeded` / `any_failed`). Iteration cursor outcomes are not
  finalized via `submit_outcome`; document this exclusion in
  `docs/plugins.md`.

## Files this workstream may modify

- `cmd/criteria-adapter-copilot/copilot.go` — constants, package
  docstring, remove `resultPrefix`.
- `cmd/criteria-adapter-copilot/copilot_session.go` —
  `sessionState` struct, `buildSessionConfig` tool registration.
- `cmd/criteria-adapter-copilot/copilot_turn.go` — `Execute`
  populates allowed set + prompt preamble; `awaitOutcome` reprompt
  loop; remove `parseOutcome`.
- `cmd/criteria-adapter-copilot/copilot_outcome.go` (new) — tool
  parameter struct, handler, helpers.
- `cmd/criteria-adapter-copilot/copilot_internal_test.go` — adapter
  unit tests per Step 5.1.
- `cmd/criteria-adapter-copilot/copilot_outcome_test.go` (new, if
  size warrants) — adapter unit tests for the handler.
- `cmd/criteria-adapter-copilot/conformance_test.go` — extension per
  Step 5.2.
- `cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main.go` —
  scenario harness per Step 4.
- `cmd/criteria-adapter-copilot/testfixtures/fake-copilot/main_test.go`
  — fixture self-tests if any.
- `internal/engine/engine_test.go` (or wherever the engine
  unmapped-outcome regression lives) — Step 5.3 regression.
- `docs/plugins.md` — outcome finalization documentation.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, top-level `CHANGELOG.md`,
  `workstreams/README.md`, or any other workstream file.
- `proto/criteria/v1/adapter_plugin.proto` or any `.pb.go` — the
  wire change shipped in W14.
- `internal/engine/node_step.go` — the unmapped-outcome guard stays
  exactly as-is (locked decision §6).
- `internal/plugin/loader.go` — the host already populates
  `AllowedOutcomes` per W14.
- Any other adapter under `cmd/criteria-adapter-*/`.

## Tasks

- [ ] Verify `github.com/github/copilot-sdk/go v0.3.0` is current in
      `go.mod`; audit `SessionConfig.Tools` /
      `copilot.DefineTool` / `Tool.SkipPermission` /
      `copilot.ToolResult` API surface.
- [ ] Add `submitOutcomeToolName` and tool-description constants to
      `copilot.go`. Remove `resultPrefix`.
- [ ] Define `SubmitOutcomeArgs` and the handler / helpers in
      `copilot_outcome.go`.
- [ ] Register `submit_outcome` in `buildSessionConfig` with
      `SkipPermission = true`.
- [ ] Extend `sessionState` with `activeAllowedOutcomes`,
      `finalizedOutcome`, `finalizedReason`, `finalizeAttempts`.
- [ ] Reset finalize state in `beginExecution`; populate
      `activeAllowedOutcomes` in `Execute` before the prompt is sent.
- [ ] Prepend the allowed-outcomes preamble to the model prompt.
- [ ] Replace `awaitOutcome` body with the 3-attempt reprompt loop;
      remove `parseOutcome`.
- [ ] Update the `errMaxTurnsReached` path to return `failure`
      unless `needs_review` is in the allowed set.
- [ ] Update the permission-denied path to return `failure`.
- [ ] Update the package-level docstring in `copilot.go` per
      Step 3.3.
- [ ] Extend the fake-Copilot fixture with the scenarios in Step 4.
- [ ] Add adapter unit tests per Step 5.1.
- [ ] Add the conformance propagation test per Step 5.2.
- [ ] Add the engine-guard regression test per Step 5.3.
- [ ] Update `docs/plugins.md` per Step 6.
- [ ] Capture the CHANGELOG text in reviewer notes for W16.
- [ ] `make build`, `make plugins`, `make test`,
      `make test-conformance`, `make lint-go`, `make ci` all green.

## Exit criteria

- `submit_outcome` is registered exactly once per session, at
  `OpenSession`, with `SkipPermission = true`.
- Per-step `activeAllowedOutcomes` is populated from
  `ExecuteRequest.AllowedOutcomes` before the prompt is sent.
- The model prompt always includes the allowed-outcomes preamble.
- Valid `submit_outcome` calls finalize the step; invalid calls
  return tool-errors and increment the attempt counter without
  ending the turn.
- The 3-attempt reprompt loop succeeds on attempts 1, 2, or 3 and
  exhausts to `failure` after 3 missing/invalid attempts.
- Duplicate `submit_outcome` calls keep the first; subsequent calls
  return tool-errors.
- Max-turns-reached returns `failure` unless `needs_review` is in
  the allowed set.
- Permission-denied returns `failure`.
- `parseOutcome` and `resultPrefix` are removed from the tree.
- Adapter event `outcome.finalized` is emitted on every successful
  finalize; structured failure event is emitted on exhausted
  reprompts.
- Every adapter unit test in Step 5.1 passes.
- The conformance propagation test in Step 5.2 passes.
- The engine-guard regression test in Step 5.3 passes.
- `make ci` and `make test-conformance` green.
- `docs/plugins.md` documents the new contract; the prior prose
  documentation is removed (not retained).
- CHANGELOG text for W16 is captured in reviewer notes.

## Tests

Eleven adapter unit tests (Step 5.1) + one conformance test
(Step 5.2) + one engine-guard regression (Step 5.3). Race-safe;
deterministic. Existing Copilot tests must remain green after
migration to the tool-call fixture path.

## Risks

| Risk | Mitigation |
|---|---|
| `copilot-sdk/go v0.3.0` API for `DefineTool` / `ToolResult` differs from the architecture archive note's pseudo-code | Read the SDK source / godoc before writing the call. The pseudo-code is from the archive note's pre-merge research; only the *shape* (typed-tool, SkipPermission, handler signature) is locked, not the precise type names. Adjust call sites to match the actual SDK. |
| Tool errors returned from the handler end the turn instead of allowing in-turn retry | The archive note Phase 2 §4 prescribes returning a `ToolResult` with error content (not a Go error). Verify against the SDK before merging. If the SDK does not expose an in-turn retry path, fall back to a single-call-per-turn model and rely on the reprompt loop alone — document the deviation in reviewer notes and the docs. |
| Removing `parseOutcome` breaks an existing test that relied on the prose default | Audit all `parseOutcome` callers and tests before deleting; update or replace those tests to use the fixture's tool-call scenarios. The locked decision §1 forbids keeping the prose path. |
| The prompt preamble interferes with operator prompts that already enumerate outcomes | The preamble is mandatory and authoritative. Document it in `docs/plugins.md`. Operators with their own enumeration are now redundant but harmless — the model sees the structured tool plus the preamble plus their prose. |
| Workflows that depended on `needs_review` as the default fallback now fail differently | This is documented as a behavior change in the W16 CHANGELOG entry. Workflow authors who want the prior behavior must declare `needs_review` (and add a mapped transition) and rely on the max-turns path. The strict-failure policy is locked decision §2. |
| Per-step state on a session struct races against an Execute that did not call `beginExecution` (e.g. unusual lifecycle order) | `beginExecution` is invoked unconditionally at the top of `Execute`; the new fields reset there. The fixture-driven concurrency tests should run with `-race` to surface any miss. |
| Coordinating with W12's `OnAdapterLifecycle` plumbing | W12 has merged. This workstream consumes its `OnAdapterLifecycle` hook unchanged; do not modify W12's wiring. The `outcome.finalized` and failure events are *adapter* events (different surface from lifecycle events), so the two channels do not conflict. |
| The engine guard catches a regression where the adapter returns an outcome not in the allowed set | This is the intended defense-in-depth behavior (locked decision §6). The new test in Step 5.3 verifies it. The adapter tool handler also rejects out-of-set outcomes, so reaching the engine guard is itself a bug to investigate — not a normal operating path. |
| Existing `copilot_internal_test.go` is large (564 lines) and a pure addition makes it unwieldy | Split out a sibling `copilot_outcome_test.go` if the file would exceed ~750 lines after this workstream. Keep the split mechanical. |
| `CRITERIA_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS` env-gated event payloads need a parallel knob for finalize reasons | The `Reason` field is operator-supplied free text; treat it as already-allowed. Do not gate it on the sensitive-details flag in this workstream — file a follow-up if security review later requires it. |
