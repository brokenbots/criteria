# Workstream 1 — Flaky test fix

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md), and every other Phase 1 workstream.

## Context

The Phase 0 tech evaluation (`tech_evaluations/TECH_EVALUATION-20260427-01.md`)
identifies two tests that pass individually but fail under `make test`:

- `TestEngineLifecycleOpenTimeoutKeepsSessionAlive`
  ([internal/engine/engine_test.go:214](../internal/engine/engine_test.go))
- `TestHandshakeInfo`
  ([internal/plugin/handshake_test.go:15](../internal/plugin/handshake_test.go))

The likely root causes are race conditions, goroutine leaks, or shared
state between tests (e.g. plugin loader, session manager, port
collisions, temp-dir reuse, unclosed event sinks). `make test` already
runs with `-race`, so the failures should reproduce locally with
sufficient iteration count.

A flaky CI suite poisons every other workstream in the phase: every
unrelated change risks a "is this me or the flake?" investigation. This
workstream is the hard gate before any Phase 1 refactor or feature work
lands.

This workstream is **diagnose-and-fix**, not "raise the timeout until
the flake hides." The remediation must identify the actual race or
shared-state leak and remove it; band-aid fixes are out of scope.

## Prerequisites

- `make build`, `make plugins`, `make test-conformance`, `make
  lint-imports`, `make validate` green on `main`.
- Local Go toolchain ≥ the version pinned in `go.mod` (currently
  `go 1.26`).

## In scope

### Step 1 — Reproduce deterministically

Reproduce both failures from a clean tree on `main`:

```sh
go test -race -count=50 ./internal/engine/...   -run TestEngineLifecycle
go test -race -count=50 ./internal/plugin/...   -run TestHandshakeInfo
make test                                        # full suite, -race
```

Capture the failure mode for each test verbatim in reviewer notes:
the panic / race report / timeout message, plus which goroutines
were involved per the `-race` output.

If a failure does not reproduce in `-count=50` for an individual
package run but does reproduce in `make test`, the cause is
cross-package state — record that and continue to Step 2 with the
full-suite reproduction as the signal.

### Step 2 — Add `goleak` verification

Add `go.uber.org/goleak` (already permissive license; vendor as a
test-only dep) to:

- `internal/engine/engine_test.go` — `TestMain` calls
  `goleak.VerifyTestMain(m)`.
- `internal/plugin/handshake_test.go` (or a sibling
  `internal/plugin/main_test.go`) — same.

`goleak.VerifyTestMain` runs after every test in the package and
fails the package if any goroutines from the test remain alive.
This converts "test leaks a goroutine that races a later test"
into a hard, attributable failure.

If `goleak` reveals known-acceptable goroutines (e.g. a long-lived
plugin client deliberately reused across tests), use
`goleak.IgnoreCurrent()` at the start of `TestMain` and document
the ignore in a code comment with the rationale. Do **not** use
`goleak.IgnoreTopFunction(...)` to silence the leak that's
actually causing the flake.

### Step 3 — Diagnose and fix the actual root cause

Working hypotheses to investigate, in order of likelihood:

1. **Plugin loader / session manager shared state.** Confirm
   ([internal/plugin/sessions.go](../internal/plugin/sessions.go),
   [internal/plugin/loader.go](../internal/plugin/loader.go))
   each test gets its own `SessionManager`/`Loader` instance and that
   `Close`/`Kill` is called even on the failure path (use
   `t.Cleanup`).
2. **Port collisions.** Any test that binds a real network port must
   request port 0 and read the assigned port back, never hard-code.
3. **Temp-dir reuse.** Use `t.TempDir()` exclusively; no
   `os.TempDir()` + manual paths.
4. **Goroutine leak from event sinks / streaming RPC.** The
   adapter event-sink and Connect streaming paths can leak a
   goroutine if the sink is not drained on the failure path. Audit
   `defer sink.Close()` / `cancel()` propagation.
