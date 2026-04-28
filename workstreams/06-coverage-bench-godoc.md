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

- [x] Add CLI unit tests per Step 1; verify ≥ 60% coverage.
- [x] Add MCP adapter unit tests per Step 2; verify ≥ 50%
      coverage.
- [x] Add `internal/run/` tests per Step 3; verify ≥ 60%
      coverage.
- [x] Add three benchmark suites per Step 4.
- [x] Author `docs/perf/baseline-v0.2.0.md` with measured
      numbers.
- [x] Add doc comments per Step 5 for public-package symbols.
- [x] Burn matching `.golangci.baseline.yml` entries (public
      packages only).
- [x] Add `make test-cover` and `make bench` targets.
- [x] `make ci` green; `make lint-go` green; `make test-cover`
      reports the per-package thresholds met.
- [x] `make bench` runs to completion locally.

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

## Reviewer Notes

### Coverage results (measured with `make test-cover`)

| Package | Coverage | Target | Status |
|---|---:|---:|---|
| `internal/cli/` | 65.9% | ≥60% | ✅ (raised from 60.0% after B1 tests) |
| `internal/run/` | 77.8% | ≥60% | ✅ |
| `cmd/criteria-adapter-mcp/` | 82.4% | ≥50% | ✅ |

Key reattach function coverage after B1 remediation:
- `attemptReattach`: 100%
- `resumePausedRun`: 73.3%
- `resumeActiveRun`: 77.8%
- `drainAndCleanup`: 100%

### Benchmark baseline (Apple M3 Max, arm64/darwin, go1.26.2, commit e890474, `make bench`)

**Workflow compile:**

| Benchmark | ns/op | allocs/op |
|---|---:|---:|
| `BenchmarkCompile_Hello` | 68,115 | 942 |
| `BenchmarkCompile_1000Steps` | 33,163,892 | 389,695 |
| `BenchmarkCompile_WorkstreamLoop` | 1,605,975 | 13,902 |

**Engine run (fake noop adapter, no plugin overhead):**

| Benchmark | ns/op | allocs/op |
|---|---:|---:|
| `BenchmarkEngineRun_10Steps` | 12,325 | 268 |
| `BenchmarkEngineRun_100Steps` | 123,252 | 2,608 |
| `BenchmarkEngineRun_1000Steps` | 1,414,919 | 26,008 |

**Plugin execution:**

| Benchmark | ns/op | allocs/op |
|---|---:|---:|
| `BenchmarkBuiltinPlugin_Execute` (shell/`true`) | 11,146,722 | 110 |
| `BenchmarkPluginExecuteNoop` (in-process, session-once) | 8.386 | 0 |
| `BenchmarkBuiltinPlugin_Info` | 231.6 | 4 |
| `BenchmarkLoaderResolveBuiltin` | 43.26 | 2 |

Full details and regression policy in `docs/perf/baseline-v0.2.0.md`.

### Step 5 (GoDoc burn-down) — no entries

All `.golangci.baseline.yml` entries are `var-naming` suppressions for
proto-generated code aliases in `sdk/pb/criteria/v1/`. There are **zero**
`revive`/`exported` entries for public packages (`sdk/`, `workflow/`,
`events/`, `cmd/criteria/`). Step 5 is a no-op — the baseline was clean
before this workstream started.

### Remediation notes (Review 2 response)

