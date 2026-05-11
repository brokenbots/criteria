# test-01 — Adapter conformance suite expansion ⛔ adapter-rework gate

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** C (test buffer) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** **Phase 4 (adapter rework)** — this workstream is the gate. The adapter rework cannot start until this lands.

## Context

The conformance harness at [internal/adapter/conformance/](../internal/adapter/conformance/) is the contract every adapter must pass. Today it has 11 contract sub-tests across 4 files:

| File | Sub-tests | What they prove |
|---|---|---|
| `conformance_happy.go` | `name_stability`, `nil_sink`, `happy_path`, `chunked_io` | Basic invariants and streaming |
| `conformance_outcomes.go` | `outcome_domain`, `permission_request_shape` | Outcome string set membership; permission wire shape |
| `conformance_lifecycle.go` | `context_cancellation`, `step_timeout`, `session_lifecycle`, `concurrent_sessions`, `session_crash_detection` | Cancellation, timeouts, session open/close, multi-session, crash recovery |

That is solid for happy paths and one or two negative-path scenarios. **It is not solid enough to gate a full adapter rework.** The rework will inevitably introduce regressions in places the current suite does not exercise:

- **Error injection at the protocol boundary** — what happens when the plugin handshake half-completes?
- **Partial-failure recovery** — a tool call returns mid-stream, then the connection drops; does the engine recover the prior state?
- **Permission gate denial paths** — the happy denial is covered (`permission_request_shape`); the unhappy paths (deny-with-error, deny-after-timeout, deny-after-session-close) are not.
- **Concurrent session stress** — `concurrent_sessions` runs N concurrent sessions to a happy adapter; it does not stress the **lifecycle ordering invariants** under load (e.g. what if `CloseSession` arrives before `Execute` completes for a concurrent peer session?).
- **Lifecycle ordering invariants** — events should arrive in a specific sequence (`OnSessionOpened` before any `OnExecuteStarted`, `OnExecuteFinished` before `OnSessionClosed`, etc.). The current suite does not assert ordering directly.

This workstream **adds 7 new conformance sub-tests** covering these gaps, runs them against all three external adapters (`copilot`, `mcp`, `noop`) plus the built-in `shell` adapter, and ensures the suite is the safety net the rework can land against.

The new tests live in three new files under `internal/adapter/conformance/` so existing files don't grow unbounded. They are wired into `Run` and `RunPlugin` so every adapter automatically gets the new coverage.

## Prerequisites

- `make ci` green on `main`.
- All 11 existing conformance sub-tests pass for all four adapters (`shell`, `copilot`, `mcp`, `noop`) on `main`. Verify:
  ```sh
  go test -race -count=2 ./internal/adapters/shell/...
  go test -race -count=2 ./cmd/criteria-adapter-copilot/...
  go test -race -count=2 ./cmd/criteria-adapter-mcp/...
  go test -race -count=2 ./cmd/criteria-adapter-noop/...
  ```