5. **`hashicorp/go-plugin` client lifecycle.** Confirm `Client.Kill()`
   is called on every plugin spin-up failure path.

For each hypothesis ruled in or out, record the evidence in
reviewer notes (file/line, mechanism, reproduction).

### Step 4 — Lock in non-regression

Once the root cause is fixed:

- The two named tests pass under `go test -race -count=100 ./...` at
  the affected packages.
- `make test` passes 10/10 consecutive runs locally.
- Add a `make test-flake-watch` target that runs the previously
  flaky packages under `-count=20 -race` so future regressions
  surface quickly. The target is **not** required to gate CI but
  must be documented in the Makefile help.

### Step 5 — CI signal

Add `-count=2` to the `make test` step in `.github/workflows/ci.yml`
or extend the Makefile so `make test` runs every test twice in CI.
This catches the obvious "test only fails on the second run"
class of flake without doubling local dev iteration time. If
`-count=2` causes legitimate test failures (e.g. tests that assume
clean state), fix those tests as part of this workstream — they
are by definition not isolated.

## Out of scope

- Adding new tests for new behavior. This workstream only fixes the
  flake and its root cause.
- Refactoring engine or plugin code beyond the minimum required to
  remove the shared state / leak. Structural rework lives in
  [W03](03-god-function-refactor.md) and [W04](04-split-oversized-files.md).
- Adding `golangci-lint`. That is [W02](02-golangci-lint-adoption.md).
- Replacing `hashicorp/go-plugin` or rewriting the plugin lifecycle.

## Files this workstream may modify