- **B1 — `attemptReattach`/`resumePausedRun`/`resumeActiveRun`**: Introduced `reattachTransport` interface in `internal/cli/reattach.go`; changed function signatures; changed `run.Sink.Client` to `Publisher` interface (minimal: only `Publish`). `executeServerRun` in `apply.go` was updated to receive `*servertrans.Client` as a separate parameter (avoids promoting transport methods into `Publisher`). Added `fakeTransport` in `reattach_test.go` implementing the interface. Added 7 new tests covering all specified branches.
- **B2 — `BenchmarkCompile_Perf1000Logs`**: Replaced with `BenchmarkCompile_1000Steps` using in-memory generated HCL with 1 000 sequential step nodes. New allocation count is 389,695 (vs 942 for Hello), confirming the benchmark exercises the compiler at scale.
- **B3 — Baseline doc**: Added Go version (`go1.26.2`), commit hash (`e890474`), and verbatim 20% regression statement.
- **R1 — CheckpointFn negative assertion**: Added `TestSink_CheckpointFn_NotCalledOnTerminalEvents` asserting the flag is NOT set after `OnRunCompleted` and `OnRunFailed`.
- **R2 — `-race` in `test-cover`**: Restored; target now runs `-race -coverprofile`.
- **R3 — `bench` target scope**: Documented deviation in `docs/perf/baseline-v0.2.0.md`. The `bench` target runs targeted packages instead of `./...` to avoid triggering `TestMain` setup in packages with no benchmarks (notably `cmd/criteria-adapter-mcp`).
- **R4 — `BenchmarkPluginExecuteNoop`**: Added with `noopAdapter` (in-process, zero allocs). Session opened once before `b.ResetTimer()`; Execute called N times. Measures 8.386 ns/op (pure dispatch) vs ~11 ms for subprocess spawn.
- **R5 — Dead `time` import**: Removed `time` import and `var _ = time.Second` from `execute_bench_test.go`.
- **WEAK1 — `TestMCPBridge_FullRoundTrip` event ordering**: Now asserts the last event is a `Result` event (not just that any result exists), enforcing the ordering contract.

### Architecture Review Required

**[ARCH-REVIEW / major] — Step 3 publish-failure and checkpoint-write-failure**

The plan's Step 3 requires "Sink under Client.Publish failure: assert the error is propagated and the run is marked failed." and "checkpoint write failure: assert run continues but logs a warning." These cannot be tested at the Sink boundary without design changes:

- `Sink.publish()` calls `s.Client.Publish(...)` (now via `Publisher` interface) but captures no return value — the design is fire-and-forget.
- `CheckpointFn` has no error return — checkpoint failures silently drop.

Addressing these would require: (a) changing `Sink.publish` to capture errors and emit `OnRunFailed`, or (b) adding error returns to `CheckpointFn`. Both change production behavior and are out of scope for this measurement workstream. Tracked as Phase 2 item.

### Notable fixes applied

- **HCL2 semicolons** in `reattach_test.go`: `state "done" { terminal = true; success = true }` is invalid HCL2. Fixed to multi-line syntax.
- **`max_step_retries` placement**: must be inside `policy { }` block, not top-level. Fixed in test fixtures.
- **Retry logic off-by-one**: `resumeOneLocalRun` with `Attempt=1` and default `MaxStepRetries=0` hits the retry-exceeded branch (nextAttempt=2 > maxAttempts=1). Fixed to `Attempt=0` for happy-path test.
- **1000-step engine benchmark**: failed with `max_total_steps exceeded (100)` default. Fixed `buildNStepWorkflow` to set `policy { max_total_steps = n+10 }`.
- **Lint nits**: `prealloc` in `sink_test.go`, unused `nolintlint` directives in MCP test, `stringXbytes` in `compile_test.go`, all resolved.

### Validation (Review 2)

- `make test`: all packages pass (race-clean)
- `make lint-go`: exits 0
- `make lint-imports`: exits 0
- `make test-cover`: exits 0; internal/cli: 65.9%, internal/run: 77.8%, mcp: 82.4%
- `make bench`: all 10 benchmarks run to completion

---

### Review 2026-04-28-02 — approved

#### Summary

All three blockers from the first review are fully resolved. `attemptReattach` is now at 100%, `resumePausedRun` at 73.3%, `resumeActiveRun` at 77.8%, and `drainAndCleanup` at 100% — the `reattachTransport` interface was correctly introduced in `internal/cli/` (not in the transport package) and the test fake implements it. `BenchmarkCompile_1000Steps` replaces the previous misleading fixture: 389,695 allocs/op confirms 1000 HCL nodes are compiled. The baseline doc now includes Go version, commit hash, and the verbatim 20% regression statement. All five required remediations (R1–R5) and the MCP ordering weakness are addressed. `make test` (race-clean), `make lint-go`, `make lint-imports`, `make test-cover`, and `make bench` all exit 0. The arch-review item (publish-failure / checkpoint-write-failure untestable without design changes) is correctly documented in the workstream and deferred to Phase 2.

#### Plan Adherence

