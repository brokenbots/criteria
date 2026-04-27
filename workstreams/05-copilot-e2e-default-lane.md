# Workstream 5 — Copilot E2E in default lane

**Owner:** Test-infra agent · **Depends on:** none · **Unblocks:** [W08](08-phase0-cleanup-gate.md).

## Context

`cmd/overseer-adapter-copilot/conformance_test.go` skips its end-to-end
suite unless `COPILOT_E2E=1` is set, because it requires the `copilot`
CLI installed and configured. The split-era reviewer notes flagged
this as deferred work (W08 reviewer, "Copilot E2E moved into the
default test lane").

Letting a major adapter sit out of the default test lane is a slow
poison: regressions in the Copilot adapter only surface when a human
remembers to flip the env var. By the time someone does, the bug is
buried under unrelated changes.

This workstream brings Copilot E2E into the default lane by
substituting a deterministic fake for the real `copilot` CLI in CI,
keeping the real-CLI path available behind the existing env var for
local validation.

## Prerequisites

- `make test` green on `main`.
- The Copilot adapter conformance lane runs successfully when
  `COPILOT_E2E=1` is set in a local checkout with `copilot` on PATH.

## In scope

### Step 1 — Decide the fake's shape

Two viable shapes:

- **In-process fake.** Substitute the `copilot` interface at the
  Go boundary. Cheap; doesn't exercise the subprocess wiring;
  diverges from the real path in subtle ways (env propagation,
  signal handling).
- **Tiny binary fake.** Build `cmd/overseer-adapter-copilot/testfixtures/fake-copilot/`
  — a self-contained Go program that speaks the same stdin/stdout
  protocol as the real `copilot` CLI for the cases the tests
  exercise. Costs more upfront but exercises the subprocess
  boundary the way production does.

Recommend the binary fake. The plumbing already exists for
`testfixtures/echo-mcp/` ([cmd/overseer-adapter-mcp/testfixtures/echo-mcp/](../cmd/overseer-adapter-mcp/testfixtures/echo-mcp/));
mirror that pattern.

### Step 2 — Build the fake

`cmd/overseer-adapter-copilot/testfixtures/fake-copilot/main.go`
implements the minimum subset of the `copilot` CLI behavior the
tests need: read prompts from stdin, emit responses on stdout in
the expected JSON / streaming format, exit 0 on clean shutdown.

The fake is **deterministic** — given a recorded prompt sequence,
it returns a recorded response sequence. The conformance test
rewinds and replays this every run.

### Step 3 — Wire into the test

`cmd/overseer-adapter-copilot/conformance_test.go`:

- Default path: build the fake at `TestMain` time, set
  `OVERSEER_COPILOT_BIN` to the fake binary, run the suite. No
  external dependency.
- Real-CLI path: if `COPILOT_E2E=1` is set, skip the fake and use
  whatever's at `OVERSEER_COPILOT_BIN` or `copilot` on PATH —
  preserving today's behavior for local end-to-end runs against a
  real install.

Drop the test-skip when `COPILOT_E2E=1` is unset; the fake covers
that case now.

### Step 4 — CI

The default `make test` lane now runs Copilot conformance against
the fake. No new CI step is needed — the test joins `go test ./...`.

Optional: add a separate `copilot-e2e` job (manual `workflow_dispatch`
or scheduled) that runs the suite against the real CLI. Out of
scope for this workstream unless trivial.

## Out of scope

- Re-recording the prompt/response fixtures against a newer Copilot
  CLI version. The fake covers what the tests already exercise; if
  the real CLI evolves, the manual `COPILOT_E2E=1` lane catches it.
- Any change to the Copilot adapter's production behavior.
- A network-replay layer (e.g., go-vcr-style cassettes). The fake
  binary is simpler.

## Files this workstream may modify

- `cmd/overseer-adapter-copilot/conformance_test.go`
- `cmd/overseer-adapter-copilot/testfixtures/fake-copilot/` (new)
- Any helper added under `cmd/overseer-adapter-copilot/` to wire
  the fake.
- `Makefile` (if a new test-build hook is needed; unlikely).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
or other workstream files.

## Tasks

- [ ] Author the fake binary under `testfixtures/fake-copilot/`.
- [ ] Update `conformance_test.go` to default to the fake; preserve
      the `COPILOT_E2E=1` path for the real CLI.
- [ ] Verify `make test` runs the Copilot conformance suite by
      default (no env var) and that it passes.
- [ ] Verify `COPILOT_E2E=1 make test` still routes through the real
      CLI when one is on PATH.

## Exit criteria

- `make test` exercises Copilot conformance without any env var or
  external CLI.
- The conformance assertions are unchanged in semantic strength
  (the fake doesn't degrade what the tests check).
- `COPILOT_E2E=1` continues to work for local real-CLI validation.

## Tests

The conformance suite itself; no new tests beyond the fake's own
small unit tests (e.g., that the fake parses its recorded fixture
file correctly).

## Risks

| Risk | Mitigation |
|---|---|
| Fake diverges from real CLI behavior over time | Keep the fake's behavior set narrow; add a CI job (cron or manual) that runs `COPILOT_E2E=1` against the real CLI weekly. Document in the workstream's reviewer notes. |
| Fake fixtures become a bug-magnet (large, brittle, drift between PRs) | Keep the fixtures small. If they grow past a few hundred lines, that's a signal the conformance suite is over-fitting to one specific CLI version — push back on the test rather than the fake. |
| `COPILOT_E2E=1` regresses silently (the codepath becomes dead) | The fake-vs-real branching is one `os.Getenv` call; keep it readable. Add a single test that sets `COPILOT_E2E=1`, points at a stub binary that prints "real path", and asserts the stub got invoked. |
