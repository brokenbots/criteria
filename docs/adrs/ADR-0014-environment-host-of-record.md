# ADR-0014 — The environment as host-of-record: one session/ownership seam

**Status:** Accepted (2026-11-06, dave — design chat + decomposition session). Series filed as Kanboard KB-374 → KB-384 (project 1, UN-ARMED); KB-376 (M1) lands this record in `docs/adrs/` per the wave discipline — repo merge is the formalization act, the decision itself is settled and governs ticket bodies from now on.
**Date:** 2026-11-06
**Deciders:** dave; design trace from the subworkflow-session-leakage deep dive
(companion: `~/.hermes/plans/2026-11-06_123002-engine-subworkflow-session-leakage-behavior-table.md`)
**Sequencing:** ADR-0007 (peer execution, Stage A DONE) · ADR-0008
(criteria-as-adapter) · ADR-0012 (shell). This ADR executes the convergence those
three left open: "container and remote converge: one host-of-record, two launch
modes (criteria-launched vs external)" — ruled v0.6.0 doctrine, not yet built.

---

## Problem

The peer model (ADR-0007) made the criteria peer the host of record for
REMOTE environments only, and only on the dial path. Everything else still
treats the run process as the host, and the engine↔session code pays for that
asymmetry everywhere:

### P1 — Two supervision regimes, one wired into the other by string matching

`internal/peer` supervises adapter children with real exit facts, journals them
(gapless seq, replay), classifies crashes at the process (`CrashClassified`,
T-05/T-07). The host-side SessionManager consumes that journal verbatim — but
ONLY when a handle came off a peer session. Every other handle falls back to
`classifySessionCrash`'s legacy arms: `ProcessExited` (structurally false for
any non-local handle, the exact blind spot ADR-0007 §1 named) and error-text
string-matching ("transport is closing", "unavailable", "broken pipe", "eof",
"terminated" — `isLikelySessionCrash`). One failure domain has a journal; the
rest diagnose by hoping the transport's phrasing is stable.

### P2 — Session fate is process-global, so every consumer compensates

Sessions key on adapter instance name inside one run-scoped SessionManager.
The environment is ruled the isolation boundary (KB-214/253/342/346: ONE peer
pod per environment identity; scope isolation lives at the conn layer), yet
session BOOKKEEPING lives host-side:

- `remoteShimsByEnv` + `remoteShimForAdapter` + declaring-graph adapter caches:
  every session method branches on "is this remote?".
- `BorrowRemoteProvisioningFrom`: a fresh parallel-iteration SessionManager
  must have shims + caches COPIED from the parent so its per-scope tokens
  register on a listener it does not own. Pure compensation for ownership not
  following the environment.
- `deriveSibling` / `deriveForParallelIteration`: same compensation, second
  shape — lifecycle records cloned instead of scoped.
- `deferredPerScopeRemoteAdapters` re-walking the entire graph tree; CRI-269
  existed only because the eager path and the provisioning path disagreed about
  which adapters are remote.
- The detach ledger (KB-257), `tryReuseScopeInstance`, `tryAdoptPriorRunScopeInstance`,
  token-file scans and tombstones: host-side re-derivations of state whose
  authoritative copies (rotated tokens, current-instance records) already live
  on the shared data volume — which the PEER already reads (ScanRemoteScopes).
  The bookkeeping is split across both sides of the wire and reconciled by
  convention.

### P3 — Session control has no owner at the process that owns the process

OpenSession/CloseSession ride the v2 adapter contract (12 methods), but
session-fate semantics — who may keep a session across a scope boundary, when a
fresh rotation is forced, which session a re-entry resumes — are decided
engine-side through six scattered knob surfaces (SetRemoteShimForEnv,
RegisterScope/unregister, CloseHandle, wait budgets, scope registrar, session
sink). The peer sees none of them; it cannot enforce fate where the process
actually lives. This is the root of the subworkflow-session-leakage violation:
the boundary declaration has no enforcement point except engine bookkeeping,
which is exactly what hosting mode can bypass.

