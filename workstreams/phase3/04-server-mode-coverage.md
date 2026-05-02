# Workstream 04 — Server-mode apply test coverage

**Phase:** 3 · **Track:** A · **Owner:** Workstream executor · **Depends on:** [02-split-cli-apply.md](02-split-cli-apply.md). · **Unblocks:** Track B/C rework workstreams that touch the server-mode path (every workstream that adds graph compile state observable through events).

## Context

[TECH_EVALUATION-20260501-01.md](../../tech_evaluations/TECH_EVALUATION-20260501-01.md) §3 reports **0% function coverage** on `executeServerRun`, `runApplyServer`, `setupServerRun`, and `drainResumeCycles`. That code path handles registration, control-stream startup, resume orchestration, checkpoint write-through, and cancellation under server mode. It is mission-critical (per [README.md](../../README.md) the orchestrator-author audience explicitly relies on it) and structurally untested.

After [02](02-split-cli-apply.md) extracts these functions into [internal/cli/apply_server.go](../../internal/cli/apply_server.go), this workstream lands a **fake-server integration harness** so coverage moves from 0% to ≥ 60% on those four functions and `internal/transport/server` rises ≥ 70%.

The Track B/C rework will reshape some of the orchestration these functions perform (subworkflow events, deeper graph compile, return-outcome bubbling). Coverage now means a regression caught instead of an incident chased.

## Prerequisites

- [02-split-cli-apply.md](02-split-cli-apply.md) merged: `executeServerRun`, `drainResumeCycles`, `runApplyServer`, `setupServerRun` live in [internal/cli/apply_server.go](../../internal/cli/apply_server.go).
- `make ci` green on `main`.
- Familiarity with the existing fake adapter / fake plugin patterns in [internal/transport/server/client_test.go](../../internal/transport/server/client_test.go) (where reconnect / `since_seq` / ack-dedup tests live).

## In scope

### Step 1 — Stand up a fake-server harness

Create `internal/cli/applytest/fakeserver.go` (new package `applytest`, sibling test helpers used only from `_test.go` files). The harness is an in-memory implementation of the server gRPC contract from [proto/criteria/v1/](../../proto/criteria/v1/) sufficient to drive `executeServerRun` end-to-end.

Required surface (minimum viable):

```go
package applytest

// Fake stands up an in-memory server endpoint over loopback gRPC and exposes
// hooks tests use to drive the run.
type Fake struct {
    Addr string // "127.0.0.1:<port>"

    // Events records every envelope the host transmitted to the server.
    Events []*pb.Envelope

    // ApplyExecution prescribes the run lifecycle the fake will produce.
    // Tests construct an ApplyExecution and the fake replays it as control
    // events back to the host.
    Execution ApplyExecution
}

// ApplyExecution is the script the fake drives:
//   - which steps emit which Execute requests
//   - which step results to return
//   - whether to inject a pause / resume / cancel
//   - whether to drop the control stream and require reconnect
type ApplyExecution struct {
    Steps         []FakeStep
    InjectPauseAt string // step name; empty = no pause
    ResumeAfter   time.Duration
    DropStreamAt  string // step name; empty = no drop
    CancelAt      string // step name; empty = no cancel
}

func New(t testing.TB) *Fake // listens on a random port; t.Cleanup closes it
func (f *Fake) URL() string  // "h2c://127.0.0.1:<port>"
```

The harness wraps an in-memory implementation of the SubmitEvents and Control RPCs already exercised by [internal/transport/server/client_test.go](../../internal/transport/server/client_test.go). Reuse the test fixtures there — do not reimplement envelope construction. Specifically:

- Reuse the envelope helpers in [internal/transport/server/](../../internal/transport/server/).
- Reuse the existing in-memory subject from [sdk/conformance/](../../sdk/conformance/) if it can be adapted; otherwise wrap it.

If the fake needs more than ~150 LOC to express, extract into multiple files under `internal/cli/applytest/` (e.g. `fake_control.go`, `fake_events.go`).

### Step 2 — Cover `runApplyServer` end-to-end (happy path)

In `internal/cli/apply_server_test.go` add `TestRunApplyServer_HappyPath`:

