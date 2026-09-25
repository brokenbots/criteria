# ADR-0007 — Peer execution: criteria-to-criteria remote adapters (staged A→B)

**Status:** Proposed

**Date:** 2026-09-25

**Deciders:** Project lead (this repo); wire review per
`REMOTE-ADAPTER-ARCHITECTURE-REVIEW-20260921.md` §8–§10.

---

## Context

A remote adapter runs outside the Criteria host process and phones home over a
TCP/mTLS connection. The current bridge is a **byte pipe**: the runner
(`cmd/criteria-adapter-remote-runner`) launches the adapter as a local
go-plugin child, dials the host's shim, authenticates with a JSON identity
frame, and then the shim bridges the raw gRPC byte stream onto a host-local
Unix socket that a go-plugin **Reattach** client consumes
(`setupUDS`/`bridgeAndDial`, `internal/adapter/environment/remote/shim.go:545/583`).
The session layer then talks to it through a `Handle` whose attached runner is
`noopAttachedRunner` (`internal/adapterhost/loader_reattach.go:89-92`).

At scale in Kubernetes this architecture is breaking, and the failure mode is
structural, not incidental:

1. **The host cannot see the adapter process.** For remote handles,
   `ProcessExited()` is structurally false (`noopAttachedRunner`), so the
   precise first branch of `classifySessionCrash`
   (`internal/adapterhost/sessions.go:1993`) — "adapter process exited before
   the call completed" — is unreachable remotely. Classification falls back to
   string-matching grpc-go transport errors ("transport is closing",
   "unavailable"), which name the transport's phrasing, not the failure fact.
2. **Evidence dies with the bridge.** Adapter logs and RPCs share the one
   bridged connection; a crash that severs it also kills the channel that
   would have carried the crash report (review §8.1).
3. **The runner is a capability gap.** `proxyService` forwards only 6 of the
   12-method `Client` surface (`internal/adapterhost/serve.go:22`);
   Pause/Resume/Snapshot/Restore/Inspect/Prompt fall through to the embedded
   `UnimplementedAdapterServiceServer` and fail at call time only.
4. **Per-scope isolation is Go-runner-only.** `per_scope_sessions` keys
   sessions by `adapterType+"\x00"+scope`, but the TS/Python ServeRemote
   handshake frames carry no `scope`, so non-Go adapters cannot use it.
5. **The runner's failure handling is the weakest link.** Fixed 2s reconnect
   with no backoff or jitter (`reconnectDelay`,
   `cmd/criteria-adapter-remote-runner/runner.go:35`), a fresh child process
   spawned on every reconnect, bare `net.Dial` with no timeout or
   ctx-cancellation (`serve_remote.go`), and a hardcoded 6-name env-scrub
   allowlist (`remoteEnvVars`, `runner.go:231`) that leaks new
   `CRITERIA_REMOTE_*` vars to children.
6. **The incident trail is real.** Nine engine-level CRI regressions
   (`internal/engine/cri{137,236,269,270,271,276,287,293,304}_repro_test.go`)
   document production pain in exactly these areas (idle-transport death,
   teardown cascades, stale pods, subworkflow dispatch, colliding shims).

The CRI-276 keepalive work, the CRI-137 wait-budget diagnosis classes, and the
identity gate (mTLS → identity pattern → lockfile digest → constant-time
token) are mature and worth keeping. The byte pipe is what's failing.

Precedents worth noting: HashiCorp go-plugin's own docs and Temporal both
reject go-plugin-over-networks for the same reason — the plugin lifecycle
model presumes a parent process that can reap children, and a tunnel removes
that. The alternative that works is semantic relay with a supervisor on the
far side: GitHub Actions runners phone home for jobs but stream typed events
over separate control/results channels; Temporal workers execute activities
locally and report outcomes over gRPC. The proposed design is the same shape.

## Decision

### D1 — Stage A: the peer-supervisor model (criteria-to-criteria, semantic)

A `criteria` instance in **peer mode** runs adapters as local, fully
instrumented go-plugin children and serves the host **semantic** gRPC over a
single phone-home connection:

- The **peer** owns process supervision: it launches the adapter via the
  normal loader path (real `ProcessExited`, stderr→hclog drain), applies the
  adapter's declared isolation, and is the gRPC **server** on the held
  phone-home connection (the runner's `singleConnListener` pattern,
  `serve_remote.go:125-167`, promoted into `internal/peer`).
- The **host** accepts the connection, runs the existing identity gate, and
  talks **typed gRPC** to the peer: the adapter v2 `AdapterService` surface
  (all 12 methods — lifecycle parity for free) plus a new supervision stream
  of process-lifecycle truth. No Unix-socket bridge, no reattach, no
  `noopAttachedRunner` on this path.