### Measured

`sessions.go`: 4,275 lines, ~130 methods, 36 call sites threading envKey/shim
plumbing between engine and SM. `internal/peer`: ~2,600 lines already
implements everything a local supervisor needs (child supervision, journals,
respawn, keepalive, kill) but is reachable only across a phone-home conn.

## Decision

### D1 — One seam: `EnvironmentHost`

Every environment declaration materializes ONE host object. The contract is
narrow and non-adapter-flavored:

```go
// internal/envhost (new package; no SDK change — in-process)
type Host interface {
    // Ensure registers the scope's adapter definitions and returns live
    // handles. Idempotent per (scope, definition): re-entry re-adopts, it
    // never re-provisions — the CRI-145 skip becomes the contract, not a skip.
    Ensure(ctx, scope ScopeRef, defs []Definition) (Handles, error)
    // Release ends a scope's engagement per the scope's DECLARED fate policy
    // (fresh: close + unregister + tombstone; detach: ledger entry, live).
    Release(ctx, scope ScopeRef, policy FatePolicy) error
    // Journal exposes supervision truth (spawn/exit/crash-classified) for
    // every child, local or remote — one diagnostic surface.
    Journal(scope ScopeRef, adapter string) JournalReader
    // Control: kill/respawn/open/close per (definition, scope). Replaces the
    // six scattered shim knobs.
    Control(ctx, req ControlRequest) (ControlResponse, error)
}
```

Bindings:

- `environment "remote"` → the peer-backed host (today's
  `peerSessionProvider` + shim, merged under one type).
- `environment "container"` → a criteria-launched local peer serving the same
  contract (in-process supervisor first; subprocess peer second). This IS the
  ruled container/remote convergence with a concrete carrier.
- `environment "shell"` / `sandbox` → the degenerate in-process host (same
  journal shapes; supervision is the real go-plugin parent's).

The run-level SessionManager shrinks to a SCHEDULER + LEDGER: which scopes
exist, VerifyGraph conformance, execute-turn routing to handles, detach/adopt
accounting, fate-policy evaluation at boundaries. It no longer knows what a
shim is.

### D2 — Supervision unifies on the journal; string heuristics die

`classifySessionCrash` becomes: read the journal's `CrashClassified` (peer or
in-process supervisor), else a structurally-known terminal condition. The
string-matching arms and `ProcessExited`-for-remote-composites delete with the
legacy-runner compat class in v0.6.0 (they exist only because supervision is
unreachable for non-peer handles today). One story: "read the journal" —
remotely and locally.

### D3 — Session fate becomes an env-host-enforced contract

Fate policy (the leakage fix, Item 2 of the 2026-11-06 design set) is declared
on the adapter instance (`continuation = "reuse" | "fresh"`) and ENFORCED in
`Host.Ensure/Release` — the same code
path in both hosting modes, which is what makes local and remote identical by
construction instead of by coincidence. Adopt/reuse across runner generations
(invocation-identity adoption, CRI-137/304 class) stays keyed to RUN identity
inside the host's record lookup — recovery is not continuation, and this ADR
does not change their semantics, only their address.

Ruling addendum (2026-11-08, session-identity × per-call adapter config —
Item 3 interplay): a per-call config change (model / reasoning effort /
max_turns / any future per-call agent config) FORCES a fresh session — the
conversation under the old config must not be inherited under the new one;
retuning a live session in place stays available only where the call did not
change config. Per-call config declared on a `continuation = "reuse"`
instance is a HARD COMPILE ERROR — contradictory intents on one call;
fail-loud beats last-writer-wins. These semantics belong to Item 3's future
per-call-config schema; M7's validation matrix carries them as recorded rules.
The compile-time re-entry rule — an adapter instance re-declared by both a
parent and a callee graph must declare one agreeing fate on both sides; an
undeclared or conflicting re-entry declaration is a hard validation error —
is M7's fail-loud shape, and the selective fresh release (KB-257's detach
path for everything not declared "fresh") is what the engine enforces.

