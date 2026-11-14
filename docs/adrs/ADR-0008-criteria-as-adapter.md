# ADR-0008 — Criteria-as-adapter: a criteria instance serving the adapter v2 surface

**Status:** Proposed

**Date:** 2026-10-02

**Deciders:** Project lead; design session 2026-10-02 (engine trace + team review).

---

## Context

v0.6.0 removes the shim: environments stand up via the criteria CLI as the
entrypoint, and a remote environment runs a full criteria instance that dials
the main process (ADR-0007 peer model). That substrate makes a previously
unnameable composition form concrete: **a criteria instance serving the
adapter v2 wire surface** — a workflow embedded in a parent graph through the
ordinary adapter seam.

The relationship to subworkflows (ADR-0009) is complementary, not competing:

- A **subworkflow** is compiled INTO the parent: `mergeCalleeGraph` inlines the
  callee graph, the parent FSM executes it, sessions/policies resolve in the
  parent process. Composition surface: shared sessions (CRI-145), adapter
  pass-in, borrow-not-owned binding. Identity is kept.
- A **criteria-workflow adapter** is a seam: the parent sees an adapter
  (handshake Info, Execute, typed outputs); a full criteria instance — own FSM,
  own run record, own persistence, own lockfile — executes the callee graph
  outside parent memory. Isolation surface: independent engine version, env
  policy, lifecycle, and run record. Identity is handed away.

Engine-trace findings that constrain the design:
- Compile-time schema discovery is eager and handshake-driven:
  `CollectSchemas` (`internal/diagutil/collect.go`) loads every declared
  adapter type and calls Info over v2 BEFORE `CompileWithContext`; step
  input/output validation runs against the returned `AdapterInfo` — so a
  criteria child answering Info from its own compiled graph (config schema =
  variable declarations, output schema = the terminal `output` projection)
  gets full parent-side compile-time conformance with ZERO new parent
  compile machinery.
- Capability gating has an existing seam: `adapterHasCapability`
  (`workflow/compile_adapters.go`) reads the handshake Capabilities list.
- Adapter v2 already carries Pause/Resume/Snapshot/Restore/Inspect as
  first-class RPCs; a criteria child honors them with real semantics — the
  child's `Engine.Pause` is the identical drain-snapshot-persist-ack
  machinery the host run uses.
- Adapter v2 has NO run-stop RPC (CloseSession is teardown, not orderly
  stop). Run-level control belongs on the peer connection, not the adapter
  surface: `PeerService.Control` (peer.proto) exists (KillChild arm) and the
  peer.proto header already reserves the Stage-B run-graph event lane
  ("extend SupervisionEvent arms; do not fork a second stream").
- Local-mode constraint: `wait`/`approval` nodes are rejected without a
  server-compatible orchestrator. Children used standalone must therefore be
  runnable against the parent-dialed control connection or avoid those node
  kinds (non-goal in this ADR; see D6).

**Design constraint (project-lead ruling):** standalone single-machine full
feature stands. Nothing in this design requires castle or any orchestrator:
parent + child are two criteria processes on one host, the child dials home
over localhost, and the child's run record lives in the child's own local run
store. Castle integration is optional future visibility plumbing, never a
dependency.

## Decision

### D1 — Zero engine knowledge: entrypoint-is-criteria plus a capability string

The engine gains NO new adapter kind, manifest kind, or HCL block. Any
adapter whose entrypoint happens to be a criteria instance and whose Info
handshake advertises `workflow.v1` in Capabilities may carry a workflow. On
the parent side:

- HCL references it as an ordinary `adapter` block with an ordinary
  environment binding.
- Parent compile validates step input/output references against the
  handshake AdapterInfo exactly as for any adapter (config schema = the
  child workflow's declared variables; output schema = the child's terminal
  `output` projection — both static HCL, available before any run).
- The `workflow.v1` capability lets the parent compile fail closed when an
  adapter claims workflow semantics it cannot verify, without any
  workflow-specific compile path.

### D2 — Parent-side execution surface: one-shot run-to-terminal per Execute

- Session open (per scope/session key, as today) launches and verifies the
  child; the child registers itself over the phone-home connection.
- Step Execute spawns the child RUN and streams until a terminal state:
  - child run lifecycle and node events forward on the adapter's existing
    event channels (Log RPC + ExecuteEvent), preserving the parent's
    NDJSON event-stream contract — the child run appears in the parent's
    event feed as adapter events with `workflow.v1` event kinds.
  - terminal state maps through the workflow outcomes vocabulary; the
    declared `outcomes` from the child handshake govern valid transitions.
  - outputs = the child's projected `output` map via `outputs_json`.
- Pause/Resume/Snapshot/Restore/Inspect ride the EXISTING adapter RPCs onto
  the live child run — a parent pause propagates (pauseSessions →
  session.Pause) into a genuine child-run checkpoint (drain in-flight
  adapter call, snapshot FSM, persist, ack), not an approximation.
