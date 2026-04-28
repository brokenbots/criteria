# Workstream 6 — Coverage, benchmarks, GoDoc

**Owner:** Workstream executor · **Depends on:** [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md), [W04](04-split-oversized-files.md) · **Unblocks:** [W10 Phase 1 cleanup gate](10-phase1-cleanup-gate.md) (which gates `v0.2.0` on the coverage and GoDoc thresholds set here).

## Context

The Phase 0 tech evaluation surfaces three measurable quality gaps
that this workstream closes:

- **CLI coverage at 42%** ([internal/cli/](../internal/cli/)) and
  **`internal/run/` at 48%** — the thinnest-tested code paths in
  the repo, both touching crash recovery and server-mode resume.
- **`cmd/criteria-adapter-mcp` at 0%** — only exercised via
  conformance integration, no unit tests.
- **No benchmarks anywhere.** Performance claims in the README
  ("suitable for local dev workflows") are unvalidated.
- **Spotty GoDoc on exported symbols.** [W02](02-golangci-lint-adoption.md)'s
  `revive`/`exported` rule baselined a long suppression list at
  the start of Phase 1; this workstream burns the list down for
  the public packages.

This workstream is the **measurement and lock-in** workstream. It
does not add new features or change behavior. It adds tests against
existing behavior, baseline benchmarks against existing
implementations, and doc comments against existing exported
symbols. The cleanup gate ([W10](10-phase1-cleanup-gate.md)) gates
`v0.2.0` on the numeric thresholds defined here.

## Prerequisites

- [W02](02-golangci-lint-adoption.md), [W03](03-god-function-refactor.md),
  [W04](04-split-oversized-files.md) merged. Without W03/W04 the
  refactored functions are not stable targets for new tests; with
  them, the seams for unit testing are clear.
- `make ci` green on `main`.

## In scope

### Step 1 — Raise CLI test coverage to ≥ 60%

The W03 refactor of `resumeOneRun` and `runApplyServer` produced
testable seams. Add unit tests for:

- `buildRecoveryClient` (W03-extracted): every failure path
  (missing credentials, `NewClient` error, `SetCredentials` no-op
  when already credentialed). Each test asserts the matching
  log line and that `RemoveStepCheckpoint` was called.
- `attemptReattach` (W03-extracted): RPC error → checkpoint
  removed; `CanResume = false` → checkpoint removed; success →
  response returned unchanged.
- `loadCheckpointWorkflow` (W03-extracted): file missing,
  unparseable HCL, valid HCL → graph returned.
- `resumePausedRun` and `resumeActiveRun` (W03-extracted): table
  test with fake server-transport client; assert the correct
  `WithPendingSignal` vs straight-resume path.
- `applyClientOptions` (W03-extracted): each TLS mode + CA/cert/key
  combination, including the all-empty default.
- `buildServerSink` (W03-extracted): assert `CheckpointFn` writes
  a checkpoint with the expected fields.

Use a fake `servertrans.Client` interface where the existing code
takes a concrete type — introduce a minimal interface in
`internal/cli/` (not in `internal/transport/server/`) that the
test fake implements. Do **not** add the interface to the
production transport package; this is a test-only seam.

Coverage gate: `go test -coverprofile cover.out ./internal/cli/...`
reports ≥ 60% for the package as a whole. Document the exact
percentage in reviewer notes.

### Step 2 — Add unit tests for `cmd/criteria-adapter-mcp`

The MCP adapter currently only has a conformance test
([cmd/criteria-adapter-mcp/conformance_test.go](../cmd/criteria-adapter-mcp/conformance_test.go),
if present). Add a `cmd/criteria-adapter-mcp/mcp_internal_test.go`
that exercises:

- `Info` returns the expected `ConfigSchema` / `InputSchema`
  shapes (table-driven).
- `OpenSession` round-trip with a mock MCP server (in-process,
  no network) — opens, sends a basic tool call, closes cleanly.
- `Execute` with a basic prompt → assert the resulting events
  ordering.
- Error paths: malformed config, server connection failure,
  timeout.

Coverage gate: `go test -coverprofile cover.out ./cmd/criteria-adapter-mcp/...`
reports ≥ 50% (lower bar than CLI because the conformance suite
provides external coverage).

### Step 3 — Raise `internal/run/` coverage to ≥ 60%

The `internal/run/` package contains the server-mode `Sink`
implementation. The 48% number comes from untested resume +
checkpoint paths. Add tests for:

- `Sink.OnRunFailed`, `Sink.OnRunCompleted`: assert the correct
  envelope is published and `CheckpointFn` is or is not called
  per contract.
- `Sink` under `Client.Publish` failure (in-memory fake that
  refuses publish): assert the error is propagated and the run
  is marked failed.
- Checkpoint write failures (fake `WriteStepCheckpoint`): assert
  the run continues but logs a warning.

Coverage gate: ≥ 60% for the package.

### Step 4 — Add baseline benchmarks

Add `*_bench_test.go` files measuring three critical paths:

#### 4.1 `workflow.Compile` benchmark

`workflow/compile_bench_test.go`:

