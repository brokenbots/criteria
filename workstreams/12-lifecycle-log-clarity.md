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
  document this in the CHANGELOG (W14 territory; provide text in
  reviewer notes).

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

- [ ] Add `closing` atomic flag to the session struct; set in
      `SessionManager.Close`.
- [ ] Update `isLikelySessionCrash` to suppress on `closing`.
- [ ] Lower log level for expected EOF events in
      `internal/plugin/loader.go`.
- [ ] Add `OnAdapterLifecycle` to the `Sink` interface; implement
      across all sinks.
- [ ] Render the `[adapter: ...]` tag in concise console output.
- [ ] Update `docs/plugins.md` with the adapter-lifecycle-logs
      section.
- [ ] Add tests per Step 5.
- [ ] `make build`, `make plugins`, `make test`, `make ci` all green.

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