| Step | Status | Notes |
|---|---|---|
| Step 1 — CLI ≥ 60% | ✅ 65.9% | `attemptReattach` 100%, `resumePausedRun` 73.3%, `resumeActiveRun` 77.8% — all plan-named functions now tested |
| Step 2 — MCP ≥ 50% | ✅ 82.4% | Event ordering now asserted in `TestMCPBridge_FullRoundTrip` |
| Step 3 — `internal/run/` ≥ 60% | ✅ 77.8% | CheckpointFn negative assertion added; arch-review item documented |
| Step 4 — Benchmarks | ✅ | `BenchmarkCompile_1000Steps` correctly stresses compiler (389,695 allocs); `BenchmarkPluginExecuteNoop` 8 ns/op pure dispatch |
| Step 4.4 — Baseline doc | ✅ | Go version, commit hash, 20% threshold all present |
| Step 5 — GoDoc burn-down | ✅ N/A | No `revive`/`exported` entries existed |
| Step 6 — Makefile targets | ✅ | `-race` restored; bench scope deviation documented |

#### Test Intent Assessment

Tests added in this pass that prove behavioral intent:

- `TestAttemptReattach_RPCError`: asserts side-effect (checkpoint removed) and return value (`err != nil`, `resp == nil`) — a faulty implementation that doesn't clear the checkpoint or swallows the error would fail.
- `TestAttemptReattach_NotResumable`: asserts `(nil, nil)` contract and checkpoint removal — a regression that returns the response would fail.
- `TestAttemptReattach_Success`: asserts response payload forwarded unchanged — a regression that mutates the response would fail.
- `TestResumeActiveRun_ExceedsMaxRetries`: asserts a `RunFailed` envelope is published via `ft.published` inspection — a regression that silently drops the failure would fail.
- `TestResumeActiveRun_HappyPath`: asserts `RunCompleted` envelope is published and checkpoint is removed.
- `TestResumePausedRun_StartsStreamsAndRunsEngine`: asserts engine drives to completion and checkpoint is cleaned up.
- `TestResumePausedRun_StartStreamsError`: asserts no engine events are emitted when `StartStreams` fails — prevents accidental event emission on aborted recovery.
- `TestSink_CheckpointFn_NotCalledOnTerminalEvents`: negative assertion — proves the contract that `CheckpointFn` is exclusively an `OnStepEntered` side-effect.

Remaining low-coverage paths that are acceptable (not plan requirements):
- `serviceResumeSignals` 16.7%: the wait-for-resume loop requires a live `ResumeCh` signal; testing would need concurrency scaffolding well beyond this workstream's scope. The happy-path (immediate paused exit) IS covered.
- `resumeOneRun` 0%: outer orchestrator; fully tested via its components individually.

#### Validation Performed

```
make test          → exit 0 (all packages, race-clean)
make lint-go       → exit 0
make lint-imports  → exit 0
make test-cover    → exit 0
  internal/cli/:               65.9%  (target ≥60%) ✅
  internal/run/:               77.8%  (target ≥60%) ✅
  cmd/criteria-adapter-mcp/:   82.4%  (target ≥50%) ✅
go tool cover -func=cover.out (reattach functions):
  attemptReattach   100%  ✅
  drainAndCleanup   100%  ✅
  resumePausedRun    73.3% ✅
  resumeActiveRun    77.8% ✅
make bench         → exit 0; 10 benchmarks (workflow ×3, engine ×3, plugin ×4)
  BenchmarkCompile_1000Steps:  389,695 allocs/op  ← confirms 1000-node compiler stress
  BenchmarkPluginExecuteNoop:    8.371 ns/op, 0 allocs ← confirms session-once dispatch
```



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

## Reviewer Notes

### Review 2026-04-28 — changes-requested

#### Summary

The implementation clears coverage thresholds (CLI 60.0%, run 77.8%, MCP 82.4%), all three benchmark suites produce numbers, the GoDoc burn-down is a no-op (baseline already clean), and `make test`, `make lint-go`, `make bench` all exit 0. However three blockers prevent approval: (1) `attemptReattach`, `resumePausedRun`, and `resumeActiveRun` are at 0% coverage despite being explicitly named as required test targets in Step 1; (2) the `perf_1000_logs.hcl` fixture has one shell step with a runtime loop rather than 1 000 HCL workflow nodes, so `BenchmarkCompile_Perf1000Logs` does not measure what the plan specifies and the baseline numbers are misleading; (3) `docs/perf/baseline-v0.2.0.md` is missing the Go version, commit hash, and the explicit 20 % regression threshold required by Step 4.4. Additionally, several test-intent gaps and Makefile deviations require remediation before approval.