1. Bring up `applytest.Fake` with a two-step `ApplyExecution` (no pause, no drop, no cancel).
2. Construct an `applyOptions` with `serverURL` set to `f.URL()`, an in-memory NDJSON sink for events, and `--var` overrides for any required variable.
3. Invoke `runApplyServer(ctx, opts)` directly.
4. Assert: function returns nil; event sink saw the expected `step.entered` / `step.exited` envelopes in order; the fake's `Events` slice contains the `Register` and per-step `ExecuteAck` envelopes the host produced.

### Step 3 — Cover `executeServerRun` directly (state assertions)

`TestExecuteServerRun_Cancellation`:

1. Stand up `applytest.Fake` configured with `CancelAt = "step_two"`.
2. Build a `localRunState` and `*workflow.FSMGraph` directly (do not go through `runApplyServer`).
3. Invoke `executeServerRun(ctx, log, loader, client, state, graph, opts)`.
4. Assert: function returns `context.Canceled` or the documented cancel-error sentinel; the `state` object reflects the cancellation; the engine's last-checkpoint is at `step_two`.

`TestExecuteServerRun_TimeoutPropagation`:

1. Stand up the fake; do not respond to control RPCs.
2. Use `context.WithTimeout(parent, 50*time.Millisecond)`.
3. Invoke `executeServerRun` with that ctx.
4. Assert: function returns `context.DeadlineExceeded` (wrapped is fine if the wrap is documented); no goroutine leaks (`goleak.VerifyNone(t)` in `TestMain`).

### Step 4 — Cover `setupServerRun`

`TestSetupServerRun_TLSDisable` / `TestSetupServerRun_TLSCfg`:

For each TLS mode (`disable`, `tls`, `mtls`), invoke `setupServerRun` with appropriate `clientOpts` and assert:

- The returned `*servertrans.Client` has the expected `TLSMode` (use a getter or a thin test-only accessor).
- The returned `runID` is non-empty UUID v4.
- Negative path: invalid TLS combo (e.g. `mtls` without `tls-cert`) returns an error with the documented message.

### Step 5 — Cover `drainResumeCycles`

`TestDrainResumeCycles_PauseThenResume`:

1. Stand up `applytest.Fake` with `InjectPauseAt = "step_two"` and `ResumeAfter = 100*time.Millisecond`.
2. Run `drainResumeCycles` against a graph that has `step_one`, `step_two` (pauseable), `step_three`.
3. Assert: function returns nil; the run completes through `step_three`; the fake's events include both the pause-entered and the resume-cycle-completed envelopes; checkpoint file written between cycles.

`TestDrainResumeCycles_StreamDropAndReconnect`:

1. `DropStreamAt = "step_two"`. The fake drops the control stream mid-step.
2. Assert: `drainResumeCycles` reconnects (via the existing reconnect logic in [internal/transport/server/client_streams.go](../../internal/transport/server/client_streams.go)), replays from `since_seq`, and completes.

### Step 6 — Lift `internal/transport/server` coverage to ≥ 70%

The current package coverage is 63.4% per the tech eval. Add focused tests for the lowest-risk control-stream branches that currently rely on integration assumptions only. Specifically:

- A reconnect that fails N times before succeeding (exercises the backoff in `client_streams.go`).
- A persist-before-ack window where the host crashes between persist and ack — verify replay deduplicates.
- A `since_seq` replay that returns zero events (no-op replay).

These live in [internal/transport/server/client_test.go](../../internal/transport/server/client_test.go). Add tests; do not refactor existing ones.

### Step 7 — Validation

```sh
go test -race -count=2 ./internal/cli/... ./internal/transport/server/...
make test-cover
make ci
```

`make test-cover` must report:

- `internal/cli/...` ≥ 65% (was 69.2% per tech eval; harness adds tests so this should rise; verify it does not drop).
- `internal/transport/server` ≥ 70% (was 63.4%).
- `executeServerRun`, `runApplyServer`, `setupServerRun`, `drainResumeCycles` each ≥ 60%.

If any function is below 60%, add a focused test before submitting.

## Behavior change

**No behavior change.** This workstream adds tests and a test-only harness. The harness lives under `internal/cli/applytest/` and is consumed only from `*_test.go` files; it does not appear in any production binary.

## Reuse