```go
func BenchmarkCompile(b *testing.B) {
    src := mustReadFile("../examples/perf_1000_logs.hcl")
    schemas := makeBenchmarkSchemas()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        spec, _ := Parse("perf.hcl", src)
        _, _ = Compile(spec, schemas)
    }
}
```

If `examples/perf_1000_logs.hcl` does not exist, generate it
deterministically inside the benchmark (1000 sequential
`log` steps), or commit a fixture under
`workflow/testdata/perf_1000_logs.hcl`. Prefer the committed
fixture so the benchmark is reproducible across machines.

#### 4.2 Engine run benchmark

`internal/engine/engine_bench_test.go`:

```go
func BenchmarkEngineRun100Steps(b *testing.B) { ... }
func BenchmarkEngineRun1000Steps(b *testing.B) { ... }
```

Use a fake noop adapter (no plugin spin-up) so the benchmark
measures engine throughput, not plugin-process overhead.

#### 4.3 Plugin Execute benchmark

`internal/plugin/execute_bench_test.go`:

```go
func BenchmarkPluginExecuteNoop(b *testing.B) { ... }
```

Spins up the noop adapter once (`b.ResetTimer()` after spin-up)
and measures Execute throughput. Captures the per-Execute
overhead of the plugin protocol.

#### 4.4 Baseline document

Author **`docs/perf/baseline-v0.2.0.md`** capturing:

- The exact hardware / OS / Go version / commit hash where the
  baselines were measured.
- The numbers from each benchmark (`go test -bench=. -benchmem`).
- A statement of intent: regressions > 20% on any of these
  baselines should fail review until justified.

The doc is the lock-in. Subsequent workstreams that change a
hot path are expected to re-run the benchmarks and update the
doc; non-regression is a soft gate, not CI-enforced (CI
benchmarks are too noisy to gate on).

### Step 5 — Burn down `revive`/`exported` GoDoc baseline entries

The `.golangci.baseline.yml` from W02 quarantined every
`revive`/`exported` finding. Burn the list down to zero **for
public packages only**:

- `sdk/` (entire module — public)
- `workflow/` (public Go API consumed by the SDK)
- `events/` (public ND-JSON event types)
- `cmd/criteria/...` (the CLI binary's exported symbols, where
  they exist)

For each `revive`/`exported` baseline entry in those packages:

- Add a short, accurate doc comment (one sentence; ≤ 120 chars)
  to the symbol.
- Delete the matching `.golangci.baseline.yml` entry.
- Verify `make lint-go` exits 0.

For `internal/...` packages, **leave** the baseline entries in
place unless they're trivially fixable while testing in Steps
1–3. Internal packages do not need full GoDoc; the cleanup
gate ([W10](10-phase1-cleanup-gate.md)) records the residual
count as a Phase 2 backlog item.

Doc comment style:

- Start with the symbol name (Go convention; `revive` enforces
  this).
- One sentence describing what it is or what it does. Avoid
  restating the type signature.