#### Plan Adherence

**Step 1 — CLI coverage ≥ 60%**

Coverage threshold met (60.0%). The following functions are explicitly named in the plan as required test targets and are at 0% coverage:

- `attemptReattach`: 0%. Plan requires: RPC error → checkpoint removed; `CanResume = false` → checkpoint removed; success → response returned unchanged.
- `resumePausedRun`: 0%. Plan requires: table test with fake server-transport client; assert `WithPendingSignal` path.
- `resumeActiveRun`: 0%. Plan requires: table test with fake server-transport client; assert straight-resume path.
- `resumeOneRun`, `drainAndCleanup`, `serviceResumeSignals`: 0% (depend on same seam).

The plan was explicit: "Use a fake `servertrans.Client` interface where the existing code takes a concrete type — introduce a minimal interface in `internal/cli/` (not in `internal/transport/server/`) that the test fake implements." This test-only interface was never introduced.

Covered as required: `buildRecoveryClient`, `loadCheckpointWorkflow`, `abandonCheckpoint`, `applyClientOptions`, `buildServerSink`/`CheckpointFn`. ✅

**Step 2 — MCP adapter ≥ 50%**

Coverage 82.4%. `Info`, `OpenSession` error paths, `Execute` unknown session, `CloseSession` unknown session, `FullRoundTrip`, `UnknownTool`, `MissingTool` are all present. Minor intent gap noted in Required Remediations. ✅ (threshold)

**Step 3 — `internal/run/` ≥ 60%**

Coverage 77.8%. Threshold met. However the plan's specific behavioral assertions are not present:

- `Sink.OnRunFailed`/`Sink.OnRunCompleted`: plan says "assert the correct envelope is published and `CheckpointFn` is or is not called per contract." Tests only assert no panic; no assertion that `CheckpointFn` is NOT called on these terminal events.
- Publish failure / checkpoint write failure paths: see Architecture Review Required section.

**Step 4 — Benchmarks**

4.1 `BenchmarkCompile_Hello` and `BenchmarkCompile_WorkstreamLoop` are valid. **`BenchmarkCompile_Perf1000Logs` is invalid**: the fixture (`examples/perf_1000_logs.hcl`) has a single shell step with a runtime loop, not 1 000 sequential HCL workflow nodes. The plan explicitly requires "1 000 sequential `log` steps" to stress the compiler. Evidence: `BenchmarkCompile_Hello` allocates 942 allocs/op; `BenchmarkCompile_Perf1000Logs` allocates 956 allocs/op — a delta of 14, confirming there is only one workflow node in the fixture. A proper 1 000-node workflow would show thousands of additional allocations.

4.2 Engine benchmarks (10/100/1000 steps) are correct and use the fake noop adapter. ✅

4.3 Plugin benchmark uses the shell adapter (not the noop adapter as specified) and spins up a full session on every iteration instead of once before `b.ResetTimer()`. The comment describes the intent as "full per-step dispatch cost" which is different from the plan's "spin up once, measure Execute throughput." Numbers are interesting but the benchmark does not implement what the plan specified.

4.4 `docs/perf/baseline-v0.2.0.md` is missing: Go version, commit hash, and the explicit "regressions > 20% should fail review" statement.

**Step 5 — GoDoc burn-down**

No-op; executor correctly determined no `revive`/`exported` entries exist. ✅

**Step 6 — Makefile targets**

`test-cover` and `bench` targets added; `.PHONY` updated. However:
- `test-cover` drops `-race` (plan spec includes `-race`).
- `bench` runs only 3 targeted packages, not `./...` + sdk + workflow per plan spec; adds undocumented `-benchtime=3s`.

#### Required Remediations