### D4 — What the peer gains (adapter session control)

Once the peer fronts `Host` for remote envs, ledger verbs become first-class
peer control arms (extending `ControlRequest`'s oneof — next arm is 2+;
wire stays additive per ADR-0007 D2's numbering discipline):

- `OpenSession`/`CloseSession` per (definition, scope) as CONTROL, not
  side-effects of Execute.
- Execute-turn serialization enforcement peer-side for multiplexed sessions
  (KB-155 request multiplexing is the substrate; the host stops guessing
  capacity).
- Idle reaping / keepalive as declared policy (rides the tunables registry,
  not host timer constants), replacing the scope-session-pending wait budgets
  as the only fate input.
- Respawn/detach decisions co-located with journal truth.

### D5 — Migration (pipeline lane; cards, never direct engine edit)

Stage 1 — carve `envhost.Host` and bind it for REMOTE only, wrapping
`peerSessionProvider`; `sessions.go` consumes the seam. No behavior change
(engine-level session tests are the pin: remote reentry, reuse, adoption,
detach all stay green).

Stage 2 — in-process supervisor for CONTAINER envs off `internal/peer`'s
supervise/journal machinery (it is conn-agnostic: `adapterJournal`,
`Replay(cursor)`, `CrashClassified` are process-local types today). Local
container sessions stop using string heuristics.

One operator consequence of the supervisor shape: `InProcessHost.Kill()`
ends the session only and never kills the child process, so a hung
container step has no process-level abort; the child is reaped at host
shutdown, at a Control kill, or by crashing.

Stage 3 — shell/sandbox onto the degenerate host; delete the six knob surfaces,
the adapter caches, `deferredPerScopeRemoteAdapters`, `BorrowRemoteProvisioningFrom`,
`deriveSibling` (iterations reference the same host with their own scope keys —
parallelism already provisions N token sets against one env pod; that
operator contract is UNCHANGED).

Stage 4 — fate policy declaration + enforcement (the leakage card), riding the
seam instead of engine bookkeeping. Sequenced WITH Item 3 (per-call adapter
config): a config change must say whether it forces fresh.

Each stage ships independently green on the deployed fleet; Stage 1+2 unblock
the crash-classification deletion; Stage 4 must not precede Stage 1 (enforcement
needs the seam it lives in).

## Consequences

- **Positive:** one supervision story; ~1,500 lines of duplicated
  bookkeeping/borrowing/caching leave sessions.go; the remote/local branching
  disappears from 130 methods; the peer gets real session-control authority;
  local/container diagnostics stop being string-matching; the leakage
  declaration gets an enforcement point that hosting mode cannot bypass.
- **Negative/costs:** a new seam in the middle of a hardening cycle (mitigated:
  Stage 1 is a wrap, not a rewrite — existing tests pin behavior before any
  movement); `internal/peer` acquires an in-process entry point (new surface,
  additive); the env-host contract becomes load-bearing API (v0.6.0 window is
  the right time; pre-v1 breaking liberty applies).
- **Rejected alternatives:**
  - *Extend `RemoteShim` and keep everything in sessions.go:* reaches the P3
    goal but keeps P2's compensation machinery and the two-regime supervision;
    the seam would grow, not shrink.
  - *Move the SessionManager into the peer wholesale:* wrong boundary —
    scheduling/ledger is run-wide (parallel iterations, tool-resource leases,
    crashed-session sets cross environments); only ENVIRONMENT-scoped
    ownership moves.
  - *Do nothing / patch the leakage engine-side only:* the six knob surfaces
    stay, hosting modes keep diverging by accident, and the next CRI-269-class
    defect re-derives the same seams.
- **Operator contract unchanged:** one peer pod per environment identity,
  graph-derived manifest, conn-per-(scope, definition), provision_wanted /
  release reconciliation — all byte-identical. The operator never learns this
  seam exists.
- **SDK unchanged:** no Subject/ServiceHandler/proto field changes; the
  env-host surface is in-process Go; peer verbs are additive oneof arms.

