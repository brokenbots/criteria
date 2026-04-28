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

- [ ] Reproduce both failures with documented commands and captured
      output.
- [ ] Add `go.uber.org/goleak` and `TestMain`-level verification to
      both packages.
- [ ] Identify the actual root cause for each test, with evidence in
      reviewer notes.
- [ ] Fix the root cause (no timeout-bumps, no `t.Skip`, no
      `goleak.IgnoreTopFunction`).
- [ ] `go test -race -count=100` on the affected packages green.
- [ ] `make test` green 10/10 consecutive local runs.
- [ ] `make test-flake-watch` target added and documented in
      `make help`.
- [ ] CI `make test` runs with `-count=2`.

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