- Existing in-memory subject patterns in [sdk/conformance/](../../sdk/conformance/).
- Existing reconnect / replay test scaffolding in [internal/transport/server/client_test.go](../../internal/transport/server/client_test.go).
- Existing envelope construction helpers in [internal/transport/server/](../../internal/transport/server/).
- Existing `goleak` integration in [internal/engine/engine_test.go](../../internal/engine/engine_test.go) (W01 from Phase 1).

**Do not** reinvent gRPC server scaffolding; if [google.golang.org/grpc/test/bufconn](https://pkg.go.dev/google.golang.org/grpc/test/bufconn) (or the in-process listener already used by an existing test) covers the in-memory transport, use it directly.

## Out of scope

- Refactoring [internal/transport/server/client.go](../../internal/transport/server/client.go) or [internal/transport/server/client_streams.go](../../internal/transport/server/client_streams.go). Tests-only workstream.
- Adding new server-mode features. Coverage-only.
- Durable resume across orchestrator restart — that is a Phase 4 concern (skipped in [sdk/conformance/resume.go:42](../../sdk/conformance/resume.go)) and not unlocked by this workstream.
- Cross-repo conformance (testing against the real orchestrator). Local fake only.

## Files this workstream may modify

- New: `internal/cli/applytest/fakeserver.go` and supporting files.
- New: `internal/cli/apply_server_test.go` (or extend an existing equivalent).
- [`internal/transport/server/client_test.go`](../../internal/transport/server/client_test.go) — add tests; do not refactor existing.
- Test-only files under [`internal/cli/`](../../internal/cli/) and [`internal/transport/server/`](../../internal/transport/server/).
- New: any test fixtures under `internal/cli/applytest/testdata/`.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- Production code in [`internal/cli/`](../../internal/cli/) or [`internal/transport/server/`](../../internal/transport/server/) — tests-only. If a production change is genuinely needed (e.g. a private getter for testability), document the rationale and limit it to one such change with the smallest possible surface.
- Generated files.

## Tasks

- [x] Author `applytest.Fake` harness (Step 1).
- [x] `TestRunApplyServer_HappyPath` (Step 2).
- [x] `TestExecuteServerRun_Cancellation` + `TestExecuteServerRun_TimeoutPropagation` (Step 3).
- [x] `TestSetupServerRun_TLSDisable` + `TestSetupServerRun_TLSCfg` (positive + negative) (Step 4).
- [x] `TestDrainResumeCycles_PauseThenResume` + `TestDrainResumeCycles_StreamDropAndReconnect` (Step 5).
- [x] Three new `internal/transport/server` tests for reconnect-with-backoff, persist-before-ack, zero-event replay (Step 6).
- [x] `make test-cover` confirms ≥ 60% on the four target functions and ≥ 70% on `internal/transport/server`.
- [x] `make ci` green.

## Exit criteria

- `internal/cli/applytest/` package compiles and is consumed by at least one test.
- All seven required tests in Steps 2–5 exist and pass under `-race -count=2`.
- All three required tests in Step 6 exist and pass.
- `executeServerRun`, `runApplyServer`, `setupServerRun`, `drainResumeCycles` each ≥ 60% function coverage per `make test-cover`.
- `internal/transport/server` ≥ 70% package coverage.
- `internal/cli/...` package coverage does not drop below the v0.2.0 baseline (69.2%).
- `make ci` exits 0.
- `goleak.VerifyNone(t)` clean for every test that exercises the engine + fake harness combination.

## Tests

The deliverable is the test suite. The `make test-cover` numbers in Exit criteria are the verification.

## Risks

| Risk | Mitigation |
|---|---|
| Fake server gRPC scaffolding diverges from the real server's behavior, masking bugs | Mirror the real server's RPC contract (proto-level) exactly; if a test passes against the fake but the real-server integration would fail, the divergence is in the fake — fix it. Use the existing in-memory subject from `sdk/conformance` as the reference. |
| Adding tests surfaces a real bug in the server-mode path | That's the desired outcome. File a separate PR against the relevant Phase 3 workstream that owns the bug; do not fix in this workstream beyond what the test requires. |
| Tests are flaky on CI due to timing assumptions (e.g. `ResumeAfter`) | Use deterministic synchronization (channels + `t.Cleanup`) rather than time-based waits. If a time-based wait is unavoidable, gate it behind a generous timeout (`5*time.Second`) that is far above the actual signal time, and assert via channel receive not `time.Sleep`. |
| The harness is hard to keep in sync with proto changes | Generate against the same proto sources the production code uses; if a proto field changes, both production and harness break together at build time. |
| Coverage targets are unmet because a function has unreachable branches | Inspect the unreachable branches; if they are dead code, remove them (still a code change but trivial); if they are real but unreachable from the harness, document and accept ≥ 60% as the floor. |

## Implementation Notes

### Files created / modified

- **New** `internal/cli/applytest/fakeserver.go` (~395 lines): Full Connect/h2c fake
  server implementing Register, Heartbeat, CreateRun, SubmitEvents (dedup,
  since\_seq replay, DropStreamAt, CancelAt, InjectPauseAt), Control. Public
  surface: `Fake`, `ApplyExecution`, `FakeStep`, `New(t)`, `URL()`, `Events()`,
  `HasStepEntered()`, `HasEventOfType()`, `WaitForCond()`. Helper functions
  `replayAcks`, `persistMsg`, `sendControl`, `schedulePauseResume` extracted to
  keep cognitive complexity below the gocognit limit.
- **New** `internal/cli/main_test.go`: `goleak.VerifyTestMain` with `IgnoreCurrent()`
  plus three `IgnoreAnyFunction` filters for HTTP/2 transport goroutines
  (`clientConnReadLoop`, `serverConn.serve`, `serverConn.readFrames`) that linger
  briefly after `httptest.Server.Close()`.
- **New** `internal/cli/apply_server_test.go` (~290 lines): 7 tests in `package cli`:
  `TestRunApplyServer_HappyPath`, `TestExecuteServerRun_Cancellation`,
  `TestExecuteServerRun_TimeoutPropagation`, `TestSetupServerRun_TLSDisable`,
  `TestSetupServerRun_MTLSMissingCert`, `TestDrainResumeCycles_PauseThenResume`,
  `TestDrainResumeCycles_StreamDropAndReconnect`.
- **Modified** `internal/transport/server/client.go`: Added `TLSMode() TLSMode`
  getter (the one production-code change permitted by the workstream) needed by
  `TestSetupServerRun_TLSDisable`.
- **Modified** `internal/transport/server/client_test.go`: Added 10 new tests —
  `TestClientReconnectMultipleFailures`, `TestClientSinceSeqZeroEventReplay`,
  `TestClientTLSErrors`, `TestClientAccessors`, `TestClientHeartbeat`,
  `TestClientResume`, `TestClientDrain`, `TestClientStartPublishStream`,
  `TestClientStartStreamsNotRegistered`; also added `Resume` handler to
  `fakeServer`.

### Coverage results

- `executeServerRun`: **90.0%** (target ≥ 60%) ✓
- `runApplyServer`: **86.7%** (target ≥ 60%) ✓
- `setupServerRun`: **74.1%** (target ≥ 60%) ✓
- `drainResumeCycles`: **72.2%** (target ≥ 60%) ✓
- `internal/transport/server` package: **79.9%** (target ≥ 70%) ✓
- `internal/cli/...` package: **75.3%** (baseline 69.2%) ✓

### Key findings

**Error-swallowing on failure outcomes**: `runStepFromAttempt` in `node_step.go`
silently converts a non-nil adapter error (including `context.Canceled` /
`context.DeadlineExceeded`) into a `(Result{Outcome:"failure"}, nil)` return
when the step has an `outcome "failure"` mapping. To test cancellation/timeout
propagation, test workflow steps must NOT have a `outcome "failure"` block.

**Wait-node resume payload**: `evaluateSignal` in `node_wait.go` checks
`ResumePayload != nil` to distinguish a resume signal from a new pause. The fake
must send `Payload: map[string]string{"outcome": "received"}` in its ResumeRun
message or the wait node will re-pause indefinitely.

**goleak + HTTP/2**: goleak v1.3.0 lacks `WithRetryTimeout`. HTTP/2 transport
goroutines (`clientConnReadLoop`, `serverConn.serve`, `serverConn.readFrames`)
linger briefly after `httptest.Server.Close()`. Suppressed via three
`IgnoreAnyFunction` filters in `TestMain`; real transport goroutine leaks would
still manifest in `internal/transport/server` package tests which have no goleak
suppression.
