# Technical Evaluation — Criteria v0.2.0

**Date:** 2026-04-29
**Evaluator:** AI Technical Evaluator
**Prior evaluation:** [TECH_EVALUATION-20260427-01.md](TECH_EVALUATION-20260427-01.md) (v0.1.0, verdict: MARGINAL)
**Codebase:** ~16,236 LOC production + ~15,907 LOC tests (~0.98 test:prod ratio)
**Tag:** `v0.2.0` — Phase 1 closed 2026-04-29

---

## Executive Summary

Phase 1 substantively addressed every code-quality and security blocker raised in the prior evaluation. Tests now pass deterministically with `-race`, the worst god-functions are decomposed into <=50-line helpers, the shell adapter ships a real first-pass sandbox (env allowlist, PATH sanitization, working-dir confinement, hard timeout, output cap), `golangci-lint` is wired with a per-workstream burn-down contract, benchmarks have a documented baseline, and four user-blocking issues shipped. **The verdict moves from MARGINAL to VIABLE.** What remains is organizational, not technical: a bus factor of one, a 240-entry lint baseline that is parked rather than burned down, and a Phase 2 plan that is still TBD.

---

## Grade Card

| Area | Prior | Now | Justification |
|------|-------|-----|---------------|
| Architecture | B+ | B+ | Same clean FSM + plugin model. W10 step-level iteration was a real language change executed cleanly. |
| Code Quality | D+ | B | God-functions decomposed (longest non-iteration fn now ~72 lines). One large file (copilot.go, 793 LOC) but its functions are short. |
| Test Quality | C | B+ | make test -race -count=1 clean across all packages. Coverage gates in place. CLI 65.6% (was 42%). MCP 82.4% (was 0%). |
| Documentation | B | B+ | Threat model for shell, perf baseline, lint-baseline contract, GoDoc on public packages. README and CHANGELOG honest. |
| Security | C+ | B | Shell sandbox shipped with documented threat model and time-boxed legacy escape hatch. govulncheck clean. State-dir perms a minor finding. |
| Maintainability | D | C+ | Code is readable now. Bus-factor risk unchanged: 133/137 commits in 6mo by one human. |
| Tech Debt | C- | C | Net debt is lower but 240 baselined lint entries and the W04/W10 partial residuals are real, parked debt. |
| Performance | B | B+ | Documented baselines with regression policy (>20% fails review). Numbers look reasonable. |

---

## Project Description

Criteria is a standalone HCL to FSM workflow execution engine with an out-of-process adapter plugin model and a published Connect/gRPC SDK for orchestrators. Phase 1 was a stabilization phase: harden CI, adopt lint, sandbox shell, and unblock the user-feedback queue.

---

## Current State vs. Stated Goals

### Goals met since prior evaluation

