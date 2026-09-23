# ADR-0006 — SendPrompt: mid-turn agent message injection and redirect semantics

**Status:** Accepted

**Date:** 2026-09-23

**Deciders:** Project lead (this repo) — normative plan CRI-214, mini-milestone
M12 (CRI-258); implementation ticket CRI-259.

---

## Context

A running workflow executes steps on adapters. Agent-class adapters (copilot,
and any adapter backed by an interactive agent SDK) hold a live session whose
behavior an operator may need to steer mid-run: correct course after an
external event, answer a question the agent surfaced, or tighten/loosen a
reviewer's instructions without stopping the run. Today the only way to affect
an in-flight agent step is to cancel the run and re-arm it with a modified
ticket — losing the session, the conversation context, and (in k8s) the entire
run state.

The wire contract already carries most of the plumbing as schema-only stubs:

- `ServerService/SendPrompt(SendPromptRequest)` in
  [proto/criteria/v1/server.proto](../../proto/criteria/v1/server.proto) —
  schema-only, returns UNIMPLEMENTED
  (`sdk/pb/criteria/v1/criteriav1connect/server.connect.go`).
- `AgentPrompt` rides the agent Control stream as a Phase-2.3 stub
  (`criteria.proto` ControlMessage.oneof, field 2).
- Castle (the orchestrator) already carries `agent.message` events and
  RunControls (Stop/Pause/Resume) with owner-scoped authz; the console uses
  run-control writes under the CRI-196 allowlist pattern.
- `ResumeRun(signal, payload)` delivers wait/approval resume signals into
  *paused* nodes — a different lifecycle (node is blocked, not computing).
- The copilot adapter's Copilot SDK session exposes
  `Session.Send(ctx, MessageOptions)` which can be invoked while a turn is
  streaming — the SDK serializes it as the next user message in the live
  conversation. The shell adapter is one-shot per step: its `Execute` runs a
  command to completion; there is no session to inject into.

The missing middle is the full path: **caller → castle → agent Control stream
→ engine → the adapter's active session**, plus the first-class event record.

## Decision

### D1 — End-to-end shape (castle → agent → engine → adapter)

`SendPrompt` is a control-plane write, same trust class as RunControls:

1. **Caller → castle.** The caller invokes castle's ServerService
   `SendPrompt(run_id, step, prompt)`. Castle validates ownership (D4),
   resolves the run's owning agent registration, and pushes an `AgentPrompt`
   message on that agent's Control stream. Castle stores the injection as a
   first-class event *at acceptance time* (queued state), so the record
   exists even if the agent never delivers it.
2. **Castle → agent.** `AgentPrompt` (existing schema, field 2) is extended
   additively: `session_id` (see D2), `issued_at`, and `caller_criteria_id`
   for the delivery-side re-check. The agent loop routes it like
   `handleResume`: to the active run with a matching run_id.
3. **Agent → engine.** The active-run plumbing gains a prompt channel
   (mirroring `resumeCh`). The engine's step runtime receives it via a new
   host seam (the runtime-seam pattern: the seam is real; delivery into
   non-agent adapters is the stub).
4. **Engine → adapter.** The engine resolves the *active adapter session*
   for the addressed step (step-entered, outcome not yet returned) and calls
   the adapter prompt RPC (D3). No session, no match → clean typed error.

Delivery semantics: **queued-to-next-turn, not interrupt.** The Copilot SDK
serializes a `session.send` issued mid-turn as the next user message — the
agent absorbs it between its current tool call and its next reasoning turn.
This is the non-destructive default; the agent keeps its reasoning chain. An
interrupt (abort current turn, inject, restart) is explicitly out of scope:
it destroys in-flight tool work and can wedge the SDK turn state machine.

Failure modes (typed, not log lines): no live run → NOT_FOUND; run exists but
no active agent session for the addressed step → `NO_ACTIVE_SESSION`; adapter
type cannot accept prompts (shell) → `UNSUPPORTED_ADAPTER`; prompt rejected
by the adapter (over budget/invalid) → the adapter's error is forwarded.

### D2 — Addressing: run + step, resolved to the live session

Address by `(run_id, step)`. The step name is the caller's handle (it is what
Parapet shows); the engine maps step → the adapter instance currently
executing it → its live session. If the step is between attempts (re-queued
after a transient failure), the prompt is held and delivered to the next
attempt's session; if the step has *completed*, delivery fails with
`NO_ACTIVE_SESSION` (no retroactive injection). A `session_id` field is
accepted for callers that hold a session handle, but the engine resolves
step→session authoritatively; a session_id that doesn't match the live
session for that step is an error. One prompt per addressed step at a time;
a second SendPrompt while one is queued returns the queued one's issued_at
(idempotent), not a second injection.

### D3 — Adapter contract: v2 `Prompt` RPC (additive)

Add to `criteria-adapter-proto` v2 `AdapterService`:

