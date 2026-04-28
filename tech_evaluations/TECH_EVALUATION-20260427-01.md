# Technical Evaluation — Criteria v0.1.0

**Date:** 2026-04-27  
**Evaluator:** AI Technical Evaluator  
**Commit:** Phase 0 closed, v0.1.0 tagged  
**Codebase:** ~12,300 LOC production + ~9,500 LOC tests

---

## Executive Summary

Criteria is a **marginal** viable codebase with serious code quality debt that will impede future development velocity and contributor onboarding. While the architecture is sound (FSM-based workflow engine, plugin model, well-defined SDK contract), the implementation suffers from excessive function length, intermittent test failures indicating race conditions, and effectively single-person development (bus factor of 1). The project shipped Phase 0 on schedule but accumulated technical debt that must be addressed before Phase 1 feature work or external adoption becomes viable.

**Critical blockers:** Flaky tests, 194-line functions, zero external contributors.

---

## Grade Card

| Area | Grade | Justification |
|------|-------|---------------|
| **Architecture** | B+ | Clean FSM model, good module boundaries, enforced import rules |
| **Code Quality** | D+ | Multiple 100+ line functions, high cyclomatic complexity, poor decomposition |
| **Test Quality** | C | Good coverage ratio (0.77:1) but flaky suite, two packages fail in full run |
| **Documentation** | B | Clear README/AGENTS/PLAN; missing GoDoc on many exported types |
| **Security** | C+ | No obvious vulns; shell adapter needs hardening (W04 deferred) |
| **Maintainability** | D | Single contributor, long functions, complex control flow |
| **Tech Debt** | C- | Only 3 TODOs but deferred shell sandboxing is a security time bomb |
| **Performance** | B | No profiling data; design appears reasonable for target workload |

---

## Project Description

Criteria is a standalone workflow execution engine that compiles HCL workflow definitions into finite-state machines and executes them via swappable adapter plugins. It targets teams who want a Temporal/Argo-style execution model without infrastructure dependencies. The project supports both local execution and server-mode orchestration via a published Connect/gRPC SDK.

**Phase 0 goal:** Post-fork cleanup, naming convention review, public SDK extraction, repo hygiene, and v0.1.0 tag.


---

## Current State vs. Stated Goals

### Goals Met ✅

- [x] Standalone local execution works (`criteria apply`)
- [x] HCL → FSM compilation functional
- [x] Adapter plugin model operational (noop, shell, copilot, MCP)
- [x] Published Go SDK with conformance suite
- [x] Server-mode orchestration support
- [x] Phase 0 workstreams closed, v0.1.0 tagged
- [x] Import boundary enforcement (`make lint-imports`)
- [x] Structured logging throughout

### Gaps and Risks ⚠️

- **Flaky tests:** `TestEngineLifecycleOpenTimeoutKeepsSessionAlive` and `TestHandshakeInfo` pass individually but fail in `make test` (race condition or test pollution).
- **Zero external contributors:** 98% of commits by a single author (88/90 in last 6mo).
- **Deferred security work:** Shell adapter sandboxing (W04) postponed; this is a **pre-deployment blocker** for any production use.
- **No profiling or benchmarks:** Performance claims unvalidated.
- **Missing SDK durability:** `DurableAcrossRestart` conformance test skipped pending orchestrator work.

---

## Code Quality — Grade: D+

### 1. Function Length (CRITICAL)

**Finding:** Multiple functions exceed 100 lines; longest is 194 lines.

**Evidence:**

- `internal/cli/reattach.go:40` — `resumeOneRun`: **194 lines**
- `cmd/criteria-adapter-copilot/copilot.go:186` — `Execute`: **154 lines**
- `internal/engine/engine.go:144` — `runLoop`: **113 lines**
- `internal/cli/apply.go:150` — `runApplyServer`: **106 lines**

**Impact:** These god-functions are untestable in isolation, difficult to reason about, and impossible to refactor safely. The 194-line `resumeOneRun` mixes client setup, error recovery, variable scope restoration, pause/resume logic, and cleanup in one monolithic block with 6+ levels of conditional nesting.

**Required remediation:**

1. Extract helper functions: separate credential validation, client setup, scope restoration, pause handling.
2. Introduce state machines for multi-step recovery flows.
3. Target: no function > 50 lines outside of generated code.