- Crash classification becomes a **wire fact**: the peer runs the same
  classification logic with the real exit facts and reports
  `CrashClassified{reason, detail}`; the host consumes it verbatim. The
  precise branch of `classifySessionCrash` becomes reachable for remote
  adapters.
- Evidence decouples from RPC liveness: supervision and log truth flow on
  the multiplexed HTTP/2 connection (separate streams), and terminal events
  are buffered in a peer-side journal and replayed on reconnect — so the
  crash report survives the stream that died.

Stage B (the execution graph: delegating subgraphs to peer child runs,
castle-brokered) is decided in principle — the supervision journal and the
peer substrate are its foundation — but its wire contract is a breaking
SDK change and gets its own ADR (ADR-0008, design draft in parallel; see
D6).

### D2 — Protocol home: `proto/criteria/v1/peer.proto` (additive, this repo)

The criteria↔criteria protocol lives next to `criteria.proto`/`server.proto`
in `proto/criteria/v1/`:

```proto
service PeerService {
  // Host->peer: open/reopen supervision. The peer streams lifecycle truth
  // strictly after since_event_seq (exclusive). Host-initiated: the
  // phone-home conn has the peer as gRPC server.
  rpc Supervise(SupervisionRequest) returns (stream SupervisionEvent);
  // Host->peer control plane (Stage A: kill child; future: respawn, detach).
  rpc Control(ControlRequest) returns (ControlResponse);
}

message SupervisionRequest { uint64 since_event_seq = 1; }

message SupervisionEvent {
  uint64 event_seq = 1;   // global monotonic per peer process, gapless
  google.protobuf.Timestamp at = 2;
  string adapter_type = 3; // process-level subject
  string scope = 4;        // "scopeName/scopeInstanceID" or "" (legacy mode)
  string session_id = 5;   // session-scoped events; "" for process-level
  oneof kind {              // next arm = field 11
    ProcessSpawned spawned = 6;    // pid, binary, digest, version
    ProcessExited exited = 7;      // exit_code, signal, idle_ms, graceful
    CrashClassified crash = 8;     // reason (shared const set), detail
    StreamFlushed flushed = 9;     // channel ("log"), up_to_seq
    SupervisionHeartbeat heartbeat = 10; // last_event_seq (journal lag)
  }
}
message ProcessSpawned { string binary=1; string digest=2; string version=3; int32 pid=4; }
message ProcessExited { int32 exit_code=1; int32 signal=2; uint64 idle_ms=3; bool graceful=4; }
message CrashClassified { string reason=1; string detail=2; }
message StreamFlushed { string channel=1; uint64 up_to_seq=2; }
message SupervisionHeartbeat { uint64 last_event_seq=1; }
message ControlRequest { oneof kind { KillChild kill_child=1; } string adapter_type=2; string scope=3; uint32 grace_ms=4; }
message ControlResponse { bool accepted=1; string detail=2; }
message KillChild {}
```

Key properties:

- **The journal is the truth source.** Global monotonic `event_seq` per peer
  process (gapless); at-least-once delivery; dedup on (peer, event_seq);
  replay after `since_event_seq` on reconnect — the same shape as the
  `since_seq` metadata on `SubmitEvents`. Terminal events (ProcessExited,
  CrashClassified) persist in a bounded peer-side journal (default 4096,
  env-tunable) and are replayed FIRST, so crash evidence survives the RPC
  stream that died.
- **Identity/session keys are unchanged.** Session keys stay
  `adapterType` (legacy) or `adapterType+"\x00"+scope` (per-scope,
  `shim.go:287`), and the auth order stays mTLS → identity pattern → lockfile
  digest → accept token / registered scope token, constant-time compare
  (`checkAdapterIdentity`, `shim.go:502`).
- **Stage B extensibility by design.** Run-graph events (child-run lifecycle,
  task-token completions) will ride the same gapless-seq journal — extend the
  journal, don't fork it. The oneof leaves field 11 as the next arm; forward
  numbers are never pre-reserved (reserve only abandoned arms); capability
  strings (e.g. `supervision.v1`, `adapter.v2.full`) negotiate protocol
  evolution instead of proto package churn (`criteria.v1` stays).

**Rejected alternative — extend `criteria-adapter-proto` v2:** the peer is a
criteria instance (a supervisor), not an adapter. Putting the protocol in the
adapter-proto repo couples it to the three adapter-SDK ecosystems' release
cadence and conflates supervisor semantics with adapter semantics. The
review's own recommendation (§10.1) points here.