```
rpc Prompt(PromptRequest) returns (PromptResponse);
message PromptRequest  { string session_id = 1; string prompt = 2; }
message PromptResponse { bool accepted = 1; string detail = 2; }
```

Additive = non-breaking (field/rpc numbers are new; no existing field moves —
the SDK contract rule). The go-adapter-sdk `Service` interface gains a
`Prompt` method with an `UnimplementedPrompt` embed (same pattern as
`UnimplementedPermissions`) so existing adapters keep compiling.

Adapter qualification:

- **copilot: yes.** `session.Send(mid-turn)` queues the message into the
  live conversation; the agent absorbs it on its next turn. The adapter's
  turn loop needs no restructuring — Send is safe mid-turn (the SDK
  serializes).
- **shell: no.** One-shot per step; no session. `Prompt` returns
  `accepted=false, detail="shell steps do not accept prompts"`; the host
  maps this to `UNSUPPORTED_ADAPTER`.
- The adapter's InfoResponse gains a capability flag
  (`supports_prompt = true|false`) so the host short-circuits D1's failure
  modes without a round trip.

### D4 — Authz: owner-scoped + console allowlist (CRI-196 pattern)

Same trust class as RunControls. Authorized callers:

- The run's **owner identity** (assignment owner for agent-submitted runs;
  `CallerCriteriaID == run owner`).
- The **console operator**, per the CRI-196 allowlist extension: the console
  user may inject into any run it can view (view-all), exactly like
  run-control writes.
- Agent tokens may NOT inject into runs they do not own (an agent's own run
  is allowed — the run's agent prompting itself is the owner).

Castle enforces at the RPC boundary; the agent re-checks `caller_criteria_id`
at delivery (defense in depth; the Control stream is point-to-point between
castle and the owning agent).

### D5 — Event record: first-class, not a log line

Emit an agent-owned, permanent-numbered event when the prompt is *delivered
into the session* (not at acceptance):

```
AgentPromptInjected agent_prompt_injected = <next permanent number>;
message AgentPromptInjected {
  string step = 1;
  string session_id = 2;
  string prompt = 3;        // full text; the caller is trusted (RunControls class)
  string caller = 4;        // owner or "console"
  google.protobuf.Timestamp delivered_at = 5;
}
```

Castle stores/fans it out verbatim; Parapet run detail renders it in the step
timeline. The castle-side acceptance record (D1) is a separate, internal
marker (delivery-pending → delivered) so a dropped Control message is
observable; only the delivered event is shown as injected.

### D6 — Wire changes are additive

- `SendPromptRequest` gains `session_id` (2→renumber-free: add fields
  `session_id`, `caller` — no field numbers move). Response gains
  `accepted`, `detail` alongside `issued_at`.
- `AgentPrompt` gains `session_id`, `issued_at`, `caller`.
- New `Prompt` RPC in criteria-adapter-proto v2 (additive rpc; SDKs re-gen).
- New `AgentPromptInjected` event with the next permanent field number.
- `make proto` regeneration; Go + TypeScript SDK stubs exposed for callers.

### D7 — Security posture

Injection writes into an agent's context — it can steer tool calls and
outcomes. Same trust class as RunControls: owner-scoped (D4), castle-enforced,
delivery re-checked. The injected message is recorded with its caller, so
every steer is attributable in the event stream. No rate limiting in this
phase; idempotency (D2) bounds accidental double-sends.

### D8 — Redirect is a follow-up ticket, explicitly out of scope here

Redirect (pause at a step boundary + re-enter a step with injected input) is
phase 2 of the feature and is NOT decided by this ADR's implementation:
it composes with Pause/Resume + `ResumeRun(signal, payload)` (a paused run
re-enters a node with injected payload — redirect is "pause + re-enter with a
*modified step input*"), and it interacts with review loops (inject into the
reviewer, not just the developer). It will be its own ADR/ticket after the
injection path is live-validated; nothing in this ADR pre-commits its shape
beyond noting the composition points. Shipped now: injection into the
*active* session only.

### D9 — Deterministic gating

Feature-gating is deterministic: capability check via the adapter's
InfoResponse (`supports_prompt`), host-side session-state check
(step-active), and typed errors for every not-possible path. No model
judgment anywhere in the gating.

## Consequences

- CRI-259 implements D1–D7; D8 is filed as a follow-up ticket before this
  ADR closes.
- The copilot adapter needs a small change (implement `Prompt` →
  `session.Send`; declare `supports_prompt`); the shell adapter needs only
  the Unimplemented default.
- Castle's SendPrompt interceptor moves from allowlist-stub to implemented;
  the event contract in Parapet gains a timeline entry.
- The SDK (go/typescript) regen is additive; no major-version bump.
- Live smoke validation (CRI-259 exit): an injected mid-turn message visibly
  changes the agent's subsequent behavior in a real develop step, observable
  in the run's event stream.