- Familiarity with the existing `Options` struct at [internal/adapter/conformance/conformance.go:18-37](../internal/adapter/conformance/conformance.go#L18-L37) — most of the new sub-tests will need at least one new field on `Options`.

## In scope

### Step 1 — Add new fields to the `Options` struct

The new sub-tests need adapter-specific configuration. Extend `Options`:

```go
type Options struct {
    // ... existing fields ...

    // ErrorInjectionConfig optionally provides a config map that, when passed
    // to OpenSession, instructs the adapter to misbehave for error-injection tests.
    // Adapters that do not support error injection can leave this nil; the
    // related tests are skipped via t.Skip with a clear reason.
    ErrorInjectionConfig map[string]string

    // SupportsPartialFailure reports whether the adapter implementation can
    // be driven into a partial-failure state by ErrorInjectionConfig. When
    // false, partial_failure_recovery is skipped.
    SupportsPartialFailure bool

    // ExpectedLifecycleOrder is the canonical sequence of adapter.EventSink
    // event types this adapter emits during a happy execution. Used by
    // lifecycle_ordering_invariants. Example: ["session_opened", "execute_started",
    //   "execute_finished", "session_closed"]. Adapters omit events they don't emit.
    ExpectedLifecycleOrder []string

    // PermissionDenyWithErrorConfig optionally provides a config map that, when
    // passed to a step input, makes the adapter request a permission and then,
    // on receiving a deny, return a structured error rather than a clean outcome.
    // Adapters that don't have permission flows can leave this nil; the related
    // test is skipped.
    PermissionDenyWithErrorConfig map[string]string

    // ConcurrentSessionStressN is the number of concurrent sessions to run for
    // the lifecycle-stress test. Default 8 when zero. Adapters that genuinely
    // can't run >1 session can set this to 1 to opt out (the test then degenerates
    // to a single-session lifecycle check).
    ConcurrentSessionStressN int
}
```

These fields are **optional**. An adapter that doesn't set them gets sensible defaults (or the relevant test is skipped with a clear `t.Skip` message). Backwards compatibility for existing adapter tests is preserved — no existing call site needs updating to keep passing.

Convert `Options` to be passed by **pointer** in `Run`, `RunPlugin`, `runContractTests`, and `newPluginTargetFactory` if td-02 has not already done so. This eliminates 4 of the existing `//nolint:gocritic // W15: Options passes by value for API clarity` directives. If td-02 is in flight, coordinate via reviewer notes — only one workstream changes the signature.

### Step 2 — New sub-test: `error_injection_handshake`

New file: `internal/adapter/conformance/conformance_error_injection.go`.

```go
// testErrorInjectionHandshake drives the adapter into a half-completed handshake state
// (e.g. OpenSession returns success but the underlying plugin process is then signalled
// to drop the connection before the first Execute). Asserts the engine receives a
// well-defined error rather than hanging or panicking.
func testErrorInjectionHandshake(t *testing.T, name string, factory targetFactory, opts *Options) {
    if opts.ErrorInjectionConfig == nil {
        t.Skipf("%s: error injection not supported (Options.ErrorInjectionConfig is nil)", name)
    }
    // ... open session with ErrorInjectionConfig
    // ... call Execute
    // ... assert: error is non-nil
    // ... assert: error implements adapter.RetriableError or adapter.FatalError (whichever is appropriate)
    // ... assert: no goroutine is leaked (use goleak.VerifyNone)
}
```

Wire it into `runContractTests`:

```go
if opts.ErrorInjectionConfig != nil {
    t.Run("error_injection_handshake", func(t *testing.T) { testErrorInjectionHandshake(t, name, factory, opts) })
}
```

The test fixtures live under `internal/adapter/conformance/testfixtures/`. Add a new fixture plugin `testfixtures/handshake_dropper/` whose `OpenSession` succeeds but whose `Execute` blocks on an unreachable channel until the underlying process is killed externally — the test triggers the kill via a config knob like `error_injection: drop_after_open`.

For the four real adapters:
- `shell`: support `ErrorInjectionConfig{"error_injection": "exit_after_open"}` by spawning the inner process with a wrapper that exits non-zero after acknowledging the session. **Add a `parallel_safe` and `error_injection` capability declaration** so the adapter advertises the feature.
- `copilot`: support `ErrorInjectionConfig{"error_injection": "drop_session_after_open"}` by injecting a `chan struct{}` close into the test session.
- `mcp`: support a similar knob.
- `noop`: declare it does NOT support error injection — leave `ErrorInjectionConfig` nil in its conformance call. The sub-test will skip.

If an adapter genuinely cannot support the injection (e.g. `noop` is too minimal), skip is the right answer. The test must NEVER produce a false positive.

### Step 3 — New sub-test: `partial_failure_recovery`

In the same `conformance_error_injection.go`:

```go
// testPartialFailureRecovery drives the adapter through a multi-event Execute that
// emits N events and then injects a failure mid-stream. Asserts the engine receives
// the events emitted before the failure (not silently dropped) AND a terminal error
// indicating the failure point.
func testPartialFailureRecovery(t *testing.T, name string, factory targetFactory, opts *Options) {
    if !opts.SupportsPartialFailure {
        t.Skipf("%s: partial-failure recovery not supported", name)
    }
    // ... configure adapter to emit 3 events and fail
    // ... call Execute; collect events via a recording sink
    // ... assert: recorded events contain the first N before the failure
    // ... assert: returned err is non-nil with a structured failure type
    // ... assert: no goroutine leak (goleak.VerifyNone)
}
```

The test asserts:
1. **Pre-failure events are delivered.** The recording sink contains ≥ 1 event before the failure point. Adapters that can't deliver pre-failure events fail the test (this is the intended contract — fail with full context, not silently).
2. **Failure type is structured.** The error implements `adapter.FailureWithContext` (a new interface defined in this workstream — see Step 7) carrying the event index at which failure occurred.
3. **No goroutine leak.** Wrap the test body in `defer goleak.VerifyNone(t)`.

Wire into `runContractTests` under the `if opts.SupportsPartialFailure {` guard.

### Step 4 — New sub-test: `permission_deny_with_error`

New file: `internal/adapter/conformance/conformance_permission_paths.go`.

```go
// testPermissionDenyWithError drives a permission request through a deny path that
// also returns a structured error. Asserts the wire envelope shape and the engine's
// outcome routing match.
func testPermissionDenyWithError(t *testing.T, name string, loader plugin.Loader, opts *Options, info plugin.Info) {
    if opts.PermissionDenyWithErrorConfig == nil {
        t.Skipf("%s: permission deny-with-error not supported", name)
    }
    // ... open session
    // ... start Execute; collect permission request via recording sink
    // ... reply with Permit{Allow: false, Reason: "test deny"}
    // ... assert: returned outcome matches PermissionDenialOutcome (or "failure" when error)
    // ... assert: returned err is non-nil if deny-with-error path
    // ... assert: any pending goroutines exit within 2s
}
```

Add similar new sub-tests covering:

- `testPermissionDenyAfterTimeout` — engine takes too long to respond to the permission request; the adapter must time out gracefully and return a deterministic outcome.
- `testPermissionDenyAfterSessionClose` — the engine closes the session while the adapter is awaiting a permission decision; the adapter must abort its wait and return without panicking.

Wire all three into `RunPlugin` (since they need a plugin loader for the wire test) under appropriate `if opts.PermissionDenyWithErrorConfig != nil` and similar guards.

### Step 5 — New sub-test: `lifecycle_ordering_invariants`

New file: `internal/adapter/conformance/conformance_ordering.go`.

```go
// testLifecycleOrderingInvariants asserts the adapter's EventSink receives events
// in the canonical order declared by Options.ExpectedLifecycleOrder. Captures
// every event with a timestamp and asserts strict ordering on event types.
func testLifecycleOrderingInvariants(t *testing.T, name string, factory targetFactory, opts *Options) {
    if len(opts.ExpectedLifecycleOrder) == 0 {
        t.Skipf("%s: ExpectedLifecycleOrder not declared", name)
    }
    // ... use a recording sink that timestamps each event
    // ... drive a happy-path Execute
    // ... extract observed event types in arrival order
    // ... assert: filter the observed types to those in ExpectedLifecycleOrder, then
    //     assert the filtered sequence equals ExpectedLifecycleOrder exactly
    //     (other event types like Log are allowed to interleave freely)
}
```

The test captures **strict ordering on the declared types**, not exact equality on the full event stream (Log events can interleave between any two lifecycle events).

For the four adapters, declare `ExpectedLifecycleOrder` based on the actual event sequence the adapter emits:
- `shell`: `["execute_started", "execute_finished"]` (no session events for shell — it's stateless per call).
- `copilot`: `["session_opened", "execute_started", "execute_finished", "session_closed"]`.
- `mcp`: `["session_opened", "execute_started", "execute_finished", "session_closed"]`.
- `noop`: `["execute_started", "execute_finished"]`.

If the actual event-type names in the codebase differ, use the actual constants — verify by reading [internal/adapter/](../internal/adapter/) for the event-type definitions before writing the test.

### Step 6 — New sub-test: `concurrent_session_stress_with_lifecycle_assertions`

New file: `internal/adapter/conformance/conformance_concurrent_stress.go`.

```go
// testConcurrentSessionStress runs N concurrent sessions, each with M Execute calls,
// and asserts that lifecycle ordering invariants hold per-session under load.
// Stronger than testConcurrentSessions which only asserts no-panic.
func testConcurrentSessionStress(t *testing.T, name string, loader plugin.Loader, opts *Options, info plugin.Info) {
    n := opts.ConcurrentSessionStressN
    if n == 0 { n = 8 }
    if n == 1 {
        t.Skipf("%s: concurrent stress disabled (N=1)", name)
    }
    const executesPerSession = 5
    // ... spawn N goroutines
    // ... each opens a session, runs M Execute calls, closes the session
    // ... per-session: collect events; assert per-session ordering invariants
    // ... aggregate: no goroutine leak; no panics; no event-stream cross-contamination
    //     (event from session A never appears in session B's recording sink)
}
```

The cross-contamination assertion is the load-bearing one — it catches the class of bug where a shared mutable state in the adapter leaks events between sessions. This is exactly the kind of regression the adapter rework is most likely to introduce.

Wire into `RunPlugin`:
```go
t.Run("concurrent_session_stress", func(t *testing.T) {
    testConcurrentSessionStress(t, name, loader, opts, info)
})
```

The new test runs at `n=8` by default; the existing `testConcurrentSessions` is **left in place** (it's a happy-path no-panic check) but the stress test is the load-bearing one.

### Step 7 — Define the `FailureWithContext` interface

New file: `internal/adapter/failure_context.go`.

```go
package adapter

// FailureWithContext is implemented by structured error values that an adapter
// returns when a partial-failure scenario occurs mid-execution. The interface
// allows the engine to extract the event index at which the failure happened
// without parsing the error string.
type FailureWithContext interface {
    error
    // EventIndex is the zero-based index of the last successfully delivered event
    // before the failure. When no events were delivered, returns -1.
    EventIndex() int
    // Phase is a short identifier for the lifecycle phase in which the failure
    // occurred: "open", "execute", "close". Free-form is allowed but the four
    // adapters in tree should use these three values.
    Phase() string
}
```

This interface is the contract for the `partial_failure_recovery` test (Step 3). Each adapter implements it on whatever error type it returns from a partial-failure scenario; the test uses `errors.As` to verify.

The interface is added to `internal/adapter/` so all adapters can import it without going through the conformance package.

### Step 8 — Wire the new tests into all four adapters' conformance calls

For each adapter, update its conformance test file with the new `Options` fields:

- `internal/adapters/shell/conformance_test.go` — add `ErrorInjectionConfig`, `SupportsPartialFailure: true`, `ExpectedLifecycleOrder`, `ConcurrentSessionStressN: 8`. Implement adapter support for the injection knobs.
- `cmd/criteria-adapter-copilot/conformance_test.go` — same.
- `cmd/criteria-adapter-mcp/conformance_test.go` — same.
- `cmd/criteria-adapter-noop/conformance_test.go` — declare ExpectedLifecycleOrder; leave error-injection / partial-failure / permission-deny fields nil (the noop adapter has no permission flow). Confirm the related tests skip with the expected `t.Skip` reason; they should NOT fail.

Each adapter's implementation work is **bounded**: implement the test knobs, not new product behavior. The knobs are gated by config keys with a `error_injection: ` or `test_only: ` prefix that production code paths never set.

### Step 9 — Run against all four adapters and gate on ratchet-only progression

Establish a baseline of conformance test counts after Step 8:

```sh
go test -v -count=1 ./internal/adapters/shell/... 2>&1 | grep -c '^=== RUN.*/conformance/'
go test -v -count=1 ./cmd/criteria-adapter-copilot/... 2>&1 | grep -c '^=== RUN.*/conformance/'
go test -v -count=1 ./cmd/criteria-adapter-mcp/... 2>&1 | grep -c '^=== RUN.*/conformance/'
go test -v -count=1 ./cmd/criteria-adapter-noop/... 2>&1 | grep -c '^=== RUN.*/conformance/'
```

Record the per-adapter sub-test counts in reviewer notes. A new conformance sub-test added by a future workstream MUST appear in all four adapters' counts (or be explicitly skipped via `t.Skip` with a documented reason). This is the ratchet — sub-test count never goes down.

Add a make target:

```make
.PHONY: test-conformance-count
test-conformance-count:
	@bash tools/conformance-count.sh
```

`tools/conformance-count.sh` is a small new bash script that runs the four `go test -v` commands above, counts conformance sub-tests, and asserts the count for each adapter matches a hardcoded expected number stored in `tools/conformance-count.expected`. The expected file is a 4-line key=value:

```
shell=18
copilot=18
mcp=18
noop=14
```

(Numbers are illustrative — set them to the actual counts after Step 8.)

If a future workstream adds a conformance sub-test, it MUST update `tools/conformance-count.expected`. If a workstream removes a conformance sub-test, that's a breaking change — reviewer rejects unless explicitly justified.

Wire into CI under the existing E2E job in [.github/workflows/ci.yml](../.github/workflows/ci.yml):

```yaml
- name: conformance-count-check
  run: make test-conformance-count
```

### Step 10 — Validation

```sh
go test -race -count=2 ./internal/adapter/conformance/...
go test -race -count=2 ./internal/adapters/shell/...
go test -race -count=2 ./cmd/criteria-adapter-copilot/...
go test -race -count=2 ./cmd/criteria-adapter-mcp/...
go test -race -count=2 ./cmd/criteria-adapter-noop/...
make test-conformance-count
make ci
```

All seven must exit 0. Inspect:

- Each adapter's test output shows the new sub-tests running (or skipping with the expected reason).
- `goleak.VerifyNone` did not report any leaked goroutines.
- `tools/conformance-count.expected` matches actual counts.

Run with `-count=20` on the conformance package to stress concurrency:

```sh
go test -race -count=20 -timeout 600s ./internal/adapter/conformance/...
```

Must exit 0. Any flakiness is a real bug exposed by the stress; fix it as part of this workstream.

## Behavior change

**Behavior change: yes — additive in adapter behavior, no observable change for end users.**

The adapters now recognise specific test-only config keys (`error_injection: ...`, `test_only: ...`) that production code paths never set. When these keys are passed:
- Shell adapter exits non-zero after handshake / mid-execute.
- Copilot adapter drops the session post-handshake.
- MCP adapter does the same.
- Noop adapter ignores them (declares no support).

The `adapter.FailureWithContext` interface is new public surface in `internal/adapter/`. It's `internal/`, so not an SDK contract — but it is consumed by every adapter implementation and the conformance harness.

The conformance `Options` struct grows by 5 fields — backwards-compatible (all optional with sensible defaults).

No change to:
- Workflow HCL surface.
- CLI flags.
- Wire protocol (`pb.ExecuteEvent` envelopes).
- Engine behavior for production workflows.

## Reuse

- Existing `runContractTests` and `newPluginTargetFactory` orchestration in [internal/adapter/conformance/conformance.go](../internal/adapter/conformance/conformance.go).
- Existing `testfixtures/` plugin-binary infrastructure.
- `go.uber.org/goleak` if already a dep (check `go.mod`); otherwise pin a version. Goroutine leak detection is the load-bearing sanity check.
- Existing recording-sink helpers in [internal/adapter/conformance/assertions.go](../internal/adapter/conformance/assertions.go) and [fixtures.go](../internal/adapter/conformance/fixtures.go).
- `errors.As` from the stdlib for `FailureWithContext` detection.
- Existing CI E2E job — extend, don't add a new job.

## Out of scope

- Changing the production behavior of any adapter (other than recognising test-only config knobs).
- Changing the SDK public surface in `sdk/`. The `FailureWithContext` interface is `internal/`; if the rework needs to expose it via SDK, that is a separate workstream.
- Changing the `pb.ExecuteEvent` proto. Wire contract is immutable in this workstream.
- Changing the engine consumer of adapter events in `internal/engine/`. Conformance tests target adapters; engine consumer changes are separate.
- Reworking the existing 11 sub-tests. The new sub-tests sit beside the old ones.
- Increasing test coverage of `internal/adapter/conformance/` itself (the test infrastructure). The harness is the lock-in for adapters; recursive testing of the harness is a different concern.
- Adding tests for `internal/run/` or `internal/cli/`. Out of scope.
- Modifying `docs/plugins.md`. The new `Options` fields are documented inline in their Go doc-comments; if the rework demands public docs, that's a follow-up.

## Files this workstream may modify

- [`internal/adapter/conformance/conformance.go`](../internal/adapter/conformance/conformance.go) — extend `Options`; wire new sub-tests into `Run` / `RunPlugin` / `runContractTests`; convert `Options` to pointer if td-02 hasn't.
- New file: `internal/adapter/conformance/conformance_error_injection.go` (Steps 2 + 3).
- New file: `internal/adapter/conformance/conformance_permission_paths.go` (Step 4).
- New file: `internal/adapter/conformance/conformance_ordering.go` (Step 5).
- New file: `internal/adapter/conformance/conformance_concurrent_stress.go` (Step 6).
- New file: `internal/adapter/conformance/testfixtures/handshake_dropper/` — fixture plugin.
- [`internal/adapter/`](../internal/adapter/) — new file `failure_context.go` for the `FailureWithContext` interface (Step 7).
- [`internal/adapters/shell/`](../internal/adapters/shell/) — implement test-only knobs; update conformance call.
- [`cmd/criteria-adapter-copilot/`](../cmd/criteria-adapter-copilot/) — implement test-only knobs; update conformance call.
- [`cmd/criteria-adapter-mcp/`](../cmd/criteria-adapter-mcp/) — implement test-only knobs; update conformance call.
- [`cmd/criteria-adapter-noop/`](../cmd/criteria-adapter-noop/) — update conformance call (no implementation work; declares no support).
- New file: `tools/conformance-count.sh`.
- New file: `tools/conformance-count.expected`.
- [`Makefile`](../Makefile) — add `test-conformance-count` target.
- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — add the conformance-count CI step.
- [`go.mod`](../go.mod), [`go.sum`](../go.sum) — only if `go.uber.org/goleak` is not already pinned; add it.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files.
- [`docs/plugins.md`](../docs/plugins.md) (doc cleanup deferred to a follow-up).
- `internal/engine/`, `workflow/`, `internal/cli/`, `internal/run/`.
- [`.golangci.yml`](../.golangci.yml).

## Tasks

- [ ] Extend `Options` with 5 new optional fields (Step 1).
- [ ] Convert `Options` arguments to pointer (Step 1, coordinate with td-02).
- [ ] Add `error_injection_handshake` sub-test + handshake_dropper fixture (Step 2).
- [ ] Add `partial_failure_recovery` sub-test (Step 3).
- [ ] Add 3 permission-deny path sub-tests (Step 4).
- [ ] Add `lifecycle_ordering_invariants` sub-test (Step 5).
- [ ] Add `concurrent_session_stress` sub-test with cross-contamination assertion (Step 6).
- [ ] Define `adapter.FailureWithContext` interface (Step 7).
- [ ] Wire all four adapters into the new sub-tests; implement test-only knobs (Step 8).
- [ ] Add ratchet-only conformance-count check (Step 9).
- [ ] Validation including `-count=20` stress (Step 10).

## Exit criteria

- 7 new conformance sub-tests live in `internal/adapter/conformance/`.
- Each new sub-test runs (or skips with documented reason) for all four adapters.
- `tools/conformance-count.expected` exists and reflects actual sub-test counts.
- `make test-conformance-count` exits 0.
- `goleak.VerifyNone` passes in every new test.
- `go test -race -count=20 -timeout 600s ./internal/adapter/conformance/...` exits 0.
- `go test -race -count=2` exits 0 for each of the four adapters.
- `make ci` exits 0.
- The `adapter.FailureWithContext` interface is defined in `internal/adapter/failure_context.go` and used by at least one adapter's partial-failure error type.
- Phase 4 (adapter rework) gating ticket flips to "ready" upon merge.

## Tests

The Step 2–6 sub-tests ARE the deliverable. Their own correctness is validated by:

- Running each new sub-test against a deliberately broken fixture and confirming it fails. Document the failure mode in reviewer notes.
- Running each new sub-test against a deliberately correct fixture and confirming it passes. Already part of Step 10.
- The `-count=20` stress run.

No additional unit tests for the conformance harness itself in this workstream — recursive harness testing is a different scope.

## Risks

| Risk | Mitigation |
|---|---|
| The 7 new sub-tests are slow and bloat CI time | Each sub-test must complete in < 5s for happy-path cases. The stress test (`concurrent_session_stress`) gets a budget of 30s. Total CI time impact target: < 60s additional per adapter. Profile if exceeded. |
| Adapters that can't support an injection knob have to skip too many tests, weakening the suite | Skip with an explicit reason is acceptable for the noop adapter. For shell, copilot, mcp: the test-only knobs MUST be implementable. If an adapter genuinely can't be coerced (e.g. mcp can't drop a session mid-handshake without breaking the protocol), document the limitation and find a different injection point. |
| `goleak.VerifyNone` is too strict and fails on background goroutines that are intentional (e.g. plugin loader maintenance goroutines) | Use `goleak.IgnoreTopFunction` to whitelist the known intentional goroutines. Whitelist additions require a one-sentence reason in reviewer notes. |
| The conformance-count ratchet causes friction for legitimate test refactors | Refactors that consolidate sub-tests must update `tools/conformance-count.expected` and document the consolidation. The ratchet is a forcing function, not a hard wall. |
| Cross-contamination assertion in `concurrent_session_stress` produces false positives because the recording sink itself has a race | The recording sink uses a `sync.Mutex` around its slice. Run the test under `-race -count=20` for confidence. Any race the test detects is a real bug in the adapter under test. |
| Adding test-only config knobs to production adapter code creates a permanent attack surface | The knobs are gated by the `error_injection:` and `test_only:` config-key prefixes. Production workflows would never set these. Document in each adapter's README that the prefix is reserved. The workstream is a one-time cost; long-term cost is a single conditional branch in `OpenSession`. |
| The ratchet's hardcoded counts in `tools/conformance-count.expected` make local testing brittle (e.g. a developer adds a sub-test locally without updating the file) | The error message from `tools/conformance-count.sh` says exactly: "Adapter X had Y conformance sub-tests; expected Z. Update tools/conformance-count.expected if this is intentional." Self-explanatory failure mode. |
| The `FailureWithContext` interface is too narrow and a future failure type can't fit it | The interface has only two methods (`EventIndex`, `Phase`) and is `internal/`; widening it later is a non-breaking change. Start small. |