- **[BLOCKER] B1 — Missing tests for `attemptReattach`, `resumePausedRun`, `resumeActiveRun`**
  - *File*: `internal/cli/reattach.go` / `internal/cli/reattach_test.go`
  - *Rationale*: Explicitly required by Step 1. These are the crash-recovery hot paths. The test-only interface described in the plan was never introduced.
  - *Acceptance*: Introduce a minimal interface in `internal/cli/` (e.g., `reattachTransport` or similar) that `*servertrans.Client` satisfies. Implement a fake that records calls and returns configurable responses. Add tests for:
    - `attemptReattach`: (a) RPC error → checkpoint removed, error returned; (b) `CanResume = false` → checkpoint removed, `(nil, nil)` returned; (c) success → response returned unchanged.
    - `resumeActiveRun`: (a) nextAttempt ≤ maxAttempts → streams started, `OnStepResumed` called, engine runs; (b) nextAttempt > maxAttempts → `OnRunFailed` called, checkpoint removed.
    - `resumePausedRun`: streams started, `WithPendingSignal` passed to engine, checkpoint removed on completion.
  - The interface must stay in `internal/cli/` and must not be exported to `internal/transport/server/`.

- **[BLOCKER] B2 — `perf_1000_logs.hcl` fixture has 1 step, not 1 000 nodes**
  - *File*: `workflow/compile_bench_test.go`, `examples/perf_1000_logs.hcl`
  - *Rationale*: `BenchmarkCompile_Perf1000Logs` allocates 956 allocs/op vs `BenchmarkCompile_Hello`'s 942 — a delta of 14. The fixture does not stress the compiler. The plan requires "1 000 sequential `log` steps" (HCL nodes, not shell lines).
  - *Acceptance*: Either (a) commit `workflow/testdata/perf_1000_logs.hcl` containing 1 000 sequential HCL `step` nodes (using the `noop` adapter or `shell` with `echo`), update the benchmark to read from `workflow/testdata/`, and re-capture baseline numbers; or (b) rename the benchmark to `BenchmarkCompile_SingleShellStep` and add a new `BenchmarkCompile_1000Steps` benchmark using an in-memory generated HCL string with 1 000 steps. Re-capture and update `docs/perf/baseline-v0.2.0.md`.

- **[BLOCKER] B3 — Baseline doc missing Go version, commit hash, and 20% threshold statement**
  - *File*: `docs/perf/baseline-v0.2.0.md`
  - *Rationale*: Step 4.4 explicitly requires these three items.
  - *Acceptance*: Add Go version (output of `go version`), commit hash (output of `git rev-parse HEAD`), and the verbatim statement: "Regressions > 20% on any of these baselines should fail review until justified."

- **[REQUIRED] R1 — `Sink.OnRunFailed`/`Sink.OnRunCompleted` missing CheckpointFn negative assertion**
  - *File*: `internal/run/sink_test.go`
  - *Rationale*: Step 3 requires "assert `CheckpointFn` is or is not called per contract." `TestSink_CheckpointFnCalledOnStepEntered` proves it IS called on step entry, but there is no test proving it is NOT called on run completion or failure.
  - *Acceptance*: Add a test that sets `s.CheckpointFn` to a function that sets a flag, calls `s.OnRunCompleted(...)` and `s.OnRunFailed(...)`, and asserts the flag was NOT set.

- **[REQUIRED] R2 — `test-cover` drops `-race` without plan justification**
  - *File*: `Makefile`
  - *Rationale*: The plan's `test-cover` spec explicitly includes `-race`. The deviation is undocumented in the plan; the comment says "no -race to keep it fast" but this was not an approved deviation.
  - *Acceptance*: Restore `-race` in the `test-cover` target, or obtain explicit plan approval for the omission and document it in the workstream notes. If restoring `-race` causes a runtime penalty that is unacceptable, add a note here in the reviewer section explaining the trade-off and get it approved.

- **[REQUIRED] R3 — `bench` target does not match plan spec**
  - *File*: `Makefile`
  - *Rationale*: Plan says `go test -bench=. -benchmem -run=^$ ./...` then SDK then workflow. Actual targets only 3 specific packages and adds undocumented `-benchtime=3s`.
  - *Acceptance*: Either align the `bench` target with the plan (run `./...` then `cd sdk && ...` then `cd workflow && ...`), or document the deviation in these reviewer notes with justification and update the workstream.

- **[REQUIRED] R4 — Plugin benchmark (4.3) deviates from plan spec**
  - *File*: `internal/plugin/execute_bench_test.go`
  - *Rationale*: Plan: "Spins up the noop adapter once (`b.ResetTimer()` after spin-up) and measures Execute throughput." Actual: spins up the shell adapter and creates a new session on every iteration. These measure different things.
  - *Acceptance*: Add `BenchmarkPluginExecuteNoop` that opens one session before `b.ResetTimer()`, then calls `Execute` in the loop, then closes after the loop. Keep the existing `BenchmarkBuiltinPlugin_Execute` (renamed appropriately) if you wish to preserve the "full per-step dispatch cost" measurement as a second benchmark.

