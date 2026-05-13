# WS26 — Cross-language conformance harness

**Phase:** Adapter v2 · **Track:** SDK · **Owner:** Workstream executor · **Depends on:** [WS23](WS23-typescript-sdk-v2.md), [WS24](WS24-python-sdk-v2.md), [WS25](WS25-go-sdk-v1.md). · **Unblocks:** [WS40](WS40-v2-release-gate.md) verification gate 1.

## Context

`README.md` D57.1. Conformance suite at `internal/adapter/conformance/` (WS03 ported its 11 sub-tests to v2). This workstream **expands** the suite to cover every v2 RPC and drives it against all three SDKs on all supported platforms.

The pre-phase-4 workstream `test-01-adapter-conformance-expansion.md` was superseded by this one (see [`workstreams/archived/superseded/test-01-adapter-conformance-expansion.md`](../archived/superseded/test-01-adapter-conformance-expansion.md)) because its deliverables targeted v1 protocol surfaces that WS02 / WS03 / WS37 retire. Its load-bearing test ideas are absorbed into Step 3 below.

## Prerequisites

WS23, WS24, WS25 merged or at least at RC tag.

## In scope

### Step 1 — Conformance test matrix

Define the matrix in `internal/adapter/conformance/matrix.yaml`:

```yaml
sdks:
  - go
  - typescript
  - python
platforms:
  - linux/amd64
  - linux/arm64
  - darwin/arm64
suites:
  - happy
  - outcomes
  - lifecycle
  - permissions     # NEW (bidi); includes 3 deny-path sub-tests from test-01
  - logging         # NEW (dedicated Log stream)
  - pause_resume    # NEW
  - snapshot_restore # NEW
  - inspect         # NEW
  - secrets         # NEW
  - sensitive_output # NEW
  - heartbeats      # NEW
  - chunking        # NEW
  - error_injection # NEW (from test-01): handshake-drop + partial-failure-recovery
  - ordering        # NEW (from test-01): lifecycle event ordering invariants
  - concurrent_stress # NEW (from test-01): N concurrent sessions + cross-contamination assertion
```

### Step 2 — Reference adapters per SDK

A `criteria-adapter-conformance-target-{go,ts,python}` adapter exists in each SDK repo (or in a single `criteria-adapter-conformance-targets` repo — coordinate with WS27 if added to starters). Each implements the suite's expected behavior under each test (e.g., emit N events, request specific permissions, snapshot/restore state, etc.).

### Step 3 — Sub-test implementations

Each suite gets its own file under `internal/adapter/conformance/`:

- `conformance_permissions.go` — sends N concurrent permission requests; asserts decisions arrive correctly; asserts audit log entries; asserts deny-with-error. Includes the three deny-path sub-tests harvested from the superseded test-01: `deny_with_error` (deny returns a structured error rather than a clean outcome), `deny_after_timeout` (host takes too long to respond; adapter must time out gracefully with a deterministic outcome), `deny_after_session_close` (host closes the session while adapter awaits a decision; adapter must abort the wait without panic).
- `conformance_logging.go` — adapter emits 100 log lines and 10 events; asserts ordering at host display; asserts heartbeats land.
- `conformance_pause_resume.go` — pauses mid-execution; asserts adapter stalls; resumes; asserts continuation matches.
- `conformance_snapshot_restore.go` — snapshots after N events; restores; asserts permission state replays; asserts secret re-resolution.
- `conformance_inspect.go` — Inspect returns sensible structured state during execution.
- `conformance_secrets.go` — adapter declares a secret; host provides via the secret channel; assert adapter reads via `secrets.Get`; assert process env does not contain the secret.
- `conformance_sensitive_output.go` — adapter emits a sensitive output; assert it's redacted in host logs; assert taint propagates.
- `conformance_heartbeats.go` — adapter stalls Log stream; assert heartbeat-stall crash detection.
- `conformance_chunking.go` — adapter emits a 16-MiB event; assert chunk reassembly is correct.
- `conformance_error_injection.go` *(harvested from superseded test-01)* — two sub-tests:
  - `error_injection_handshake` — driver flips the adapter into a half-completed handshake state (OpenSession-equivalent succeeds; underlying process is signalled to drop the connection before the first Execute). Asserts the host receives a well-defined error implementing the v2 retriable-vs-fatal contract rather than hanging or panicking. Uses a `handshake_dropper` fixture under `internal/adapter/conformance/testfixtures/`. Wrapped in `defer goleak.VerifyNone(t)` to catch leaked goroutines.
  - `partial_failure_recovery` — adapter is configured to emit N events and then inject a failure mid-stream. Asserts (1) pre-failure events are delivered to the recording sink, not silently dropped; (2) returned error implements a new `adapter.FailureWithContext` interface (Phase / EventIndex — defined for v2 in this workstream, not the v1 location the superseded workstream proposed); (3) no goroutine leak.