- For interfaces, name the contract obligation (e.g. "Close
  releases all resources held by the client and is safe to
  call multiple times.").

Example:

```go
// Compile lowers an HCL Spec into a validated FSMGraph using the
// provided adapter schemas for input and config validation. It
// returns hcl.Diagnostics for every error encountered; callers
// should check Diagnostics.HasErrors before using the graph.
func Compile(spec *Spec, schemas map[string]AdapterInfo) (*FSMGraph, hcl.Diagnostics) {
```

### Step 6 — Wire coverage and benchmark targets

Add to `Makefile`:

```makefile
test-cover: ## Run tests with coverage; outputs cover.out
	go test -race -coverprofile=cover.out -covermode=atomic ./...
	cd sdk      && go test -race -coverprofile=cover.out -covermode=atomic ./...
	cd workflow && go test -race -coverprofile=cover.out -covermode=atomic ./...

bench: ## Run all benchmarks (slow)
	go test -bench=. -benchmem -run=^$ ./...
	cd sdk      && go test -bench=. -benchmem -run=^$ ./...
	cd workflow && go test -bench=. -benchmem -run=^$ ./...
```

Add `test-cover` to the `.PHONY` list and to `make help` output.
Do **not** add `bench` to `make ci` — benchmarks are too noisy
for CI gating.

`test-cover` is **not** added to CI either; coverage measurement
in CI is a Phase 2 nice-to-have. Phase 1 enforces the thresholds
manually at the cleanup gate by running `make test-cover` once
and inspecting per-package coverage.

## Out of scope

- Adding tests for new behavior. This workstream tests existing
  behavior only.
- Optimizing performance based on benchmark results. The
  benchmarks are a baseline; optimizations are Phase 2 work.
- Adding GoDoc to `internal/...` packages beyond what's trivially
  fixable while in the file. Internal-only doc coverage is a
  Phase 2 nice-to-have.
- CI-gating coverage or benchmarks. The thresholds are documented
  here and enforced manually by [W10](10-phase1-cleanup-gate.md).
- Adding test infrastructure (testify, gomock, etc.). Stick to
  the standard library + the existing fake patterns in the
  codebase.
- Replacing the existing conformance suite. New unit tests
  complement, not replace, conformance.

## Files this workstream may modify

**Created:**

- `internal/cli/reattach_test.go` (extend, not rewrite — file
  may already exist; add new tests)
- `internal/cli/apply_test.go` (extend; add tests for extracted
  helpers)
- `internal/run/sink_test.go` (extend or create)
- `cmd/criteria-adapter-mcp/mcp_internal_test.go`
- `workflow/compile_bench_test.go`
- `workflow/testdata/perf_1000_logs.hcl` (if not present)
- `internal/engine/engine_bench_test.go`
- `internal/plugin/execute_bench_test.go`
- `docs/perf/baseline-v0.2.0.md`

**Modified:**

- Files in `sdk/`, `workflow/`, `events/`, and `cmd/criteria/`
  to add doc comments to currently-undocumented exported
  symbols.
- `Makefile` (add `test-cover`, `bench`, update `.PHONY`).
- `.golangci.baseline.yml` (delete `revive`/`exported` entries
  pointed at this workstream for public packages).

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any
other workstream file. It may **not** add new features or
change behavior of any production code path.

## Tasks

- [ ] Add CLI unit tests per Step 1; verify ≥ 60% coverage.
- [ ] Add MCP adapter unit tests per Step 2; verify ≥ 50%
      coverage.
- [ ] Add `internal/run/` tests per Step 3; verify ≥ 60%
      coverage.
- [ ] Add three benchmark suites per Step 4.
- [ ] Author `docs/perf/baseline-v0.2.0.md` with measured
      numbers.
- [ ] Add doc comments per Step 5 for public-package symbols.
- [ ] Burn matching `.golangci.baseline.yml` entries (public
      packages only).
- [ ] Add `make test-cover` and `make bench` targets.
- [ ] `make ci` green; `make lint-go` green; `make test-cover`
      reports the per-package thresholds met.
- [ ] `make bench` runs to completion locally.

## Exit criteria

- Coverage thresholds met (per `make test-cover`):
  - `internal/cli/...` ≥ 60%
  - `internal/run/...` ≥ 60%
  - `cmd/criteria-adapter-mcp/...` ≥ 50%
  - All other packages: no regression vs `main` baseline.
- Three benchmark files exist, run to completion, and produce
  numbers recorded in `docs/perf/baseline-v0.2.0.md`.
- `.golangci.baseline.yml` has zero `revive`/`exported`
  entries pointing at `sdk/`, `workflow/`, `events/`, or
  `cmd/criteria/`.
- `make ci`, `make lint-go`, `make test-cover` all exit 0.
- `make bench` runs to completion (numbers vary; correctness is
  the gate).
- Reviewer notes capture the actual coverage percentages and
  benchmark numbers verbatim.

## Tests

This workstream **is** the test workstream — every test added
here is on the workstream-itself ledger. Quality bar:

- Tests must validate behavior, not implementation. The reviewer
  rubric in
  [.github/agents/workstream-reviewer.agent.md](../.github/agents/workstream-reviewer.agent.md)
  applies in full.
- Tests must be deterministic and `-race`-clean. No timing
  sleeps; use channels and `t.Cleanup`.
- Coverage padding (tests that exist only to hit lines) is
  rejected. Reviewer must be able to articulate what each test
  defends against.

## Risks

| Risk | Mitigation |
|---|---|
| Coverage thresholds tempt the executor to write padding tests | The reviewer rubric explicitly rejects "test passes" as the bar. The threshold is a floor, not a ceiling, and each test must defend against a plausible regression. Reviewer notes must articulate that defense. |
| Benchmarks are too noisy to be useful baselines | Phase 1 records the numbers but does not CI-gate on them. The doc explicitly marks regression-detection as a soft gate. Phase 2 may invest in benchstat-based statistical comparison. |
| GoDoc burn-down balloons into broad rewrites of every public symbol | Step 5 caps at one-sentence comments ≤ 120 chars. Reviewer rejects multi-paragraph docstrings; those are scope creep. |
| New test seams (the test-only `servertrans.Client` interface) leak into production code | The interface lives in `internal/cli/` (the consumer), not in the transport package. Reviewer rejects any new exported test seams in `internal/transport/server/`. |
| Benchmarks depend on machine-specific timings and become brittle | The baseline doc captures hardware/OS/Go-version/commit-hash; future workstreams running on different hardware re-baseline. The 20% regression threshold is documented as guidance, not policy. |
| `internal/run/` coverage push exposes a latent bug | Fix the bug in this workstream **only if** the fix is mechanical (≤ 5 lines); larger fixes go to a Phase 2 forward-pointer with `[ARCH-REVIEW]` and the test marks the path as `t.Skip` with the pointer. Do not silently leave the bug uncovered. |
| The MCP adapter's mock server fixture becomes its own maintenance burden | Cap the in-process MCP server at ~150 LOC. If it grows beyond, switch to a documented-skip strategy and rely on conformance for that path. |
| Burning the `revive`/`exported` baseline entries reveals genuinely-confusing exports that should be unexported | Note them in `[ARCH-REVIEW]` rather than fixing in this workstream. Public API breaking changes are out of scope here and require deliberate Phase 2 deprecation. |
