# Workstream 12 — Adapter lifecycle log clarity (UF#06)

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** [W11](11-reviewer-outcome-aliasing.md) (both touch the Sink interface — schedule the merge order to avoid conflicts).

## Context

Deferred user-feedback item #06 (preserved in git history at commit
`4e4a357`,
`user_feedback/06-reduce-adapter-process-churn-and-eof-noise-user-story.txt`):

> Current pain:
> - plugin EOF + process exited debug/info messages are frequent during transitions.
> - It is unclear when these events are expected versus actionable errors.
>
> Acceptance criteria:
> - expected EOF on normal shutdown is logged at lower verbosity or with explicit "expected" wording.
> - actionable failures are clearly distinguished from normal process lifecycle events.
> - run summaries include a compact per-step adapter lifecycle status.

Two touchpoints today emit lifecycle noise:

1. [internal/plugin/sessions.go:237-248](../internal/plugin/sessions.go#L237-L248)
   — `isLikelySessionCrash` heuristic that string-matches "eof",
   "broken pipe", "terminated", etc. When the heuristic is wrong,
   normal close-on-shutdown events get classified as crashes.
2. The plugin loader emits `io.EOF` log lines on normal stream
   close ([internal/plugin/loader.go:211](../internal/plugin/loader.go#L211))
   that surface in operator logs as scary stack-trace-like messages
   when in fact the adapter exited cleanly.

This workstream:

- Distinguishes **expected** lifecycle events from **actionable**
  failures by the *cause* (close-context propagation), not by string
  heuristics.
- Lowers the log level for expected events.
- Adds a compact per-step adapter-lifecycle status line to run
  summaries (concise mode).

This is a small, surgical workstream. The full verbose run-output
mode (UF#07) is deferred to Phase 3; this workstream lays the
groundwork by adding the lifecycle status line to the existing
concise mode.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with
  [internal/plugin/sessions.go](../internal/plugin/sessions.go),
  [internal/plugin/loader.go](../internal/plugin/loader.go), and
  the console sink rendering in
  [internal/run/console_sink.go](../internal/run/console_sink.go).

## In scope

### Step 1 — Track expected-close intent

Add a per-session "closing" flag in
[internal/plugin/sessions.go](../internal/plugin/sessions.go) that
the close path sets *before* tearing down the gRPC stream. Pseudocode:

```go
// On the session struct:
closing atomic.Bool

// In SessionManager.Close:
sess.closing.Store(true)
// then proceed with the existing teardown
```

Then in `isLikelySessionCrash`:

```go
func isLikelySessionCrash(sess *session, err error) bool {
    if err == nil {
        return false
    }
    if sess.closing.Load() {
        // Expected: caller initiated close; any subsequent EOF /
        // transport-closing / broken-pipe is the normal teardown.
        return false
    }
    // Existing string heuristic remains as a fallback for unsolicited
    // process exits, but only when not in a closing state.
    msg := strings.ToLower(err.Error())
    return strings.Contains(msg, "connection") ||
        strings.Contains(msg, "transport is closing") ||
        strings.Contains(msg, "unavailable") ||
        strings.Contains(msg, "broken pipe") ||
        strings.Contains(msg, "eof") ||
        strings.Contains(msg, "terminated")
}
```

Update the call sites accordingly (every place that calls
`isLikelySessionCrash(err)` now passes `(sess, err)`). If the
heuristic is centralized to one site, this is a small change; if
multiple sites call it, refactor to a helper.

### Step 2 — Lower log level for expected EOF

In [internal/plugin/loader.go:211](../internal/plugin/loader.go#L211)
and any other site that emits `io.EOF`-related log lines on stream
close, gate the log level on whether the close was expected:

- Expected close (the `closing` flag is set, or the surrounding
  context was canceled by the host): emit at **debug** level, with
  wording like `adapter "<name>" stream closed (expected)`.
- Unexpected close (no closing flag, no canceled context): emit at
  **warn** level with the existing wording.

The Criteria CLI uses `log/slog` (per the codebase pattern; verify by
grep for `slog.Debug`/`slog.Info`). The level routes through the CLI
log handler. Do not introduce a new logger; reuse the existing one.

### Step 3 — Adapter lifecycle status line in run summaries

Today the concise console sink renders per-step status. Extend it to
include a compact adapter-lifecycle indicator alongside the step
outcome.

Add a new sink event (coordinate with [W11](11-reviewer-outcome-aliasing.md)
since that workstream also adds a Sink method — pick a merge order
and conform):

```go
// OnAdapterLifecycle is emitted at adapter session lifecycle events
// (started, exited cleanly, crashed). status is one of:
//   "started", "exited", "crashed", "signaled".
// stepName is the step that owns the lifecycle event (empty for
// session-level lifecycle); detail is a one-line description (empty
// for clean exit).
OnAdapterLifecycle(stepName, adapterName, status, detail string)
```

Emit from:

- `SessionManager.Open` after successful plugin startup → `started`.
- `SessionManager.Close` after clean teardown → `exited`.
- `isLikelySessionCrash` (or its caller) when the heuristic fires →
  `crashed` with the error string as detail.

In `internal/run/console_sink.go`, render the lifecycle as a tag on
the step-status line. Example:

```
[ok]   build (shell, 2.3s) [adapter: started → exited]
[fail] review (copilot, 8.1s) [adapter: started → crashed: connection refused]
```

Keep it to one line per step. The existing renderer for `OnStepOutcome`
is the place to insert this — record the lifecycle in the per-step
state and render it alongside outcome/duration.

### Step 4 — Documentation

Update [docs/plugins.md](../docs/plugins.md):

- Add a "Adapter lifecycle logs" section explaining:
  - Expected close events log at debug level by default.
  - Unexpected exits log at warn level.
  - The `[adapter: ...]` tag in concise output.
- Note the slog level can be tuned via the existing CLI verbosity
  flag (whatever it is — confirm by inspecting `cmd/criteria/main.go`).

If a `--log-level` CLI flag does not exist, do **not** add one in
this workstream. Document the existing knob (probably an env var or
the slog default).

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

### Step 5 — Tests

- `internal/plugin/sessions_test.go` (extend):
  - `TestSession_ClosingFlagSuppressesCrashHeuristic` — set the
    closing flag, return an EOF from the gRPC stream, assert the
    crash heuristic returns false.
  - `TestSession_UnexpectedExitTriggersHeuristic` — without the
    closing flag, an EOF triggers the heuristic.
- `internal/plugin/loader_test.go` (extend):
  - `TestLoader_ExpectedCloseLogsAtDebug` — verify via a log capture
    that the EOF log is at debug level when the close was expected.
- `internal/run/console_sink_test.go` (extend):
  - `TestConsoleSink_LifecycleTag` — emit a sequence of
    `OnAdapterLifecycle` events and assert the rendered output
    contains the `[adapter: started → exited]` tag.

## Behavior change

**Yes — observable but not breaking.**

- Log level for expected EOF on adapter close drops from info/warn
  to debug. Operators on default verbosity will see fewer log lines.
- Concise output gains a per-step `[adapter: ...]` tag.
- New Sink method `OnAdapterLifecycle`. Every existing sink gains a
  no-op or rendering implementation.
- The crash heuristic suppresses when the `closing` flag is set;
  edge-case behavior should improve (fewer false positives), not
  regress.
- No HCL surface change, no CLI flag change, no proto change.
- Operators who *parse log output* for "EOF" or "process exited"
  patterns (a fragile but possible practice) may need to adjust;
  document this in the CHANGELOG (W16 territory; renumbered from W14
  on 2026-04-30; provide text in reviewer notes).

## Reuse

- Existing `slog` logger and verbosity routing.
- Existing `Sink` interface and concise-mode rendering.
- Existing `isLikelySessionCrash` heuristic — extend, do not
  replace.
- Existing session struct in `internal/plugin/sessions.go` — add the
  flag; do not refactor.

## Out of scope

- Full verbose output mode (`--output verbose`). That is Phase 3
  (UF#07).
- A new `--log-level` CLI flag. Use what exists.
- Restructuring the `slog` setup. Reuse the existing handler.
- Per-adapter log filtering (e.g. mute the copilot adapter while
  showing shell). Out.
- Replacing the string-matching crash heuristic with a typed-error
  scheme. The flag-suppression in Step 1 catches the noisy case;
  typed errors are a larger refactor for a future phase.

## Files this workstream may modify

- `internal/plugin/sessions.go` — add `closing` flag; pass `sess`
  into the heuristic.
- `internal/plugin/loader.go` — log-level gate for expected close
  events.
- `internal/plugin/sessions_test.go` (extend).
- `internal/plugin/loader_test.go` (extend).
- `internal/engine/engine.go` — add `OnAdapterLifecycle` to the
  `Sink` interface.
- `internal/run/console_sink.go` — render the `[adapter: ...]` tag.
- `internal/run/console_sink_test.go` (extend).
- All other sink implementations (locate via grep for `OnStepOutcome`):
  no-op or render-this-event implementations of
  `OnAdapterLifecycle`.
- `docs/plugins.md` — adapter-lifecycle-logs section.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the wire contract proto, the HCL surface, or
the CLI flags.

## Tasks

- [x] Add `closing` atomic flag to the session struct; set in
      `SessionManager.Close` and `Shutdown`.
- [x] Update `isLikelySessionCrash` to suppress on `closing`.
- [x] Lower log level for expected EOF events in
      `internal/plugin/sessions.go` (slog.Debug for expected, slog.Warn for crash).
- [x] Add `OnAdapterLifecycle` to the `Sink` interface; implement
      across all sinks (no-op on LocalSink and server Sink; fan-out on MultiSink;
      rendering in ConsoleSink).
- [x] Render the `[adapter: ...]` tag in concise console output.
- [x] Update `docs/plugins.md` with the adapter-lifecycle-logs
      section.
- [x] Add tests per Step 5.
- [x] `make build`, `make plugins`, `make test`, `make ci` all green.

## Exit criteria

- Setting the `closing` flag and returning EOF from a session
  results in `isLikelySessionCrash` returning `false`.
- Unsolicited EOF without the flag still triggers the heuristic.
- Expected close events log at debug level; unexpected exits log at
  warn level.
- Concise output renders the `[adapter: ...]` tag for every step
  that ran an adapter.
- All existing tests pass unchanged.
- `make ci` green.

## Tests

Three new tests per Step 5. Existing sink tests extend with a no-op
sanity check for `OnAdapterLifecycle`.

## Risks

| Risk | Mitigation |
|---|---|
| Coordinating `Sink` additions with [W11](11-reviewer-outcome-aliasing.md) | Land W11 first if it's ready; W12 inherits the pattern. If W12 lands first, document the precedent. Either way, all existing sink implementations gain *both* methods in a single PR sweep at merge time. |
| The `closing` flag races with an in-flight Execute call returning EOF mid-stream | The flag is set *only* by an explicit close path, not by Execute completion. An Execute that returns EOF without a Close call still triggers the heuristic. Test `TestSession_ExecuteEOFWithoutCloseIsCrash` covers this. |
| Lowering the log level hides a real intermittent crash from operators | The crash heuristic still fires for unexpected exits. Expected-close logs at debug remain available via the verbosity flag. The level change is conservative: warn → debug for the specific "EOF on closing stream" case only. |
| The `[adapter: ...]` tag clutters the concise output | Keep it to one line; render in dim color so it doesn't compete with the step outcome. If feedback comes back negative, gate it on a flag in a follow-up — not in this workstream. |
| The atomic flag adds contention on the session-close path | One atomic store and one load per close. Negligible. |

## Reviewer Notes

### Implementation summary

**`internal/plugin/sessions.go`**
- Added `closing atomic.Bool` to the `Session` struct.
- `SessionManager.Close` sets `sess.closing.Store(true)` before `CloseSession`+`Kill`.
- `SessionManager.Shutdown` sets `sess.closing.Store(true)` before teardown of each session.
- `isLikelySessionCrash(err error)` → `isLikelySessionCrash(sess *Session, err error)`: early return `false` when `sess.closing.Load()` is true.
- `SessionManager.Execute` now logs at `slog.Debug` when closing flag + error (expected), `slog.Warn` on crash heuristic trigger.

**`internal/engine/engine.go`**
- Added `OnAdapterLifecycle(stepName, adapterName, status, detail string)` to the `Sink` interface with W12 annotation comment.

**`internal/engine/node_step.go`**
- Lifecycle "open" step: emits `OnAdapterLifecycle(step.Name, agent.Adapter, "started", "")` after successful open.
- Lifecycle "close" step: looks up agent adapter, emits `OnAdapterLifecycle(step.Name, adapterName, "exited", "")` after successful close.
- Named-agent execute: emits `OnAdapterLifecycle(step.Name, adapterName, "crashed", execErr.Error())` on any Execute error.
- Anonymous session: emits "started" after open, "crashed" or "exited" after Execute based on result.

**`internal/run/console_sink.go`**
- Added `stepLifecycle map[string][]string` to `ConsoleSink` struct.
- Added `OnAdapterLifecycle` method: accumulates events per step with optional detail for "crashed".
- Updated `OnStepOutcome` to append a dim-color `[adapter: <events joined by " → ">]` tag.

**`internal/run/local_sink.go`, `internal/run/sink.go`** — no-op `OnAdapterLifecycle`.

**`internal/run/multi_sink.go`** — fan-out `OnAdapterLifecycle` to all children.

**All test sinks** (fakeSink, pauseSink, branchSink, benchSink, recordingSink, integrationSink) — no-op or bump `OnAdapterLifecycle`.

**`internal/plugin/sessions_test.go`** — added `TestSession_ClosingFlagSuppressesCrashHeuristic`, `TestSession_UnexpectedExitTriggersHeuristic`, `TestSession_ExecuteEOFWithoutCloseIsCrash`.

**`internal/plugin/loader_test.go`** — added `eofPlugin` stub + `TestLoader_ExpectedCloseLogsAtDebug` (uses `slog.SetDefault` capture).

**`internal/run/console_sink_test.go`** — added `TestConsoleSink_LifecycleTag`, `TestConsoleSink_LifecycleTagCrash`, `TestConsoleSink_LifecycleTagAbsent`.

**`internal/run/sink_test.go`** — extended `TestSink_PublishMethodsDoNotPanic` and `TestLocalSink_AllRemainingEvents` with `OnAdapterLifecycle` calls.

**`docs/plugins.md`** — added "Adapter lifecycle logs" section.

### Design notes

- Step 2 logging is in `sessions.go` (not `loader.go`): `loader.go:211` returns errors but never logged; the correct emission site is `SessionManager.Execute` which has both the session state and the error.
- The closing flag is set on the session before teardown in both `Close` and `Shutdown`, covering the race where an in-flight `Execute` returns EOF after a Close starts.
- `isLikelySessionCrash` retains full string-matching fallback for unsolicited exits; only the `closing` flag suppresses it.
- `OnAdapterLifecycle` lifecycle events are emitted from `node_step.go` (not `sessions.go`) to avoid the circular import constraint (`internal/plugin` cannot import `internal/engine`).
- Anonymous sessions emit all three events ("started", "exited"/"crashed") from within the single step execution, so the `[adapter: ...]` tag always shows the full lifecycle on that step's output line.
- `make ci` output shows live rendering: `✓ success in 9ms  [adapter: started → exited]` for the greeter plugin example.

### CHANGELOG note (for W14 / release notes)

> **Behavior change — adapter lifecycle logging:** Expected adapter closes (triggered by `SessionManager.Close` or `Shutdown`) now log at DEBUG instead of WARN. Unexpected exits continue to log at WARN. Operators who parse log output for "EOF" or "process exited" patterns for alerting may see fewer WARN entries and should validate their alerting rules.

### Review 2026-04-30 — changes-requested

(See full reviewer notes above; all three required remediations addressed in revision below.)

### Revision 2026-04-30 — remediations applied

#### Blocker 1 — Named-agent lifecycle emission fixed

`internal/engine/node_step.go`:
- Removed `OnAdapterLifecycle(..., "started", "")` from the `lifecycle == "open"` branch.
- Removed `OnAdapterLifecycle(..., "exited", "")` from the `lifecycle == "close"` branch (also removed the now-unused `adapterName` local in that branch).
- In the named-agent execution branch (`step.Agent != ""`): added `OnAdapterLifecycle(..., "started", "")` before `Execute` and `OnAdapterLifecycle(..., "exited", "")` on success path (crash path was already present).

`internal/engine/engine_test.go`:
- Added `lifecycleCaptureSink` type (embeds `fakeSink`, records lifecycle events by step name).
- Added `TestNamedAgentLifecycleEventsOnExecutionStep` regression test using `testdata/agent_lifecycle_noop.hcl`: asserts `run_agent` receives both "started" and "exited", and `open_agent`/`close_agent` receive none.

#### Blocker 2 — Host-canceled context expected-close case implemented

`internal/plugin/sessions.go`:
- In `Execute`, changed `if sess.closing.Load()` to `if sess.closing.Load() || ctx.Err() != nil` before the `slog.Debug("adapter stream closed (expected)")` call. Context cancellation by the host is now treated as an expected close and logs at DEBUG instead of WARN.

`internal/plugin/loader_test.go`:
- Added `canceledCtxPlugin` stub that returns `context.Canceled` from Execute.
- Added `TestLoader_HostCanceledContextLogsAtDebug`: pre-cancels the context (closing flag NOT set), calls Execute, asserts DEBUG log appears and no WARN appears.

#### Major — Docs corrected

`docs/plugins.md` "Tuning verbosity" section rewritten:
- Removed incorrect reference to `cmd/criteria/main.go` as the logger config site.
- Removed incorrect implication that `CRITERIA_LOG_LEVEL` controls slog lifecycle messages.
- Now accurately states: apply logger is fixed at `INFO` in `internal/cli/apply.go`; no `--log-level` CLI flag exists; debug messages visible only by swapping the slog default handler (example provided); `CRITERIA_LOG_LEVEL` governs only the go-plugin RPC-layer logger.

#### Validation

- `make ci` — **green** (all tests + lint + import boundaries + example validation).
- `TestNamedAgentLifecycleEventsOnExecutionStep` — PASS.
- `TestLoader_HostCanceledContextLogsAtDebug` — PASS.
- All pre-existing tests unchanged.

#### Summary

This is not approvable yet. Step 1 is in place and the repository validation targets are green, but the Step 3 lifecycle rendering is wired to the wrong steps for named-agent workflows, and the Step 2 logging/docs work stops short of the required host-canceled expected-close case. No separate security issue surfaced beyond the operator-facing logging/documentation mismatch.

#### Plan Adherence

- **Step 1 — Track expected-close intent:** implemented and covered. `closing` was added to `Session`, set in `Close`/`Shutdown`, and the crash heuristic now suppresses while closing.
- **Step 2 — Lower log level for expected EOF:** partially implemented. `internal/plugin/sessions.go` now emits `DEBUG` for the `sess.closing` path and `WARN` for crash-classified exits, but the workstream also required the surrounding host-canceled context to count as an expected close; that branch is not implemented or tested.
- **Step 3 — Adapter lifecycle status line in run summaries:** partially implemented. Anonymous adapter steps render a full tag, but named-agent workflows split lifecycle events across the `open` and `close` lifecycle steps instead of the step that actually executed the adapter work.
- **Step 4 — Documentation:** not acceptable as written. The new docs describe CLI logging control that does not exist in this tree and point at the wrong file for slog configuration.
- **Step 5 — Tests:** insufficient. The new tests miss the named-agent happy-path rendering bug and do not exercise the real expected-close boundary for host-canceled stream shutdown.

#### Required Remediations

- **blocker** — `internal/engine/node_step.go:448-485`, `internal/run/console_sink.go:115-135`. Lifecycle events are attached to `open`/`close` lifecycle steps, not to the named-agent step that actually runs the adapter. Repro: `./bin/criteria apply --output concise internal/engine/testdata/agent_lifecycle_noop.hcl` currently renders `[adapter: started]` on `open_agent`, no adapter tag on `run_agent`, and `[adapter: exited]` on `close_agent`. That misses the exit criterion _"Concise output renders the `[adapter: ...]` tag for every step that ran an adapter"_ and does not match the workstream examples. **Acceptance criteria:** the step that performs named-agent execution must render the lifecycle tag on its own outcome line for both success and crash paths, and add a regression test that fails on the current split-tag behavior.
- **blocker** — `internal/plugin/sessions.go:141-145`, `internal/plugin/loader_test.go:53-82`, `docs/plugins.md:449-466`. Step 2 required expected-close handling when either the session is explicitly closing **or the surrounding context was canceled by the host**. The implementation only logs the expected-close path when `sess.closing` is true, and the new logging test bypasses `loader.go`/stream shutdown entirely by hand-wiring a `SessionManager` with a fake plugin. **Acceptance criteria:** implement the host-canceled expected-close case, and add a test that exercises the real close-classification boundary instead of only the synthetic `sess.closing` path.
- **major** — `docs/plugins.md:457-466`, `cmd/criteria/main.go:13-29`, `internal/cli/apply.go:174-176`. The docs say the CLI logger is configured in `cmd/criteria/main.go` and imply a usable runtime knob for debug-level lifecycle logs, but in this repo the apply logger is created in `internal/cli/apply.go` at fixed `INFO`, and `CRITERIA_LOG_LEVEL` only affects the go-plugin logger. **Acceptance criteria:** correct the docs to describe the controls that actually exist in-tree and do not promise a CLI verbosity mechanism for slog lifecycle logs unless this workstream implements one.

#### Test Intent Assessment

- `internal/plugin/sessions_test.go` is strong for Step 1: it proves the close-flag suppression and unsolicited-EOF fallback at the heuristic boundary.
- `internal/run/console_sink_test.go` is too weak for Step 3: it manually calls `OnAdapterLifecycle` and only proves string formatting, not the engine wiring for named-agent `open → execute → close` flows. That is why the current split-tag regression passed.
- `internal/plugin/loader_test.go` is too weak for Step 2: despite the filename and test name, it does not exercise `loader.go`, a real plugin stream, or the host-canceled expected-close path. It only checks that a synthetic `SessionManager.Execute` path writes a `DEBUG` record when `sess.closing` is pre-set.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- `make ci` — passed.
- `./bin/criteria apply --output concise internal/engine/testdata/agent_lifecycle_noop.hcl` — acceptance mismatch reproduced: `open_agent` rendered `[adapter: started]`, `run_agent` rendered no lifecycle tag, and `close_agent` rendered `[adapter: exited]`.

### Review 2026-04-30-02 — changes-requested

(reviewer notes preserved above; remediation applied below)

### Revision 2026-04-30-03 — blocker remediated

#### Blocker — Context-cancel + EOF crash misclassification fixed

`internal/plugin/sessions.go` — `SessionManager.Execute`:
- Restructured error handling to check expected-close intent **before** calling `isLikelySessionCrash`. Both `sess.closing.Load()` and `ctx.Err() != nil` are now checked first; if either is true the function logs DEBUG and returns early, so a host-canceled context with an EOF/broken-pipe error can never reach the crash-heuristic branch and WARN path.
- Old flow: `isLikelySessionCrash(…) → crash path → WARN` (even when `ctx.Err() != nil` with EOF).
- New flow: `sess.closing || ctx.Err() != nil → DEBUG + return early`; only reaches heuristic when neither holds.

`internal/plugin/loader_test.go`:
- Added `TestLoader_HostCanceledContextWithEOFLogsAtDebug`: uses the existing `eofPlugin` (returns `"eof: connection terminated"`, which matches the crash heuristic), pre-cancels the context, and asserts DEBUG appears without WARN. This is the exact regression case.

`docs/plugins.md` — "expected close" definition updated:
- Now states: "An expected close is one where `SessionManager.Close` or `Shutdown` was called by the host **or** the surrounding execute context was canceled by the host (run timeout, user abort)."

#### Validation

- `make ci` — **green**.
- `TestLoader_HostCanceledContextWithEOFLogsAtDebug` — PASS (would have failed before the reorder).
- `TestLoader_ExpectedCloseLogsAtDebug`, `TestLoader_HostCanceledContextLogsAtDebug` — PASS.
- All pre-existing tests unchanged.

#### Plan Adherence

- **Step 1 — Track expected-close intent:** still implemented correctly.
- **Step 2 — Lower log level for expected EOF:** still partial. `SessionManager.Execute` now checks `ctx.Err() != nil`, but only after `isLikelySessionCrash(sess, execErr)` returns false. A canceled-context EOF / broken-pipe / transport-closing error still matches the crash heuristic first and therefore still logs `WARN`.
- **Step 3 — Adapter lifecycle status line in run summaries:** implemented correctly now. Named-agent workflows render the lifecycle tag on the execution step, not on the `open`/`close` lifecycle steps.
- **Step 4 — Documentation:** improved, but not yet fully accurate because the “expected close” definition still documents only the explicit close path and omits the intended host-canceled EOF case.
- **Step 5 — Tests:** improved, but the new Step 2 test still misses the actual boundary that remains broken.

#### Required Remediations

- **blocker** — `internal/plugin/sessions.go:141-148`, `internal/plugin/loader_test.go:56-90`, `docs/plugins.md:447-455`. The current control flow checks `ctx.Err() != nil` only inside the `!isLikelySessionCrash(...)` branch. That means a host-canceled execute context paired with an EOF-like error still takes the crash path, logs `adapter session crashed` at `WARN`, and fails the Step 2 requirement to treat host-canceled close-context propagation as expected. **Acceptance criteria:** reorder or refactor the expected-close classification so a canceled host context suppresses EOF / broken-pipe / transport-closing crash classification before the string heuristic fires; add a regression test that cancels the context and returns an EOF-like error (not `context.Canceled`) and proves `DEBUG` without `WARN`; update the docs’ “expected close” wording to match the final behavior.

#### Test Intent Assessment

- `TestNamedAgentLifecycleEventsOnExecutionStep` is a strong regression test and closes the Step 3 wiring gap.
- `TestLoader_HostCanceledContextLogsAtDebug` is still too weak for Step 2 because it uses a plugin stub that returns `context.Canceled` directly. That does not exercise the code path where `ctx.Err() != nil` and `execErr` still looks like `eof` / `broken pipe` / `transport is closing`, which is the real regression-sensitive case here.

#### Validation Performed

- `./bin/criteria apply --output concise internal/engine/testdata/agent_lifecycle_noop.hcl` — passed; `run_agent` now renders `[adapter: started → exited]`, while `open_agent` and `close_agent` render no lifecycle tag.
- `go test -race ./internal/plugin -run 'TestHandshakeInfo|TestPublicSDKFixtureConformance' -count=1` — passed.
- `make ci` — passed on rerun.

### Review 2026-04-30-03 — approved

#### Summary

Approved. The prior Step 2 blocker is now fixed: expected-close classification happens before the crash heuristic, so host-canceled execute contexts no longer misclassify EOF-like teardown errors as crashes. The named-agent lifecycle tag behavior remains correct, the docs now describe the host-canceled expected-close case, and the current tree meets the workstream exit criteria.

#### Plan Adherence

- **Step 1 — Track expected-close intent:** implemented and covered.
- **Step 2 — Lower log level for expected EOF:** implemented. `SessionManager.Execute` now treats both explicit close/shutdown and host-canceled execute contexts as expected-close conditions before crash-heuristic evaluation.
- **Step 3 — Adapter lifecycle status line in run summaries:** implemented. Named-agent execution steps render the lifecycle tag on the step that actually ran the adapter.
- **Step 4 — Documentation:** implemented. `docs/plugins.md` now documents expected close versus unexpected exit consistently with the final behavior.
- **Step 5 — Tests:** sufficient for this workstream. The new regression coverage now includes the exact canceled-context + EOF case that was previously missing.

#### Test Intent Assessment

- `TestNamedAgentLifecycleEventsOnExecutionStep` proves the behavior that matters for concise rendering and would fail on the prior split-tag bug.
- `TestLoader_HostCanceledContextWithEOFLogsAtDebug` now exercises the regression-sensitive boundary for Step 2: canceled host context plus an EOF-like error that would previously have matched the crash heuristic.
- The existing close-flag and unsolicited-EOF heuristic tests still provide good coverage for the non-canceled classification paths.

#### Validation Performed

- `make ci` — passed.
- `./bin/criteria apply --output concise internal/engine/testdata/agent_lifecycle_noop.hcl` — passed; `run_agent` rendered `[adapter: started → exited]` and the lifecycle steps rendered no adapter tag.