- **Re-Execute guard (one-shot):** Execute on a session whose child run is
  already in-flight returns a typed error (fail closed). Re-Execute after a
  parent crash adopts the surviving child run from its run record instead of
  double-running — adoption is the crash-reattach rule; generalized
  adopt-across-steps is explicitly DEFERRED (D6) as a config-gated follow-on.
- Stop semantics: parent StopRun or scope teardown cancels in-flight child
  runs via the child's own machinery (D3 control arm) and criteria cleans up
  its processes when possible before session teardown. Nothing outlives its
  owner unless a future ADR introduces explicit detach (reject: detach by
  default — silent orphaned local runs on every parent crash).

### D3 — Run-level control rides the peer connection, not the adapter surface

Run-level verbs (stop/cancel now; detach/adopt extensions later) ride
`PeerService.Control` as additive arms — the child is a criteria peer with a
held phone-home connection — capability-gated on `workflow.v1` so plain
adapters never see the surface. Run-graph truth (child run state, task
completions) rides `SupervisionEvent` arms per the stage-B lane already
reserved in peer.proto. Rationale: widening the adapter v2 surface for run
semantics would Unimplement across the three adapter SDK ecosystems for a
contract only a criteria child can honor; the peer connection already
carries supervised-process control and a gapless-seq journal built to be
extended.

### D4 — Environment and policy doctrine preserved

- The child container IS its environment (v0.6.0 host-of-record doctrine):
  the child enforces env policy for ITS adapters — process scrub, sandbox
  primitives, fs/net policy, digest verification against ITS pin set.
- ONE-OWNER: the parent owns the seam (one tool-shaped adapter, grants
  shipped with the child's start command per the peer doctrine); the child
  is the host of record for its own adapters. No second engine gate on the
  parent side; no grant laundering across the seam.
- Inner-adapter isolation is the child's local stack. Per-child adapter
  concurrency shares one container budget: a per-child process budget is an
  explicit consequence to carry into the environment-policy spec.

### D5 — Standalone record: the child run is its own record

The child's run record, journal, and snapshots live in the child's own run
data directory — inspectable by running `criteria` read commands against it,
surviving parent crashes for the re-Execute adoption rule (D2). No castle or
orchestrator participation anywhere in this design; multi-process standalone
(parent + n children on one machine, child dialing localhost) is the
reference deployment.

### D6 — Non-goals (deferred, this ADR does not design them)

- Queue-await semantics (spawn once, advance/resume across steps,
  park-and-resume inventory, per-workflow queueing).
- Generalized adopt/detach lifecycle (parent-crash adoption in D2 is the
  minimal crash-reattach rule only).
- Child `wait`/`approval` against parent-adjacent control (requires a
  server surface decision).
- Castle-dual-homed children (optional future visibility plumbing).

## Consequences

- No wire/SDK BREAKING change: `criteria-adapter-proto` v2 untouched;
  `peer.proto` gains additive Control arms + SupervisionEvent arms
  (capability-negotiated). Conformance suite unaffected.
- Parent compile gets workflow-shaped adapters "for free" through the
  existing eager handshake path (D1) — the compiler never learns a new
  concept.
- Two criteria processes per workflow-adapter slot: memory/concurrency cost
  is explicit and per-slot, in exchange for independent versioning, env
  policy, and lifecycle isolation.
- Debuggability improves over inline subworkflow composition for isolated
  callees: the child run is inspectable in its own journal; a child crash is
  a wire fact, not a transport-error guess (ADR-0007 D1 rationale applies).
- Candidate wave decomposition (planning session will finalize): (W1) peer
  protocol arms for run control/run-graph events gated on `workflow.v1`;
  (W2) child-side serve-workflow mode on `criteria peer` (serve adapter v2 +
  run service over one phone-home); (W3) parent-side session/run mapping,
  Execute one-shot + re-Execute guard, pause/resume propagation;
  (W4) conformance kit for workflow-shaped adapters; (W5) example + docs;
  (W6) live validation run. W1 depends on the ADR-0007 peer substrate
  (cards in the current v0.6.0 wave); the whole series files DISARMED and
  sequences after the peer wave in the v0.6.0 wave plan.
- Rejected alternatives: manifest `kind = criteria-workflow` (labels the
  artifact, not the behavior — a criteria instance is discoverable by its
  handshake; adds a manifest concept for zero isolation gain); a first-class
  `workflow_adapter` HCL block (a second declaration path for what is
  mechanically an adapter slot); run control on the adapter v2 surface
  (Unimplements across three SDK ecosystems); orphan-by-default (silent
  local-run leak); designing queue-await now (deferred pending semantics
  the team wants to think through).