---

### 2. File Size

**Finding:** Single-file modules exceed recommended limits.

**Evidence:**

- `workflow/compile.go` — **1,099 lines**
- `internal/adapter/conformance/conformance.go` — **797 lines**
- `internal/transport/server/client.go` — **644 lines**
- `cmd/criteria-adapter-copilot/copilot.go` — **614 lines**

**Impact:** The workflow compiler is a 1,099-line monolith mixing HCL parsing, schema validation, node construction, and error diagnostics. This violates SRP and makes partial rewrites (e.g., adding sub-workflow support) high-risk.

**Required remediation:**

- Split `workflow/compile.go` into `compile_variables.go`, `compile_steps.go`, `compile_agents.go`, etc.
- Extract conformance helpers into `conformance/assertions.go`, `conformance/fixtures.go`.

---

### 3. Cyclomatic Complexity

**Finding:** Several functions exceed reasonable complexity thresholds (estimated 15+).

**Evidence:**

- `resumeOneRun` (194 lines): handles 6+ error cases, pause/resume state machine, credential setup, variable restoration — estimated McCabe complexity **> 20**.
- `runLoop` (113 lines): nested for-loop with context checks, error unwrapping, pause detection, iter cursor management — estimated **> 15**.
- `copilotPlugin.Execute` (154 lines): event handler with channel orchestration, permission denial, turn limits, outcome parsing — estimated **> 18**.

**Impact:** Functions with complexity > 10 are error-prone and difficult to test exhaustively. The current state requires heroic effort to add feature branches without introducing regressions.

**Required remediation:**

- Extract decision logic into named functions (e.g., `shouldRetryStep`, `isTerminalError`).
- Replace deeply nested conditionals with early returns.
- Introduce table-driven tests for complex branching.

---

### 4. Duplication

**Finding:** Minimal copy-paste duplication detected; abstraction boundaries are generally respected.

**Evidence:** Adapter conformance suite uses shared test harness (`executeNoPanic` helper). Engine node implementations follow consistent interface pattern.

**Positive note:** The plugin model and conformance suite demonstrate good abstraction.

---

### 5. Naming and Documentation

**Finding:** Most names are clear; GoDoc coverage is spotty.

**Evidence:**

- `internal/engine/engine.go` — `Sink` interface well-documented (W04 amendments inline).
- `workflow/compile.go` — `Compile` function has clear doc comment.
- `sdk/doc.go` — Package-level doc exists but incomplete.

**Minor issue:** Many exported functions lack GoDoc (e.g., `buildCompileJSON`, `renderDOT`).

**Recommended:** Run `go vet` with `-unsafeptr=false` and enforce GoDoc for all exported symbols before Phase 1.


---

## Test Quality — Grade: C

### Coverage Numbers (from `make test`)

```
events:                     96.7%
workflow:                   77.7%
internal/adapters/shell:    83.6%
sdk/conformance:            varies (ack 60%, schema 70%, resume skipped)
internal/cli:               42.0%
internal/run:               48.0%
internal/transport/server:  63.4%
cmd/criteria-adapter-copilot: 60.7%
cmd/criteria-adapter-mcp:     0.0% (integration-only)
internal/plugin:            test failure
internal/engine:            test failure
```

**Findings:**

1. **Flaky tests (CRITICAL):** Two tests fail in `make test` but pass individually:
   - `TestEngineLifecycleOpenTimeoutKeepsSessionAlive`
   - `TestHandshakeInfo`
   
   **Root cause:** Likely race condition or test pollution (shared global state, goroutine leaks, or timing dependency).

2. **Coverage gaps:**
   - `internal/cli/apply.go` — 42% coverage; server-mode resume path undertested.
   - `cmd/criteria-adapter-mcp` — 0% unit tests (conformance suite only).

3. **Deferred durability:** `sdk/conformance/resume.go` skips `DurableAcrossRestart` pending orchestrator work.

**Impact:** Flaky tests destroy CI/CD trust and indicate race conditions in production code paths (likely in plugin lifecycle or session management). Undertested CLI code is a deployment risk.

**Required remediation:**