**Rejected alternative — keep the byte pipe, harden it:** the review §7
priorities fix real bugs (frame cap, backoff, lifecycle wiring) without
moving meaning across the wire. That leaves the structural blind spot
(#1/#2) intact and re-accrues CRI debt; those hardening items survive as
small tickets where they still apply (e.g. the identity-frame cap).

### D3 — Wire direction: phone-home (peer dials host) in Stage A

Stage A keeps the phone-home property that works for docker compose, static
hosts, and k8s pods today: the peer dials out; no ingress is required on the
host; identity is verified at connection time. In k8s, the operator launches
peer pods as adapter isolation units; the host's `provision_wanted`
lifecycle event (which already carries digest, image ref, listen address,
token) remains what the operator reconciles on.

**Rejected alternative — host dials peer:** natural in-cluster (Service
discovery), but it breaks the no-ingress property, forces the host to
reconcile pod placement (orchestrator logic in Criteria), and does not fit
the static-host/compose deployment mode this feature also serves. Revisit
only under Stage B if castle-brokered topology demands it.

### D4 — Rollout: evolve `environment "remote"` in place, role-negotiated

The `remote` environment stays; peer mode is negotiated per connection via an
optional `role` field in the JSON identity frame:

```json
{"name":"...","version":"...","digest":"sha256:...","token":"...",
 "scope":"...","sdk_protocol_version":2,
 "role":"peer",
 "peer":{"criteria_version":"...","capabilities":["adapter.v2.full","supervision.v1"]}}
```

- `"role":"peer"` → the accept path hands the authenticated connection to a
  pluggable `PeerAcceptor` seam (T-06 implements the real one); the
  byte-bridge path is not taken.
- Absent role → today's `setupUDS`/`bridgeAndDial` byte-bridge path,
  unchanged. Both sides already tolerate unknown handshake fields, so a
  mixed fleet (legacy runner pods + peer pods) coexists through the
  migration window.
- No HCL change: the environment grammar is identical; the pod entrypoint
  changes from `criteria-adapter-remote-runner` to `criteria peer`.

**Identity-frame hardening rides along** (review §4.1): the identity frame is
capped at 16 KiB (reject oversize before parsing); this applies to both
paths.

**Rejected alternative — new `environment "peer"` kind:** a second remote-ish
environment doubles the compile-time registry surface and the HCL users must
learn, for no semantic difference; the role field carries the distinction
without a new kind.

### D5 — Per-scope isolation becomes language-agnostic by construction

Today `per_scope_sessions` works only for adapters served by the Go runner
(TS/Py ServeRemote handshakes carry no `scope`). In peer mode the peer — not
the adapter SDK — carries the scope in its handshake frame and keys its
journal events by it. The host's per-scope session keys
(`adapterType+"\x00"+scope`), token rotation (`rotateFreshRemoteScope`), and
scope-instance reuse/adoption (CRI-137/CRI-304) apply unchanged to peers of
any adapter language.

### D6 — Delete-after-parity and the Stage B handoff

The byte-bridge internals are deprecated when peer parity is proven in
production, removed one minor release later: `setupUDS`/`bridgeAndDial`
(`shim.go:545/583`), the remote reattach path (`LocalSocketDialer` +
`noopAttachedRunner` on the remote path), and the runner's
`proxyService`/`grpcAdapterServer` duplicates (their bridge is promoted into
`internal/adapterhost` so runner and peer share one implementation until the
runner is retired). TS/Python ServeRemote remain functional but deprioritized
— the peer wraps any v2 adapter regardless of language.

Stage B (execution graph) is deliberately out of scope here: child runs need
`parent_run_id` on run creation, a typed results channel, delegated-observer
ownership, and transitive cancel/resume — a breaking SDK change requiring an
SDK major bump per AGENTS.md. ADR-0008 drafts that contract in parallel
(SDK-bump lead time is long); its implementation is gated on Stage A parity
in production.

## Consequences

- **Crash fidelity becomes a wire fact.** The precise classification branch
  of `classifySessionCrash` becomes reachable for remote adapters; remote
  failures stop being diagnosed by string-matching transport errors.
- **Lifecycle parity.** Pause/Resume/Snapshot/Restore/Inspect/Prompt work
  remotely because the peer fronts the full 12-method `Client` surface.
- **Debuggability.** Two structured process logs (host + peer) replace a
  4-hop opaque byte pipe; the troubleshooting story becomes "read the
  supervision journal."
- **New work:** the `criteria peer` subcommand + `internal/peer` package
  (peer-side runtime), the host-side `PeerAcceptor`/peer-handle path, and the
  supervision journal protocol — all additive. The existing identity gate,
  keepalive policy, wait-budget logic, token machinery, and HCL stay.
- **Two failure domains.** Either side can now die independently; the journal
  + `since_event_seq` replay gives session re-attach semantics across peer
  restarts (the peer re-handshakes with the persisted scope token; CRI-137
  reuse/CRI-304 adoption apply).
- **Non-breaking for Stage A.** `peer.proto` is additive; `make test-conformance`
  must pass unmodified. Stage B is a breaking SDK change with its own ADR and
  major bump.
- **Migration window:** mixed fleets supported via role negotiation; runner
  + byte-bridge removed one minor release after peer parity is proven.