- **Flaky tests fixed.** `make test -race -count=1` is clean across every package; `goleak` is in place; CI runs `-count=2`. The two named flakes (TestEngineLifecycleOpenTimeoutKeepsSessionAlive, TestHandshakeInfo) pass deterministically.
- **God-function refactor.** `resumeOneRun` is now 34 lines and decomposes into `loadCheckpointWorkflow`, `attemptReattach`, `resumePausedRun`, `resumeActiveRun`, `serviceResumeSignals`, `drainAndCleanup` — exactly the structure the prior evaluation prescribed (see [internal/cli/reattach.go](internal/cli/reattach.go)).
- **`copilotPlugin.Execute` refactor.** Now 36 lines ([cmd/criteria-adapter-copilot/copilot.go](cmd/criteria-adapter-copilot/copilot.go#L233)), with `prepareExecute`, `applyRequestEffort`, `applyRequestModel`, `awaitOutcome`, `handleEvent` extracted.
- **`workflow/compile.go` split.** From 1,099 lines to 301 lines plus `compile_steps.go` (476), `compile_variables.go`, `compile_agents.go`, `compile_lifecycle.go`, `compile_validation.go` (292), `compile_nodes.go`. SRP respected.
- **Shell adapter sandbox.** Shipped: env allowlist, PATH sanitization, working-dir confinement under $HOME or CRITERIA_SHELL_ALLOWED_PATHS, default 5-minute timeout (1s-1h), 4 MiB-per-stream output cap, SIGTERM then grace then SIGKILL on timeout. CRITERIA_SHELL_LEGACY=1 opt-out is documented as time-boxed for v0.3.0 removal. Threat model at [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md).
- **CLI test coverage > 60%.** 65.6% (was 42%).
- **golangci-lint adopted** with funlen/gocyclo/gocognit/revive/errorlint/bodyclose plus 14 other linters enabled ([.golangci.yml](.golangci.yml)).
- **Benchmarks shipped.** `engine_bench_test.go`, `compile_bench_test.go`, `execute_bench_test.go` with documented baseline at [docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md) and a stated >20% regression policy.
- **Four user-blocking issues** delivered: file()/fileexists()/trimfrontmatter() (W07), step-level for_each/count/type=workflow (W10), Copilot agent defaults (W09), targeted diagnostic for misplaced agent-config fields.
- **GoDoc** on public packages (W06).

### Gaps

- **Bus factor still 1.** `git log --since="6 months ago"` shows 133 commits by Dave Sanderson, 2 by dependabot[bot], 1 by Phase 1.1 Agent, 1 by copilot-swe-agent[bot]. Zero merged human contributors other than the maintainer. Unchanged from prior evaluation.
- **Lint baseline = 240 entries.** [.golangci.baseline.yml](.golangci.baseline.yml) is 962 lines of suppressions, tagged W03=42, W04=133, W06=54, W10=11. Two-thirds of the W04 entries are gofmt/goimports/unused findings that were *introduced by* the file-split work and parked. This is debt-paid-with-debt.
- **Lint baseline is not a CI gate.** PLAN explicitly carries this forward: make lint-go is currently manual; CI enforcement as a permanent gate is a Phase 2 nice-to-have. This means the baseline can grow undetected.
- **W10 partial.** workflow_file runtime resolution is shipped at the schema level but SubWorkflowResolver is not wired into the CLI compile path; the example workflow is deferred. This is a half-shipped feature.
- **Phase 2 is TBD.** PLAN.md commits to no scope for the next phase.
- **DurableAcrossRestart still skipped** in the SDK conformance suite (orchestrator-side dependency, unchanged from v0.1.0).
- **Six user-feedback files (02, 03, 05, 06, 07, 08)** are listed as deferred-by-design. Only 09 was actioned in Phase 1.

---

## 1. Architecture — Grade: B+

### Strengths (mostly unchanged)

- Three-module Go workspace (root, sdk/, workflow/) with import boundaries enforced by `make lint-imports` ([tools/import-lint/](tools/import-lint/)).
- FSM model is unchanged and continues to absorb feature work cleanly. W10 step-level for_each/count and type=workflow step were added without architectural rework.
- Plugin isolation via out-of-process binaries, with a lint-checked SDK boundary (internal/ may not import sdk/ except sdk/pb/...).

### Weaknesses

- **Parallel regions still TODO** in [internal/engine/node.go](internal/engine/node.go) line 47: TODO(1.6) parallelNode would call deps.BranchScheduler.Run(...). Tracked for a future language phase per PLAN.
- **workflow_file validation requires a resolver at compile time** (PLAN forward-pointer). The W10 step type is shipped but its file-loading sibling is not. If a user writes type=workflow with workflow_file=... they hit a deferred path.

**Impact:** No new architectural risk. The architecture has now absorbed two phases of feature/refactor work without breaking, which is positive evidence.

---

## 2. Code Quality — Grade: B (was D+)

### Function length

The 194-line `resumeOneRun` is gone. Spot-check of the previously-cited offenders:

| Function | Was | Now | Evidence |
|---|---:|---:|---|
| resumeOneRun | 194 | 34 | [internal/cli/reattach.go](internal/cli/reattach.go) |
| copilotPlugin.Execute | 154 | 36 | [cmd/criteria-adapter-copilot/copilot.go](cmd/criteria-adapter-copilot/copilot.go#L233) |
| Engine.runLoop | 113 | 32 | [internal/engine/engine.go](internal/engine/engine.go) |
| runApplyServer | 106 | (split) | [internal/cli/apply.go](internal/cli/apply.go) — runApplyLocal 72, helpers 33-46 |

The longest production function I could find is `compileSteps` at ~276 lines in [workflow/compile_steps.go](workflow/compile_steps.go) — this is a switch-on-step-type dispatcher and is a candidate for further decomposition, but is significantly more linear/readable than the prior god-functions. `routeIteratingStepInGraph` is 68 lines and carries //nolint:funlen with justification (iteration router is inherently stateful; splitting adds indirection) — this is acceptable when it is a documented exception, not a default.

### File size

`workflow/compile.go` (1,099 to 301 LOC, split into focused sibling files) is the headline win.

**Regression to call out:** `cmd/criteria-adapter-copilot/copilot.go` grew from 614 to **793 lines** despite W03 splitting its functions. The function decomposition is real and good, but the file itself accumulated more methods rather than splitting into copilot_session.go / copilot_permission.go / copilot_turn.go. This is the single largest non-test, non-generated file in the repo and warrants a follow-up split in Phase 2.

### Cyclomatic complexity

Most cited offenders are now straight-line glue with named helpers. compileSteps and routeIteratingStepInGraph are the remaining inherently-stateful ones; both have //nolint with justification rather than being lint-baselined.

### Naming and documentation

Spot-check: helpers in reattach.go (abandonCheckpoint, attemptReattach, loadCheckpointWorkflow, serviceResumeSignals, drainAndCleanup) are well-named with intent-revealing GoDoc. W06 added GoDoc on public packages.

---

## 3. Test Quality — Grade: B+ (was C)

### Coverage (current)

- events: 96.8%
- internal/adapters/shell: 88.1%
- internal/engine: 82.5% (was failing)
- cmd/criteria-adapter-mcp: 82.4% (was 0.0%)
- internal/run: 77.9%
- internal/plugin: 69.4% (was failing)
- cmd/criteria-adapter-mcp/mcpclient: 68.5%
- cmd/criteria-adapter-copilot: 65.9%
- internal/cli: 65.6% (was 42.0%)
- internal/transport/server: 63.4%

### Verification

`go test ./... -count=1 -race` ran clean across the root, sdk/, and workflow/ modules in 26.7s wall (longest package). No flakes observed.

`govulncheck ./...` reports **no vulnerabilities found** across all three modules.

### Concerns

- internal/transport/server 63.4% is the lowest on the hot path. The reattach/resume client streams have edge-case coverage gaps that future durability work will exercise.
- DurableAcrossRestart remains skipped in [sdk/conformance/resume.go](sdk/conformance/resume.go) — orchestrator-side dependency, accepted.
- cmd/criteria-adapter-noop reports 0% coverage by go test -cover; this is a thin reference adapter and is exercised by the conformance suite, but the standalone coverage is misleading.

---

## 4. Security — Grade: B (was C+)

### Shell adapter sandbox (the headline)

Implemented in [internal/adapters/shell/sandbox.go](internal/adapters/shell/sandbox.go) (341 LOC):

- Environment **allowlist** (PATH, HOME, USER, LOGNAME, LANG, LC_*, TZ, TERM); everything else dropped unless explicitly declared via input.env.
- PATH sanitization: strips empty / non-absolute / `.` entries.
- Working-directory confinement: must resolve under $HOME or CRITERIA_SHELL_ALLOWED_PATHS; `..` rejected.
- Hard timeout: default 5 min, range 1s-1h, SIGTERM then 5s grace then SIGKILL.
- Bounded output capture: default 4 MiB/stream, range 1 KiB-64 MiB; truncation event emitted, step continues.
- Threat model published: [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md) explicitly enumerates T1-T7, what is in/out of scope for Phase 1, and which mitigations defer to Phase 2 (syscall filtering, cgroups, network egress controls).
- Legacy escape hatch (CRITERIA_SHELL_LEGACY=1) is documented as **time-boxed for v0.3.0 removal**, not a permanent flag.

This is a defensible first hardening pass: it does not claim full isolation, it documents what it does and does not protect against, and it commits to removing the escape hatch on a published schedule. That is the right shape.

### Remaining security findings

1. **State directory permissions (minor).** [internal/cli/local_state.go](internal/cli/local_state.go#L74) lines 74 and 129 create ~/.criteria/ with 0o755 — world-readable. The token files inside are 0o600 (correct), but the *directory listing* leaks run IDs and workflow names to other local users. Recommend tightening to 0o700 to match the threat model of operator-only state.
2. **Platform-specific sandbox deferred.** macOS sandbox-exec, Linux seccomp, Windows Job Object profiles are explicitly Phase 2. The threat model is honest about this. A workflow author who can supply HCL still has full RCE capability up to the operator's UID — this is documented but is the **single largest remaining production risk**.
3. **No syscall filtering, no network egress controls, no cgroups.** All deferred. Acceptable for v0.2.0 (developer-local) but blocking for any production / multi-tenant claim.

### Positive

- govulncheck clean.
- errorlint, bodyclose, nilerr, contextcheck all enabled in golangci-lint config.
- HCL parsing is hashicorp/hcl/v2 (trusted upstream).
- TLS / mTLS is correctly opt-in on the server transport.
- New file() HCL function is bounded by CRITERIA_FILE_FUNC_MAX_BYTES and CRITERIA_WORKFLOW_ALLOWED_PATHS — designed with abuse in mind.

**Verdict:** Acceptable for developer-local use. Still **not production-ready for multi-tenant workflow authoring** without Phase 2 platform-specific isolation. Honestly documented as such.

---

## 5. Maintainability — Grade: C+ (was D)

### Contributor diversity (CRITICAL — unchanged)

```
git log --since="6 months ago" --pretty="%an" | sort | uniq -c
  133 Dave Sanderson
    2 dependabot[bot]
    1 Phase 1.1 Agent
    1 copilot-swe-agent[bot]
```

Bus factor is **still 1**. The prior evaluation flagged this as a 6-month action item; six months have not yet elapsed, but the lack of *any* movement (no good-first-issue labels visible, no contributor-onboarding doc landed) is a forward risk. Phase 2 should set a numeric goal.

### Code clarity (improved)

The reattach.go and apply.go refactors materially improve the new-contributor on-ramp. A new contributor can now read resumeOneRun and trace through five named helpers rather than wading through 194 lines of nested conditionals.

### Repo organization

- The workstreams/archived/v0/ and workstreams/archived/v1/ pattern is working; phase boundaries are clean.
- tools/lint-baseline/ codifies the burn-down contract.
- ADRs exist (docs/adrs/) and were used (ADR-0001 drove the W08 brand rename).
- .golangci.baseline.yml per-line tagging (# W03, # W04) makes ownership of each suppression visible. Whether they actually get burned down is the open question.

---

## 6. Tech Debt Register

| # | Item | Severity | Source | Status |
|---|------|----------|--------|--------|
| 1 | Bus factor of 1 | Critical | Prior eval | Unchanged. No visible recruitment activity. |
| 2 | Lint baseline (240 entries / 962 LOC) | High | New | Parked. Not enforced in CI. Mostly cosmetic (W04: gofmt/goimports/unused) but sheer count erodes the contract. |
| 3 | copilot.go is 793 LOC | Medium | Regression | File grew during W03 even as functions shrank. Needs file-level split. |
| 4 | Platform-specific shell isolation | High (production blocker) | Carried | Phase 2 candidate. Threat model is honest about this. |
| 5 | workflow_file runtime resolver not wired | Medium | W10 partial | Half-shipped feature. |
| 6 | DurableAcrossRestart skipped | Medium | Carried | Orchestrator-side dependency. |
| 7 | State dir 0o755 perms | Low | New | One-line fix; trivial. |
| 8 | Six user-feedback items deferred (02, 03, 05, 06, 07, 08) | Medium | Carried | Phase 2 must triage these. |
| 9 | Lint not enforced in CI as permanent gate | Medium | Stated in PLAN | Phase 2 nice-to-have. |
| 10 | compileSteps 276 LOC | Low | Spot-check | Decomposable but linear. |
| 11 | Phase 2 scope is TBD | Medium | PLAN.md | Project lacks a forward roadmap at the moment. |

---

## 7. Performance — Grade: B+

Benchmarks now exist with a published baseline ([docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md)) and a stated regression policy (>20% fails review).

Notable numbers (Apple M3 Max, go1.26.2):

- BenchmarkCompile_1000Steps: 31.9 ms, 56 MB, 389k allocs — proportional and unsurprising.
- BenchmarkEngineRun_1000Steps: 1.47 ms, ~26 allocs/step — linear, reasonable.
- BenchmarkPluginExecuteNoop: 8.3 ns, **0 allocs** — plugin-dispatch overhead is essentially free; the cost of a shell step is dominated by exec (22 ms for /usr/bin/true).

**No optimization concerns** for the stated workload. The compiler allocations could be tightened later but this is not a current bottleneck.

---

## Verdict: VIABLE (was MARGINAL)

Criteria executed Phase 1 well. Every code-quality and test-stability blocker raised in the prior evaluation was directly addressed with traceable evidence. The shell adapter sandbox is the right shape — first-pass, honestly scoped, with a published threat model and a time-boxed escape hatch. Test coverage and benchmarks now have actual gates rather than aspirations. The codebase is meaningfully easier to read and meaningfully easier to onboard into.

What prevents an A-grade verdict: the project is still effectively a one-person codebase, the lint baseline grew large enough to be a second-order problem of its own, and Phase 2 has no committed scope.

### What would change the verdict to STRONG

1. **Two non-author humans land merged PRs** within Phase 2.
2. **Lint baseline burns down to <50 entries** and `make lint-go` becomes a hard CI gate.
3. **Phase 2 plan published** (PLAN.md to committed scope).
4. **Platform-specific shell isolation** lands for at least one of macOS or Linux — moves the not-production-ready-for-multi-tenant caveat off the README.
5. **copilot.go split** into <=350-LOC files.
6. **State-dir perms** tightened to 0o700.

### What would move it back to MARGINAL

- Lint baseline grows in Phase 2 instead of shrinking.
- Bus factor still 1 at the end of Phase 2.
- A regression on the `-race -count=1` test contract (any reintroduced flake).
- Shell sandbox legacy mode (CRITERIA_SHELL_LEGACY=1) is **not** removed in v0.3.0 as promised — that would establish a pattern of slipping security commitments.

### What would move it to NOT VIABLE

- A security incident attributable to the deferred shell isolation work, with no remediation path.
- The maintainer becomes unavailable without a successor.
- Phase 2 spends 11 workstreams refactoring instead of shipping user-visible value.

---

## Specific Remediation Paths

### 1. Lint baseline burn-down (Phase 2 gate)

Triage the 240 entries:

- **W04 (133 entries, mostly gofmt/goimports/unused on split files):** these are mechanical fixes — most can be cleared in a single pass with goimports -w plus dead-code removal. Allocate one workstream.
- **W03 (42 entries):** real refactor work on handlePermissionRequest, permissionDetails, and the residual extracted helpers. Worth 2-3 days.
- **W06 (54 entries):** unclear scope — audit and either fix or document permanent-exception with a //nolint and a justification comment, not a baseline entry.
- **Promote `make lint-go` to a hard CI gate** with a cap that prevents new entries.

### 2. Contributor recruitment (Phase 2 must-do)

- Label 5 issues good-first-issue (the W04 lint fixes are excellent first PRs).
- Write docs/contributing/your-first-pr.md with a concrete walkthrough.
- Set a numeric goal (e.g., 2 non-author PRs merged by end of Phase 2) and report on it in the Phase 2 cleanup gate.

### 3. copilot.go split

Target structure:

- copilot.go — plugin lifecycle, Open/Close (<=200 LOC)
- copilot_session.go — session state, model/effort restore (<=200 LOC)
- copilot_permission.go — permission bridge, permissionDetails (<=200 LOC)
- copilot_turn.go — turnState, event handlers, awaitOutcome (<=200 LOC)

This also unblocks burning down the W03 funlen entries on permissionDetails and handlePermissionRequest.

### 4. State-dir permissions

One-line fix in [internal/cli/local_state.go](internal/cli/local_state.go#L74) lines 74 and 129: 0o755 to 0o700. Add a regression test that asserts Stat().Mode().Perm() == 0o700 on the state dir.