1. **Fix flaky tests (blocker):** Run with `-race`, add `goleak` verification, isolate shared state.
2. Raise CLI coverage to >60% (focus on `resumeOneRun`, `runApplyServer`).
3. Add MCP adapter unit tests.
4. Unskip `DurableAcrossRestart` when orchestrator ships durability.

---

## Architecture — Grade: B+

### Strengths

- **Clean module separation:** Three Go modules (root, `sdk/`, `workflow/`) with enforced import boundaries (`make lint-imports`).
- **FSM model:** Workflow → FSM compilation is conceptually clean; nodes implement shared `Evaluate` interface.
- **Plugin isolation:** Adapters run out-of-process via hashicorp/go-plugin; crashes are contained.
- **Event stream:** ND-JSON event schema versioning supports backward compatibility.

### Weaknesses

- **No parallel regions:** Current FSM is strictly sequential; parallel step execution (flagged as TODO in `internal/engine/node.go:50`) is deferred.
- **Shell adapter unsandboxed:** W04 deferred full sandboxing (filesystem isolation, syscall filtering); current implementation is a **pre-deployment security blocker**.

**Overall:** The architecture supports the stated goals but needs the deferred features (parallel regions, shell sandboxing) before claiming "production-ready."


---

## Security — Grade: C+

### Findings

1. **Shell adapter (CRITICAL):**
   - `internal/adapters/shell/shell.go` — Executes arbitrary commands with no syscall filtering, chroot, or resource limits.
   - **Risk:** Any workflow with a `shell` step is a remote code execution vector.
   - **Mitigation:** W04 deferred; blocking v1.0 without sandboxing.

2. **TLS configuration:**
   - `internal/cli/http.go:24` — `serverHTTPClient` supports mTLS.
   - `internal/transport/server/client.go` — Connect client respects `TLSMode`.
   - **Positive:** TLS is opt-in but correctly implemented.

3. **No obvious injection vulnerabilities:**
   - HCL parsing uses `hashicorp/hcl/v2` (trusted).
   - Adapter inputs are string maps (no SQL, no template injection observed).

4. **Credentials in checkpoints:**
   - `internal/cli/local_state.go` — `StepCheckpoint` stores `Token` in plaintext JSON on disk.
   - **Risk:** Credential exposure if checkpoint directory is world-readable.
   - **Mitigation:** Document recommended permissions (`chmod 700 ~/.criteria/state`).

**Verdict:** Acceptable for developer-local use; **not production-ready** without shell sandboxing.

---

## Maintainability — Grade: D

### Contributor Diversity (CRITICAL)

**Finding:** Single-person project with bus factor of 1.

**Evidence:**

```
git log --since="6 months ago" --pretty="%an" | sort | uniq -c
  88 Dave Sanderson
   1 Phase 1.1 Agent
   1 dependabot[bot]
```

**Impact:** Project continuity risk. If the primary author becomes unavailable, no one else understands the codebase deeply enough to maintain it.

**Required remediation:**

1. Recruit 2–3 additional maintainers.
2. Document tribal knowledge in `/memories/repo/`.
3. Establish code review requirement (no self-merge) to force knowledge transfer.

---

### Code Clarity

**Finding:** Long functions and missing GoDoc harm onboarding velocity.

**Evidence:** New contributors face a 194-line function with 6-level nesting as the entry point to crash recovery — this is a **contributor repellent**.

**Required remediation:**

- Refactor god-functions before advertising for contributors.
- Add architecture decision records (ADRs) for non-obvious choices (e.g., why iter cursor is JSON-serialized opaquely).


---

## Tech Debt Register

| # | Item | Severity | Blocked By | Target |
|---|------|----------|------------|--------|
| 1 | Shell adapter sandboxing | **Critical** | W04 deferred | Pre-v1.0 |
| 2 | Flaky test suite | **Critical** | Race condition | Phase 1 gate |
| 3 | `resumeOneRun` refactor | High | None | Phase 1 start |
| 4 | `workflow/compile.go` split | Medium | None | Phase 1.x |
| 5 | SDK `DurableAcrossRestart` | Medium | Orchestrator work | When ready |
| 6 | Parallel regions (FSM) | Low | Design phase | Phase 2+ |
| 7 | GoDoc coverage | Low | None | Ongoing |

---

## Performance — Grade: B

**Finding:** No benchmarks or profiling data available.

**Evidence:** No `*_bench_test.go` files in critical paths (engine, compiler).