- `conformance_ordering.go` *(harvested from superseded test-01)* — `lifecycle_ordering_invariants`: recording sink timestamps every event; asserts that, after filtering to the canonical lifecycle event types (`session_opened`, `execute_started`, `execute_finished`, `session_closed`), the observed sequence equals the adapter's declared `ExpectedLifecycleOrder`. Log/heartbeat events are permitted to interleave freely. Each SDK's reference target declares its canonical order; adapters that omit a lifecycle event (e.g. stateless shell with no session block) declare the shorter sequence.
- `conformance_concurrent_stress.go` *(harvested from superseded test-01)* — `concurrent_session_stress`: spawns N concurrent sessions (default 8), each running M Execute calls (default 5). Per session, collects events and asserts per-session lifecycle ordering invariants. The load-bearing aggregate assertion is **cross-contamination**: no event recorded for session A may appear in session B's recording sink. This is the class of regression most likely to slip in during the v2 SDK build-out, where shared mutable state (session maps, permission correlation tables) crosses session boundaries. Run under `-race -count=20`; the recording sink uses a `sync.Mutex` so any race the test detects is a real bug in the adapter under test, not in the harness.

#### Supporting interface

Define `adapter.FailureWithContext` in the v2 host package (replacing the v1-targeted location the superseded test-01 proposed):

```go
package adapter

// FailureWithContext is implemented by structured error values an adapter
// returns when a partial-failure scenario occurs mid-execution. The host uses
// errors.As to extract phase + event index for routing.
type FailureWithContext interface {
    error
    // EventIndex is the zero-based index of the last successfully delivered
    // event before the failure. Returns -1 when no events were delivered.
    EventIndex() int
    // Phase is a short identifier for the lifecycle phase in which the
    // failure occurred: "open", "execute", "close".
    Phase() string
}
```

Each SDK's reference target adapter implements this on whatever error type it returns from a partial-failure scenario. The conformance test uses `errors.As`.

#### Capability declarations

Reference target adapters declare via the WS05 manifest which injection knobs they support. Suites whose injection is unsupported by a specific target skip with `t.Skipf("%s: <feature> not supported", name)` — never silent pass. Manifest keys:

- `conformance.error_injection` — boolean; gates `error_injection_handshake` and `partial_failure_recovery`.
- `conformance.permission_deny_paths` — boolean; gates the three deny sub-tests.
- `conformance.concurrent_stress.n` — integer; default 8, set to 1 to opt out.
- `conformance.lifecycle_order` — list of event-type strings; required for `lifecycle_ordering_invariants` to run.

Each of the three SDK reference targets (`criteria-adapter-conformance-target-{go,ts,python}`) MUST set all four — they exist specifically to exercise the harness. Production-shape adapters in WS30–WS36 may declare a subset; their conformance runs skip accordingly.

### Step 4 — CI matrix execution

GitHub Actions workflow `.github/workflows/conformance.yml`:

- 3 SDKs × 3 platforms = 9 jobs.
- Each job downloads the SDK-specific reference adapter binary (from the corresponding SDK CI artifact), runs the conformance suite, uploads the report.
- Failure on any job blocks merge.

### Step 5 — Tests

The conformance suite **is** the tests. Add a meta-test that ensures every suite in `matrix.yaml` has a corresponding file.

## Out of scope

- Adapter migrations using the conformance results — WS30–WS36 are validated by passing it.
- The release-gate roll-up — WS40.

## Behavior change

**No host behavior change.** New tests.

## Tests required

- All 9 matrix cells green.

## Exit criteria

- All conformance suites pass for all SDKs on all platforms.
- CI workflow gates merges.

## Files this workstream may modify

- `internal/adapter/conformance/*.go` *(new suite files + matrix.yaml)*, including the three files harvested from the superseded test-01: `conformance_error_injection.go`, `conformance_ordering.go`, `conformance_concurrent_stress.go`.
- `internal/adapter/conformance/testfixtures/handshake_dropper/` *(new fixture for `error_injection_handshake`)*.
- `internal/adapter/failure_context.go` *(new — v2 host `FailureWithContext` interface)*.
- `.github/workflows/conformance.yml` *(new)*.
- Reference target adapters in each SDK repo (must declare the four `conformance.*` manifest fields).

## Files this workstream may NOT edit

- SDK source — additions to SDK repos go through WS23/WS24/WS25.
- Other workstream files.