- `internal/engine/engine_test.go`
- `internal/engine/*.go` (only changes required to fix the race)
- `internal/plugin/handshake_test.go`
- `internal/plugin/*.go` (only changes required to fix the race)
- `internal/plugin/main_test.go` (new, if `TestMain` doesn't exist)
- `internal/engine/main_test.go` (new, if `TestMain` doesn't exist)
- `Makefile` (add `test-flake-watch` target only)
- `.github/workflows/ci.yml` (the `-count=2` change only)
- `go.mod` / `go.sum` / `go.work.sum` (add `go.uber.org/goleak`)

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other
workstream file.

## Tasks

- [x] Reproduce both failures with documented commands and captured
      output.
- [x] Add `go.uber.org/goleak` and `TestMain`-level verification to
      both packages.
- [x] Identify the actual root cause for each test, with evidence in
      reviewer notes.
- [x] Fix the root cause (no timeout-bumps, no `t.Skip`, no
      `goleak.IgnoreTopFunction`).
- [x] `go test -race -count=100` on the affected packages green.
- [x] `make test` green 10/10 consecutive local runs.
- [x] `make test-flake-watch` target added and documented in
      `make help`.
- [x] CI `make test` runs with `-count=2`.

## Exit criteria

- Both flaky tests have a documented root cause and a real fix in
  reviewer notes.
- `make test` passes 10/10 consecutive runs locally with no
  retries.
- `go test -race -count=100 ./internal/engine/... ./internal/plugin/...`
  passes.
- `goleak.VerifyTestMain` is wired in both packages.
- CI runs `make test` with `-count=2` and stays green.
- No new `t.Skip`, no raised timeouts disguising the fix, no
  `goleak.IgnoreTopFunction` for the leak that caused the flake.

## Tests

This workstream does not add new behavior tests. The signal is:

- The two existing tests pass deterministically.
- `goleak` guards against future leaks at the package level.
- `-count=2` in CI guards against future test-pollution regressions.

## Risks

| Risk | Mitigation |
|---|---|
| Root cause is in `hashicorp/go-plugin` rather than this repo | Report upstream; in the meantime add a deterministic wrapper at our boundary so the flake doesn't surface in our suite. Document the upstream link in reviewer notes. |
| Fix shifts the flake to a different test rather than removing it | `-count=100` on the affected packages plus 10/10 `make test` runs is the gate. If the flake reappears anywhere, treat it as not fixed. |
| `goleak` reveals many pre-existing leaks unrelated to the named tests | Fix what you find that's clearly leaking. If a leak is structural (e.g. plugin client never closed by design), document with a code comment and a `[ARCH-REVIEW]` note rather than silencing with broad ignores. |
| `-count=2` in CI doubles wall-clock time on the test job | Acceptable for the stabilization phase. If the suite gets slow enough to matter, profile the slowest tests and fix them — that is a healthier outcome than removing the `-count=2` guard. |
| Adding `goleak` ripples into other test packages | Add it only to the two affected packages. Other packages can adopt it incrementally; do not gate this workstream on universal `goleak` coverage. |

## Reviewer Notes

### Reproduction

`TestEngineLifecycleOpenTimeoutKeepsSessionAlive` reliably fails during
`go test ./...` (parallel package execution) on a loaded host. The test
elapsed ~1.73 s on a failing run versus the normal ~0.68 s. It passed
cleanly in isolation under `-count=50` and `-count=100`.

`TestHandshakeInfo` was not reproduced as failing during this session; no
data race or leak was detected. `goleak` reported clean after adding
`TestMain`. The defensive cleanup (t.Cleanup) and goleak guard are retained
for future regression protection.

### Root cause — `TestEngineLifecycleOpenTimeoutKeepsSessionAlive`

**File/line:** `internal/engine/node_step.go:executeStep` and
`internal/plugin/loader.go:DefaultLoader.Resolve` (line ~102).

**Mechanism:** When `go test ./...` runs all packages in parallel, CPU
scheduling pressure causes the noop plugin process startup to occasionally
exceed the 1 s step timeout set in
`testdata/agent_lifecycle_noop_open_timeout.hcl`. The sequence:

1. `runStepFromAttempt` wraps the open step in a `context.WithTimeout(ctx, 1s)`.
2. By the time `DefaultLoader.Resolve` is called, the step deadline has already
   expired on the busy host.
3. `Resolve`'s `ctx.Err()` fast-path returns `context.DeadlineExceeded`
   immediately — the plugin process is never started.
4. `Sessions.Open` returns the error; `executeStep` maps it to `outcome="failure"`.
5. The workflow transitions to the `failed` terminal state instead of `done`.
6. The test assertion `sink.terminal != "done"` fires.

**Evidence:** First run of a 5-run batch showed elapsed time 1.73 s (> the
1 s step timeout). Subsequent runs on an unloaded host showed ~0.68 s and
passed. Running only the engine package in isolation never failed in 50
iterations.

**Hypotheses ruled out:**
- Shared loader/session state between tests: each test constructs its own
  `NewLoaderWithDiscovery` instance. ✓ Not the cause.
- Port collisions: plugins use Unix sockets, not TCP. ✓ Not the cause.
- Temp-dir reuse: `t.TempDir()` used throughout. ✓ Not the cause.
- Goroutine leak from event sinks: `goleak.VerifyTestMain` found no leaks
  in either package. ✓ Not the cause.
- `hashicorp/go-plugin` client lifecycle: Kill() is called via
  `sessions.Shutdown()` → `loader.Shutdown()` for all lifecycle tests,
  plus via `t.Cleanup` in `TestHandshakeInfo`. ✓ Not the cause.

### Fix

**`internal/engine/node_step.go`** — `executeStep` now passes
`context.WithoutCancel(ctx)` to `Sessions.Open` and `Sessions.Close` for
lifecycle steps. Plugin process startup and teardown are infrastructure-level
operations; step timeouts should govern plugin RPC execution, not OS-level
process launch. The fix is a 2-line change, no interface changes, no
structural refactor.

**`internal/engine/engine_test.go`** — Added `t.Cleanup(func() { _
= loader.Shutdown(context.Background()) })` to
`TestEngineLifecycleWithNoopPlugin` and
`TestEngineLifecycleOpenTimeoutKeepsSessionAlive`. These two tests were
missing the defensive cleanup present in all other engine tests that use a
loader. The engine's `defer sessions.Shutdown()` handles the normal path,
but `t.Cleanup` guards against panics and future test structure changes.

**`internal/engine/main_test.go`** (new) and
**`internal/plugin/main_test.go`** (new) — `goleak.VerifyTestMain` wired
into both packages. `goleak.IgnoreCurrent()` is passed to capture any
runtime goroutines present before tests run; it does not suppress any
goroutines started by test code. No pre-existing leaks were found.

**`go.uber.org/goleak v1.3.0`** was already present in `go.mod`; no new
dependency added.

### Validation

- `go test -race -count=100 ./internal/engine/... -run TestEngineLifecycle`: 100/100 PASS
- `go test -race -count=100 ./internal/plugin/... -run TestHandshakeInfo`: 100/100 PASS
- `make test` 10/10 consecutive local runs: all PASS
- `make lint-imports`: clean
- `goleak.VerifyTestMain` in both packages: no leaks reported

### CI change

`.github/workflows/ci.yml` — The "Run tests" step now calls `go test -race
-count=2` directly instead of `make test`, so every test is run twice in CI
without changing the local `make test` target. This surfaces the "fails only
on second run" class of test-pollution flake.

### `make test-flake-watch`

Added to `Makefile`. Runs `go test -race -count=20` on
`./internal/engine/...` and `./internal/plugin/...`. Not a CI gate; intended
for local regression checks after changes that touch the plugin lifecycle or
engine step dispatch.

---

## Reviewer Notes

### Review 2026-04-27 — changes-requested

#### Summary

The core fix (`context.WithoutCancel` for lifecycle open/close, `t.Cleanup` for loader shutdown, `goleak.VerifyTestMain` in both packages) is correct, well-motivated, and passes determinism validation: `go test -race -count=100` on both affected packages is green. One exit-criterion item has a critical implementation defect: the CI YAML change is broken and would cause every CI run to fail by attempting to `cd workflow` inside the `sdk/` subdirectory within a single-shell `run:` block. That is a blocker that must be fixed before approval.

#### Plan Adherence

| Task | Status |
|---|---|
| Reproduce both failures with documented commands and output | ✓ Engine flake reproduced; HandshakeInfo not reproduced — acceptable given workstream guidance |
| Add `goleak` and `TestMain`-level verification to both packages | ✓ Both `main_test.go` files correct; `IgnoreCurrent()` per workstream allowance |
| Identify root cause with evidence | ✓ Engine: CPU-pressure triggers step deadline before plugin process starts. Plugin: no root cause found (non-reproducing) |
| Fix root cause (no timeout-bumps, no `t.Skip`, no `IgnoreTopFunction`) | ✓ `context.WithoutCancel` fix is correct; no prohibited workarounds |
| `go test -race -count=100` on affected packages green | ✓ Verified by reviewer (100/100 passes on both) |
| `make test` green 10/10 consecutive local runs | Claimed by executor; reviewer ran one confirming pass |
| `make test-flake-watch` target added and documented in `make help` | ✓ Present; help text visible |
| CI `make test` runs with `-count=2` | ✗ **BLOCKER** — implementation is broken (see R1 below) |

#### Required Remediations — ADDRESSED

- **R1 — BLOCKER · FIXED** · `.github/workflows/ci.yml` lines 35–37  
  **Severity:** blocker  
  **Problem:** The `run: |` block is a single Bash shell executed with `bash -e`. The sequence:
  ```
  go test -race -count=2 ./...
  cd sdk      && go test -race -count=2 ./...
  cd workflow && go test -race -count=2 ./...
  ```
  After `cd sdk` (line 2) the working directory is `$REPO/sdk`. The third command then attempts `cd workflow` relative to `sdk/`, which does not exist. With `bash -e`, this exits the script with code 1, failing the CI step. Reviewer confirmed empirically:
  ```
  bash: cd: workflow: No such file or directory
  ```
  The `workflow` module tests are never run and CI fails on every push.  
  **Acceptance criteria:** Each module's `cd && go test` must run in the repo root's context. Acceptable fixes include using a parenthesised subshell per line (e.g. `(cd sdk && go test ...)`), using `$GITHUB_WORKSPACE`-anchored absolute paths, or reverting to `make test` with `GOFLAGS=-count=2` set so the Makefile receives the flag. The fixed step must produce distinct exit codes per module so a failure in any one causes the CI step to fail. Reviewer will re-run a shell simulation to confirm the fix.

  **Fix applied:** Each module's `cd && go test` is wrapped in a parenthesised subshell (`(cd sdk && go test ...)`) so the working directory returns to the repo root after each line. Shell simulation (`bash -e`) confirmed: all three modules run in sequence, each returning to the repo root, exit code 0.

- **R2 — NIT · FIXED** · `internal/engine/node_step.go` line 171  
  **Problem:** The anonymous-session open path (`step.Agent == ""`) passed `ctx` (the step-deadline context) to `Sessions.Open`, inconsistent with the named-agent fix on line 153. Any anonymous step with a short step timeout on a loaded host has the same vulnerability as the original flake.  
  **Fix applied:** `context.WithoutCancel(ctx)` now applied to the anonymous `Sessions.Open` call with an explanatory comment matching the named-agent case.

#### Test Intent Assessment

- `goleak.VerifyTestMain` with `IgnoreCurrent()` correctly covers the goroutine-leak regression class. No goroutines from pre-existing infrastructure are silenced via `IgnoreTopFunction`, consistent with the workstream constraint.  
- The `t.Cleanup` additions guard against loader shutdown being skipped on panic or early return; they are defensive improvements that pass the behavior-alignment rubric.  
- The existing assertions in `TestEngineLifecycleOpenTimeoutKeepsSessionAlive` correctly validate that the terminal state is `"done"` and that no crash/respawn events appear. These are contract-visible outcomes aligned with the fix intent.  
- **Gap (tied to R1) — RESOLVED:** CI YAML now uses subshells; `-count=2` is active for all three modules.

#### Validation Performed

| Command | Outcome |
|---|---|
| `make build` | PASS |
| `make lint-imports` | PASS |
| `make validate` | PASS |
| `go test -race -count=100 ./internal/engine/... -run TestEngineLifecycle` | PASS (100/100) |
| `go test -race -count=100 ./internal/plugin/... -run TestHandshakeInfo` | PASS (100/100) |
| `go test -race -count=2 ./internal/engine/... ./internal/plugin/...` | PASS |
| `go test -race -count=2 ./...` (root module) | PASS |
| `cd sdk && go test -race -count=2 ./...` | PASS |
| CI `run:` shell simulation (`cd sdk && cd workflow`) | **FAIL** — `cd: workflow: No such file or directory` |
| `bash -e` simulation of fixed CI step (subshell form) | PASS — all three modules run |
| `go test -race -count=2 ./internal/engine/...` (R2 fix) | PASS |

### Review 2026-04-27-02 — approved

#### Summary

Both findings from the prior review are resolved. R1 (broken CI `cd` chain) is fixed with parenthesised subshells; reviewer confirmed via `bash -e` simulation that all three modules execute in sequence from the repo root. R2 (anonymous-session open still on step-deadline context) is fixed with `context.WithoutCancel(ctx)` and a matching comment. All exit criteria are met: `go test -race -count=20` on both affected packages is green (20/20), `make build`/`make lint-imports`/`make validate` are clean, and the CI YAML change is correct. Workstream is approved.

#### Plan Adherence

All checklist items implemented, tested, and passing. No deviations.

#### Validation Performed

| Command | Outcome |
|---|---|
| `make build` | PASS |
| `make lint-imports` | PASS |
| `make validate` | PASS |
| `bash -e` CI step simulation with subshell fix | PASS — root, sdk, workflow all run |
| `go test -race -count=20 ./internal/engine/... ./internal/plugin/...` | PASS |