**Impact:** Performance claims ("suitable for local dev workflows") are **unvalidated**.

**Required remediation:**

1. Add benchmarks for `workflow.Compile`, `engine.Run`, `plugin.Execute`.
2. Profile a 1,000-step workflow under `examples/perf_1000_logs.hcl`.
3. Document baseline metrics (steps/sec, memory footprint).

---

## Verdict: **MARGINAL**

Criteria is **marginally viable** for its stated goal (developer-local workflow execution). The architecture is sound, but code quality debt and single-person development make the project fragile.

### What Would Change the Verdict to VIABLE

**Phase 1 Gate (3 months):**

1. ✅ Fix flaky tests (`TestEngineLifecycleOpenTimeoutKeepsSessionAlive`, `TestHandshakeInfo`) — **blocker**.
2. ✅ Refactor `resumeOneRun` to <50 lines per function.
3. ✅ Recruit 1–2 additional maintainers (GitHub contributors, not bots).
4. ✅ Raise CLI test coverage to >60%.
5. ✅ Shell adapter sandboxing design doc (W04 revival).

**Pre-v1.0 Gate (6 months):**

6. ✅ Ship shell adapter sandboxing (chroot, seccomp, resource limits).
7. ✅ Add performance benchmarks for engine + compiler.
8. ✅ GoDoc coverage >90% on exported symbols.
9. ✅ External user documentation (quickstart, troubleshooting).

### What Would Change the Verdict to NOT VIABLE

**Red flags (any ONE is terminal):**

- Flaky tests remain unfixed after 2 sprints.
- Shell adapter ships to production **without** sandboxing.
- Contributor count remains 1 after 6 months.
- Major design pivot required (e.g., FSM model fundamentally broken).


---

## Specific Remediation Paths

### 1. Fix Flaky Tests (Week 1)

**Steps:**

1. Run full suite with `-race -count=50` to reproduce.
2. Add `goleak.VerifyNone(t)` to suspected tests.
3. Audit shared state (plugin loader, session manager).
4. Introduce test isolation (separate temp dirs, unique ports).

**Success criteria:** `make test` passes 100/100 times.

---

### 2. Refactor `resumeOneRun` (Week 2–3)

**Target structure:**

```go
func resumeOneRun(ctx, log, cp, opts) {
    client, err := buildRecoveryClient(cp, opts)
    ...
    resp, err := attemptReattach(ctx, client, cp)
    ...
    if resp.Status == "paused" {
        return resumePausedRun(ctx, client, cp, resp)
    }
    return resumeActiveRun(ctx, client, cp, resp)
}

func buildRecoveryClient(...) (*Client, error) { ... }
func attemptReattach(...) (*ReattachResponse, error) { ... }
func resumePausedRun(...) error { ... }
func resumeActiveRun(...) error { ... }
```

**Success criteria:** Each extracted function <50 lines, individually testable.

---

### 3. Contributor Onboarding (Month 2–3)

**Actions:**

1. Label 5–10 issues as `good-first-issue` (e.g., "add benchmark for X").
2. Write CONTRIBUTING.md section: "Your First PR."
3. Record video walkthrough: "How the Engine Works."
4. Host office hours (Discord/Slack).

**Success criteria:** 2+ non-author PRs merged by Month 3.

---

## Conclusion

Criteria has **shipped Phase 0** on schedule and demonstrates a clean architectural vision. However, the codebase exhibits serious quality issues (god-functions, flaky tests, single-person development) that will cripple Phase 1 velocity if unaddressed. The project is **marginal** today; it becomes **viable** only after fixing tests, refactoring the worst offenders, and recruiting maintainers.

**Recommendation:** **Pause new feature work** until the Phase 1 gate criteria (§7) are met. Investing 3–4 weeks now to pay down debt will yield 10x returns in Phase 1 delivery speed and contributor retention.

**Bottom line:** The engine runs; the code doesn't. Fix the code before scaling the engine.

---

**Evaluator Notes:**

- Evaluation based on commit state as of 2026-04-27 (v0.1.0 tag).
- No access to orchestrator repo; SDK contract evaluated in isolation.
- Performance claims unverified (no benchmark data available).
- Security review scope limited to static analysis (no penetration testing).

---

END EVALUATION