- **[NIT] R5 — Dead `var _ = time.Second` in `execute_bench_test.go`**
  - *File*: `internal/plugin/execute_bench_test.go` line 89
  - *Rationale*: The `time` package is not used in the file except via this sentinel. The comment is incorrect — there is no interface signature check for time in this file.
  - *Acceptance*: Remove the `time` import and the `var _ = time.Second` line.

#### Test Intent Assessment

**Strong tests:**
- `TestParseCSVList`, `TestParseEnvPairs`: table-driven, cover all branches including boundary/error cases. Any mis-implementation of parse logic would fail them.
- `TestBuildRecoveryClient_MissingCredentials`, `TestBuildRecoveryClient_BadServerURL`: verify the correct checkpoint removal side-effect, not just the error return.
- `TestResumeOneLocalRun_ExceedsMaxRetries`: verifies ND-JSON output contains `RunFailed` — behavior-asserting, not just "no panic."
- `TestSink_CheckpointFnCalledOnStepEntered`: verifies the step/attempt forwarding contract.
- `TestEncodeAdapterData_*`: table-driven, cover object/scalar/array/error cases; cover the `_encode_error` field contract.
- `TestLogStreamFromString`: table-driven enum mapping — regression-sensitive.
- Engine benchmarks (`BenchmarkEngineRun_10/100/1000Steps`): proper fake adapter, no plugin process overhead.

**Weak or missing tests (require remediation):**
- `TestSink_PublishMethodsDoNotPanic`: a smoke test, not a behavioral test. The plan requires asserting that `CheckpointFn` is NOT called on terminal events and that the correct envelope type is published — neither is asserted.
- `TestSink_PublishAfterClientClose_DoesNotPanic`: tests that the fire-and-forget design doesn't panic, which is correct given the architecture. But the plan's "assert the error is propagated" intent cannot be satisfied without design changes (see Architecture Review Required).
- `TestMCPBridge_FullRoundTrip`: verifies a result event exists but does not check event ordering, which the plan lists as a requirement ("assert the resulting events ordering").
- `BenchmarkCompile_Perf1000Logs`: does not measure what it claims (see B2 above).

#### Architecture Review Required

- **[ARCH-REVIEW / major] — Step 3 publish-failure and checkpoint-write-failure test requirements conflict with fire-and-forget Sink design**
  - *Affected files*: `internal/run/sink.go`, plan Step 3
  - *Problem*: The plan requires "Sink under `Client.Publish` failure: assert the error is propagated and the run is marked failed." The `Sink.publish()` method calls `s.Client.Publish(...)` without capturing or surfacing the return value — the design is intentionally fire-and-forget. Error propagation from the transport layer to the `Sink` caller is not architecturally supported. Similarly, `CheckpointFn` has no error return, so "checkpoint write failure: assert run continues but logs a warning" cannot be tested at the Sink level without a design change.
  - *Why arch-review*: Addressing these test requirements requires either (a) changing `Sink.publish` to capture publish errors and take some action (changed behavior, out of scope for W06), or (b) accepting that these behaviors cannot be unit-tested at the Sink boundary and are instead covered by integration/conformance tests. A decision on whether to change the Sink design or formally accept the gap is needed before W06 can close Step 3 fully.
  - *Suggested resolution*: Document in the workstream that these two paths are not unit-testable without Sink design changes, mark them as Phase 2 items, and adjust the Step 3 test requirement text accordingly.

#### Validation Performed

```
make test          → exit 0 (all packages pass, race-clean, cached)
make lint-go       → exit 0
make lint-imports  → exit 0
make test-cover    → exit 0; internal/cli: 60.0%, internal/run: 77.8%, cmd/criteria-adapter-mcp: 82.4%
make bench         → exit 0; 9 benchmarks produce numbers
go tool cover -func=cover.out | grep internal/cli/reattach
  → attemptReattach: 0%, resumePausedRun: 0%, resumeActiveRun: 0%, resumeOneRun: 0%
BenchmarkCompile_Hello:       942 allocs/op
BenchmarkCompile_Perf1000Logs: 956 allocs/op  ← confirms fixture is not a 1000-node workflow
```
