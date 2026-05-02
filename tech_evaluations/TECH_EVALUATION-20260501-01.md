# Technical Evaluation - Criteria current state

**Date:** 2026-05-01  
**Evaluator:** AI Technical Evaluator  
**Commit:** `70eb9ce` (`v0.1.0-67-g70eb9ce`, clean worktree)  
**Baseline problem:** project docs claim `v0.2.0` is tagged, but local tags are only `v0.1.0` and `v0.1.0-rc1`; `git ls-remote --tags origin` returned only `v0.1.0-rc1`. A true `v0.2.0..HEAD` delta cannot be computed from tags in this checkout.

## Executive Summary

Criteria is **viable for continued investment** as a standalone HCL-to-FSM workflow engine and Go SDK, but it is not yet a reliable public release artifact or production-safe multi-tenant runner. The code has moved in a coherent direction since the prior evaluation: deterministic test gates, lint debt caps, local-mode approval/signal handling, per-step `max_visits`, Docker runtime smoke, and Copilot structured `submit_outcome` finalization all exist and pass local verification. The most serious current defect is release-process integrity: documentation says `v0.2.0` was tagged and installable, while the repository and remote tag state do not support that claim. Long-term success is plausible at the current velocity, but only if Phase 2 closes with a real release, the `workflow_file`/sub-workflow gap is either completed or de-advertised, and maintenance risk is reduced with actual non-author contributors.

## Grade Card

| Area | Grade | One-line justification |
|---|---:|---|
| Architecture | B | FSM, plugin, SDK, and local/server modes are coherent; sub-workflow scope and `workflow_file` remain incomplete. |
| Code Quality | B- | Major refactors landed, but large orchestrating files, 70 lint baseline entries, and 49 explicit `//nolint` exceptions remain. |
| Test Quality | B+ | Tests, conformance, examples, coverage, lint, proto drift, plugins, govulncheck, and Docker smoke pass; server-mode apply paths still lack direct coverage. |
| Documentation | B- | README/PLAN/workstream docs are detailed and directionally honest, but release/tag claims are false in the current repository state. |
| Security | B- | Shell sandbox and Docker runtime are real, `govulncheck` is clean, but untrusted workflow execution still lacks syscall, network, and cgroup isolation. |
| SDK / Wire Contract | B | Proto source is disciplined, additive W14 field is drift-clean, conformance passes; durable resume across orchestrator restart remains skipped. |
| Release / Operations | C | CI and RC artifacts exist, Docker runtime works, but official tags/releases/signing are not actually in place. |
| Maintainability | C+ | Workstream process and onboarding docs help; the project is still effectively one-human-maintained. |
| Tech Debt | C+ | Debt is being burned down, but current cap is exactly full (`70 / 70`) and several deferred gaps are user-visible. |
| Performance / Scalability | B | Published baseline shows linear engine behavior; no parallel regions and no load evidence beyond local benchmarks. |
| Frontend / UI | N/A | The project is a CLI/SDK/runtime repository; no frontend application surface exists. |

## Project Description

Criteria describes itself as a standalone workflow execution engine: users write HCL, run `criteria apply`, the workflow compiles to an FSM, and execution flows through swappable adapter plugins while emitting structured ND-JSON events [README.md](README.md#L3). Its target users are teams wanting a Temporal/Argo-like model without day-to-day infrastructure, plus orchestrator authors needing a stable client SDK [README.md](README.md#L5). The advertised box includes local execution, out-of-process adapter plugins, structured event streams, waits/branches/loops, orchestrator mode, and a published Go SDK [README.md](README.md#L69), [README.md](README.md#L77).

## Current State vs. Stated Goals

### Release Delta

The documented last release is `v0.2.0` in the changelog [CHANGELOG.md](CHANGELOG.md#L5), and PLAN says Phase 1 closed with `v0.2.0` tagged [PLAN.md](PLAN.md#L15). That is not true in the repository state I inspected. `git show-ref --tags` showed only local `v0.1.0` and `v0.1.0-rc1`; `git ls-remote --tags origin` returned only `v0.1.0-rc1`; `git diff v0.2.0..HEAD` fails because the revision does not exist.

Using the latest actual local tag, `v0.1.0..HEAD` contains 67 commits and a large delta: 263 files changed, 43,253 insertions, 5,436 deletions. Using the documented `v0.2.0` date boundary, 17 commits landed after 2026-04-29: 16 by Dave Sanderson and 1 by Copilot. The post-date direction is not random churn: it implements Phase 2 workstreams around lint baseline burn-down/capping, Copilot file split and structured outcome finalization, state-dir hardening, local approval/signal waits, per-step visit limits, Docker runtime, removal of `CRITERIA_SHELL_LEGACY`, lifecycle log clarity, RC artifacts, and W14/W15 wire/adapter changes.

### Mission Fit

The local-engine mission is currently met. Example workflows validate, `make validate` passes, bundled plugins build, the greeter external plugin smoke passes, and the Docker runtime can run `examples/hello.hcl` inside the container. The plugin model is real: adapter binaries are discovered from `CRITERIA_PLUGINS` or `~/.criteria/plugins`, not `PATH`, reducing accidental binary execution [internal/plugin/discovery.go](internal/plugin/discovery.go#L31). The public pluginhost SDK gives external plugin authors a stable service interface [sdk/pluginhost/service.go](sdk/pluginhost/service.go#L13).

The orchestrator-author mission is partially met. The SDK conformance package defines an external `Subject` contract and runs envelope, ack, control, resume, ownership, and schema tests [sdk/conformance/conformance.go](sdk/conformance/conformance.go#L33), and `make test-conformance` passes. The gap is durable resume across orchestrator restart, which is explicitly skipped [sdk/conformance/resume.go](sdk/conformance/resume.go#L42). That is acceptable as a tracked pre-v1 gap, but it blocks any claim that orchestrator durability is fully proven.

The unattended-MVP Phase 2 direction is credible. PLAN states the goal directly: lift Maintainability/Tech Debt, ship local approval plus `max_visits`, replace brittle Copilot prose parsing with `submit_outcome`, establish Docker runtime, remove the shell legacy escape hatch, and absorb deferred user feedback [PLAN.md](PLAN.md#L79). Code evidence backs the direction: local approval supports stdin/file/env/auto-approve modes [internal/cli/localresume/resumer.go](internal/cli/localresume/resumer.go#L1), `max_visits` is compiled and enforced [workflow/schema.go](workflow/schema.go#L87), [internal/engine/node_step.go](internal/engine/node_step.go#L377), and Copilot finalization is now a tool-call contract [cmd/criteria-adapter-copilot/copilot.go](cmd/criteria-adapter-copilot/copilot.go#L17).

## Verification Performed

| Check | Result |
|---|---|
| `make test` | Pass, `-race` across root, `sdk/`, and `workflow/`. |
| `make test-cover` | Pass; root total 62.5%. Key packages: `internal/cli` 69.2%, `internal/cli/localresume` 85.8%, `internal/engine` 83.7%, `internal/plugin` 71.4%, `internal/adapters/shell` 86.7%, `internal/transport/server` 63.4%, `workflow` 75.9%, `sdk` 75.0%, `sdk/conformance` 83.6%. |
| `make lint-imports` | Pass. |
| `make lint-go` | Pass under merged golangci baseline. |
| `make lint-baseline-check` | Pass: `70 / 70`. |
| `make validate` | Pass for all standalone examples; Copilot example emits expected alias diagnostics. |
| `make test-conformance` | Pass. |
| `make proto-check-drift` | Pass; no generated SDK drift reported. |
| `make example-plugin` | Pass. |
| `make plugins` | Pass; bundled adapter binaries present. |
| `make docker-runtime-smoke` | Pass; image builds and runs `examples/hello.hcl`. |
| `govulncheck` via `go run` | No vulnerabilities found in root, `sdk/`, or `workflow/`. |
| Git worktree | Clean before report creation. |

## 1. Architecture - Grade: B

### Evidence

- The stated architecture is HCL to FSM to runner, with plugin execution and ND-JSON events [README.md](README.md#L3).
- The repo is intentionally split into root, SDK, and workflow modules, with import-boundary enforcement documented in AGENTS [AGENTS.md](AGENTS.md#L46) and passing locally.
- Plugin execution is out-of-process through hashicorp/go-plugin, with one subprocess per resolved plugin handle [internal/plugin/loader.go](internal/plugin/loader.go#L100).
- The wire contract source of truth is proto; W14 adds `ExecuteRequest.allowed_outcomes = 4` with permanent numbering [proto/criteria/v1/adapter_plugin.proto](proto/criteria/v1/adapter_plugin.proto#L47).
- Server transport has reconnect-oriented SubmitEvents logic, pending replay, `since_seq`, and ack dedup semantics [internal/transport/server/client_streams.go](internal/transport/server/client_streams.go#L141), with tests for reconnect and persist-before-ack windows [internal/transport/server/client_test.go](internal/transport/server/client_test.go#L394).

### Impact Assessment

The architecture supports the described core engine. FSM compilation gives deterministic graph execution, adapter plugins are isolated at process boundaries, and the SDK contract is externalized. The project has absorbed real feature work without architectural collapse.

The architectural weak point is sub-workflow composition. `WorkflowBodySpec` claims to mirror top-level `Spec`, but it omits variables, agents, policy, and permissions [workflow/schema.go](workflow/schema.go#L108), while top-level `Spec` includes them [workflow/schema.go](workflow/schema.go#L11). `workflow_file` support exists in the compiler but fails without `SubWorkflowResolver` [workflow/compile_steps.go](workflow/compile_steps.go#L349), and the CLI compile path does not pass one [internal/cli/apply.go](internal/cli/apply.go#L399). PLAN defers full `workflow_file` resolution to Phase 3 [PLAN.md](PLAN.md#L123). This is not fatal, but it is a half-exposed language feature.

Parallel execution is also not implemented. PLAN tracks parallel regions as future work [PLAN.md](PLAN.md#L119), docs mark parallel blocks as not implemented [docs/workflow.md](docs/workflow.md#L972), and the engine still has a scheduler TODO [internal/engine/node.go](internal/engine/node.go#L47). That is acceptable for the current sequential mission, but it constrains scalability claims.

### Remediation Path

1. Either wire `SubWorkflowResolver` into CLI compile paths or remove/de-emphasize `workflow_file` until Phase 3 actually ships.
2. Replace `WorkflowBodySpec` with a true nested `Spec` or explicitly document the subset and enforce it consistently.
3. Keep parallel regions out of public examples until a scheduler and synchronization model exist.

## 2. Code Quality - Grade: B-

### Evidence

- Largest non-generated production Go files are still large: [internal/cli/apply.go](internal/cli/apply.go#L1) is 728 LOC, [workflow/compile_steps.go](workflow/compile_steps.go#L1) is 622 LOC, [internal/cli/localresume/resumer.go](internal/cli/localresume/resumer.go#L1) is 547 LOC, [internal/engine/node_step.go](internal/engine/node_step.go#L1) is 533 LOC, and [workflow/eval.go](workflow/eval.go#L1) is 517 LOC.
- The lint baseline is down to 70 entries but exactly at cap. Ownership by workstream: W04=34, W06=28, W07=4, W10=4. By linter: `gocritic` 24, `revive` 9, `errcheck` 9, `contextcheck` 9, `gocognit` 7, `gocyclo` 6, `funlen` 6.
- Baseline entries still include core compiler complexity around `compileSteps`, `resolveTransitions`, and `checkReachability` [.golangci.baseline.yml](.golangci.baseline.yml#L69), [.golangci.baseline.yml](.golangci.baseline.yml#L89).
- There are 49 explicit `//nolint` directives outside generated proto bindings. Some are justified, but they include core hot paths like plugin execution [internal/plugin/loader.go](internal/plugin/loader.go#L204), local apply orchestration [internal/cli/apply.go](internal/cli/apply.go#L86), and server control reconnect loops [internal/transport/server/client_streams.go](internal/transport/server/client_streams.go#L59).
- Copilot was split from one oversized file into focused files with a clear layout [cmd/criteria-adapter-copilot/copilot.go](cmd/criteria-adapter-copilot/copilot.go#L27), which is a real improvement.

### Impact Assessment

The codebase is no longer in the prior god-function state. The main workflows are readable enough for continued feature work. However, the debt cap being exactly full means the next lint issue fails the gate unless someone fixes debt or explicitly raises the cap. That is good discipline but also evidence that the project is operating close to its quality budget.

The largest files are mostly orchestration-heavy rather than confused piles of unrelated behavior, but they still increase review and onboarding cost. The biggest remaining maintainability risk is not a single bad file; it is the accumulation of accepted exceptions across compiler, CLI, plugin, and conformance paths.

### Remediation Path

1. Reduce the baseline below 50 before `v0.3.0`, not merely keep it flat.
2. Split [internal/cli/apply.go](internal/cli/apply.go) into local, server, pause/resume, and compile/setup files.
3. Decompose `compileSteps` into step-kind specific compilers; current baseline entries prove this is still complex debt.
4. Convert justified permanent exceptions from baseline entries into narrow `//nolint:<linter>` comments only when the design really requires them.

## 3. Test Quality - Grade: B+

### Evidence

- `make test` passes with the race detector across root, `sdk/`, and `workflow/`.
- `make test-cover` shows strong core coverage: shell 86.7%, engine 83.7%, plugin 71.4%, CLI localresume 85.8%, workflow 75.9%, SDK conformance 83.6%.
- Adapter conformance now covers name stability, nil sink, happy path, cancellation, timeout, outcome domain, chunked IO, session lifecycle, concurrent sessions, crash detection, and permission shape [internal/adapter/conformance/conformance.go](internal/adapter/conformance/conformance.go#L96).
- Shell sandbox tests cover env allowlist, PATH hygiene, timeout, bounded output, working-directory confinement, and legacy env var removal [internal/adapters/shell/shell_sandbox_test.go](internal/adapters/shell/shell_sandbox_test.go#L62), [internal/adapters/shell/shell_sandbox_test.go](internal/adapters/shell/shell_sandbox_test.go#L194), [internal/adapters/shell/shell_sandbox_test.go](internal/adapters/shell/shell_sandbox_test.go#L354).
- Max-visits tests cover hit, not-hit, omitted unlimited, retry counting, persistence, and cancellation behavior [internal/engine/engine_test.go](internal/engine/engine_test.go#L568).
- Copilot W15 has direct tests for allowed outcomes propagation and `submit_outcome` behavior [cmd/criteria-adapter-copilot/conformance_test.go](cmd/criteria-adapter-copilot/conformance_test.go#L186).

### Impact Assessment

The test suite is credible. The prior flakiness concern is not visible in this evaluation; `make test` and the relevant gates passed cleanly. The suite now tests behavior, not just function calls, especially around adapter lifecycle, shell sandboxing, and iterative execution.

The main gap is server-mode CLI coverage. `make test-cover` reports 0% for `executeServerRun`, `drainResumeCycles`, `runApplyServer`, and `setupServerRun` in [internal/cli/apply.go](internal/cli/apply.go#L257). This matters because server mode is part of the stated mission, and those paths contain registration, stream startup, resume handling, checkpoints, and cancellation behavior.

### Remediation Path

1. Add a fake server integration harness around `runApplyServer`, `executeServerRun`, and resume/cancel flows.
2. Raise [internal/transport/server](internal/transport/server) above 70% and cover the lowest-risk control-stream branches that currently rely on integration assumptions.
3. Keep `make test -race -count=2` as a CI invariant; regressions here should block release.

## 4. Security - Grade: B-

### Evidence

- `govulncheck` found no known vulnerabilities in all three modules.
- Shell adapter hardening is implemented: env allowlist, PATH sanitization, timeout, bounded output, and working-directory confinement [internal/adapters/shell/shell.go](internal/adapters/shell/shell.go#L76), [internal/adapters/shell/sandbox.go](internal/adapters/shell/sandbox.go#L43).
- `CRITERIA_SHELL_LEGACY=1` was removed from behavior, and tests assert the env var no longer weakens enforcement [internal/adapters/shell/sandbox.go](internal/adapters/shell/sandbox.go#L6), [internal/adapters/shell/shell_sandbox_test.go](internal/adapters/shell/shell_sandbox_test.go#L354).
- Local state and checkpoints now use `0o700` directories and `0o600` files [internal/cli/local_state.go](internal/cli/local_state.go#L79), [internal/cli/local_state.go](internal/cli/local_state.go#L134).
- Approval/signal local state validates node names to prevent path traversal [internal/cli/local_state.go](internal/cli/local_state.go#L164).
- Server transport supports h2c, TLS, and mTLS with TLS 1.2 minimum [internal/transport/server/client.go](internal/transport/server/client.go#L31), [internal/cli/http.go](internal/cli/http.go#L24).
- The runtime Docker image runs as an unprivileged `criteria` user and packages bundled adapters into the plugin directory [Dockerfile.runtime](Dockerfile.runtime#L16).

### Impact Assessment

The project is now acceptable for local developer workflows where the operator trusts the workflow content. It is still not safe for hostile workflow authors on a shared host. The threat model is explicit: syscall filtering, filesystem isolation, network egress controls, and cgroups are out of scope [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md#L68), [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md#L76), [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md#L79). Docker reduces host blast radius when used, but docs correctly say it is not the future per-adapter environment-plug abstraction or OS-level isolation [docs/runtime/docker.md](docs/runtime/docker.md#L7).

Plugin execution remains trust-based. Discovery avoids `PATH`, validates adapter names, and requires executable files in known plugin directories [internal/plugin/discovery.go](internal/plugin/discovery.go#L31). But a malicious installed plugin is still arbitrary code executed as the operator. That is inherent in the current plugin model and must stay clearly documented.

### Remediation Path

1. Treat Docker runtime as an interim operator boundary, not a security claim for multi-tenant workflow authoring.
2. Add the Phase 3 environment-plug abstraction around the `exec.Command(path)` site [internal/plugin/loader.go](internal/plugin/loader.go#L119).
3. Add at least one platform-specific isolation implementation: Linux seccomp/cgroups or macOS sandbox-exec.
4. Keep `govulncheck` in CI rather than relying on ad-hoc evaluation runs.

## 5. SDK / Wire Contract - Grade: B

### Evidence

- Proto source defines the adapter plugin service and permanent field numbers [proto/criteria/v1/adapter_plugin.proto](proto/criteria/v1/adapter_plugin.proto#L8).
- W14 added `allowed_outcomes` as an additive field [proto/criteria/v1/adapter_plugin.proto](proto/criteria/v1/adapter_plugin.proto#L47), and the SDK changelog describes compatibility and bump rationale [sdk/CHANGELOG.md](sdk/CHANGELOG.md#L8).
- The host populates `AllowedOutcomes` from declared step outcomes, sorted for determinism [internal/plugin/loader.go](internal/plugin/loader.go#L204), [internal/plugin/loader.go](internal/plugin/loader.go#L308).
- Copilot consumes that field and validates `submit_outcome` against the active allowed set [cmd/criteria-adapter-copilot/copilot_turn.go](cmd/criteria-adapter-copilot/copilot_turn.go#L264), [cmd/criteria-adapter-copilot/copilot_outcome.go](cmd/criteria-adapter-copilot/copilot_outcome.go#L24).
- `make proto-check-drift` passes, and `make test-conformance` passes.

### Impact Assessment

The wire-contract process is mostly healthy. The additive proto change is implemented in the right direction: source proto first, generated bindings checked, host propagation tests, adapter consumption tests, and SDK changelog. This is exactly the kind of change an SDK project should be able to make pre-v1.

The unresolved risk is durable resume. The conformance suite explicitly skips `DurableAcrossRestart` [sdk/conformance/resume.go](sdk/conformance/resume.go#L42). That means the SDK cannot yet prove the hardest orchestrator recovery behavior it advertises.

### Remediation Path

1. Close the W14/W15 SDK bump in an actual `v0.3.0` tag.
2. Add a cross-repo conformance lane against the sibling orchestrator once durable resume exists there.
3. Keep every proto change paired with `make proto-check-drift` and conformance updates.

## 6. Release / Operations - Grade: C

### Evidence

- README still says pre-built binaries will be published with the first tagged release [README.md](README.md#L22), while CHANGELOG links to a `v0.2.0` GitHub release [CHANGELOG.md](CHANGELOG.md#L36). The tags do not exist in this repository state.
- CI has lint, baseline cap, race tests with `-count=2`, conformance, e2e validation, proto drift, and RC artifact jobs [.github/workflows/ci.yml](.github/workflows/ci.yml#L11).
- The RC artifact process explicitly says it does not create a GitHub Release, does not publish to a registry, and does not sign binaries [docs/contributing/release-process.md](docs/contributing/release-process.md#L1).
- Docker runtime build and smoke pass locally through `make docker-runtime-smoke` [Makefile](Makefile#L27).

### Impact Assessment

The operational automation is stronger than the release evidence. CI and Docker are real. The release process is not. A project cannot claim `v0.2.0` is current and tagged while neither local nor remote tags show that release. This is not cosmetic; it breaks install commands, changelog trust, and any downstream SDK consumer trying to pin the documented version.

### Remediation Path

1. Publish or correct the missing `v0.2.0` tag immediately. If it was intentionally not pushed, update README, PLAN, CHANGELOG, and prior evaluation language to say so.
2. Add a final release workflow distinct from RC artifacts: build, checksums, signing, GitHub Release, and Docker registry publish or explicit no-registry policy.
3. Add a CI/release check that docs cannot claim a tag unless `git ls-remote --tags origin refs/tags/<tag>` succeeds.

## 7. Maintainability - Grade: C+

### Evidence

- Recent contributor distribution remains concentrated: over six months, Dave Sanderson accounts for 152 of 157 commits across three emails; bots/agents account for the rest. Since the documented `v0.2.0` date, 16 of 17 commits are Dave Sanderson.
- The project now has a first-PR guide [docs/contributing/your-first-pr.md](docs/contributing/your-first-pr.md#L1) and W08 records a goal of at least two non-author humans by end of Phase 2 [workstreams/08-contributor-on-ramp.md](workstreams/08-contributor-on-ramp.md#L118).
- Workstream files are unusually detailed and include scope, tests, exit criteria, and reviewer notes [workstreams/README.md](workstreams/README.md#L36).
- The active roadmap itself points to a local plan file under `~/.claude/...` [workstreams/README.md](workstreams/README.md#L13), which is not acceptable as the durable public planning source.

### Impact Assessment

The single-human concentration is a real maintenance risk, but it should not dominate the verdict. The codebase now has test gates, docs, workstreams, and contributor material that reduce onboarding risk. The problem is that no non-author human contribution has actually landed yet, so the bus-factor risk remains theoretical-mitigated rather than empirically mitigated.

The local-only planning reference is a process smell. A public repo cannot depend on a plan path that only exists on one maintainer's machine.

### Remediation Path

1. Replace the local `~/.claude/...` plan reference with tracked repo material before `v0.3.0`.
2. Land at least two non-author human PRs by Phase 2 close. This matters less as vanity contributor count and more as proof the onboarding path works.
3. Keep workstream ownership and review notes, but shorten future workstream files once patterns are stable; very long process docs can become their own drag.

## 8. Tech Debt - Grade: C+

### Evidence

- PLAN explicitly carries forward platform-specific shell sandboxing, durable resume, parallel regions, `workflow_file` full runtime resolution, and lint baseline residuals [PLAN.md](PLAN.md#L109).
- The current lint baseline is capped but full (`70 / 70`), with residual complexity/correctness entries [.golangci.baseline.yml](.golangci.baseline.yml#L41).
- `workflow_file` is still a compile error without resolver [workflow/compile_steps.go](workflow/compile_steps.go#L358).
- Durable resume conformance is skipped [sdk/conformance/resume.go](sdk/conformance/resume.go#L42).
- Server-mode apply coverage is weak despite being mission-critical [internal/cli/apply.go](internal/cli/apply.go#L257).

### Impact Assessment

Debt is being managed, not ignored. That is the good news. The bad news is that some debt is now user-facing: release tags, `workflow_file`, durable resume, and server-mode coverage are not internal polish items. They affect adoption and credibility.

### Remediation Path

1. Make W16 a real cleanup gate, not an archive exercise.
2. Burn the baseline below 50 and require any cap increase to be a separate reviewed commit.
3. Prioritize user-visible half-features over further internal polish.

## 9. Performance / Scalability - Grade: B

### Evidence

- A published benchmark baseline exists for compile, engine run, and plugin execution [docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md#L1).
- Baseline numbers show linear engine growth: 10 steps, 100 steps, 1000 steps scale proportionally [docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md#L26).
- The engine uses sequential node evaluation; parallel regions are future work [docs/workflow.md](docs/workflow.md#L972).
- Server event publish uses bounded channels and backpressure rather than silent drops [internal/transport/server/client_streams.go](internal/transport/server/client_streams.go#L234).

### Impact Assessment

Performance is adequate for the current mission: local workflows, plugin-bound execution, and orchestrator-compatible event streaming. The current bottleneck in real workflows will be adapter subprocess/runtime behavior, not FSM dispatch. The scalability ceiling is functional rather than micro-performance: no parallel regions, no distributed scheduler in this repo, and no proof beyond benchmark-scale local runs.

### Remediation Path

1. Keep the >20% benchmark regression policy, but rerun it at Phase 2 close with current HEAD.
2. Add at least one benchmark for local approval/resume and iterating workflow steps, because those are new Phase 2 paths.
3. Do not claim Argo/Temporal-scale parallel execution until the scheduler exists.

## Tech Debt Register

1. **Release tag inconsistency.** Docs claim `v0.2.0` tagged; local/remote tag evidence does not. Severity: critical for public trust.
2. **No official release workflow.** RC artifacts exist, but docs state they are not releases and are unsigned. Severity: high.
3. **`workflow_file` half-feature.** Schema/compiler path exists; CLI lacks resolver. Severity: high for language credibility.
4. **Inline sub-workflow scope mismatch.** `WorkflowBodySpec` is not a true `Spec`; variables/agents/policy/permissions do not mirror top level. Severity: high for future composition.
5. **Durable resume conformance skipped.** Orchestrator restart durability remains unproven. Severity: high for orchestrator mission.
6. **No OS-level shell/plugin isolation.** Docker helps, but syscall/network/cgroup controls remain absent. Severity: high for untrusted workflow authors.
7. **Server-mode apply coverage hole.** `runApplyServer` and `executeServerRun` show 0% function coverage in `make test-cover`. Severity: medium-high.
8. **Lint baseline exactly at cap.** Current `70 / 70` leaves no debt budget and still includes complexity/correctness suppressions. Severity: medium.
9. **Large orchestrating files.** `apply.go`, `compile_steps.go`, `localresume/resumer.go`, and `node_step.go` remain large. Severity: medium.
10. **Maintainer concentration.** High velocity comes from one human maintainer plus bots/agents. Severity: medium; not a reason to stop, but a reason to demand contributor proof.
11. **Local-only roadmap reference.** `workstreams/README.md` points to `~/.claude/...`. Severity: medium process risk.
12. **No parallel execution.** Documented future work, not current capability. Severity: medium for scalability claims.

## Verdict

**Viable.** Criteria should continue. The current codebase is coherent, tested, and moving in the right direction for its mission. The velocity is high and mostly disciplined: the project is paying down prior debt while shipping user-visible capabilities, not merely adding features on unstable ground.

The viability caveat is strict: this is viable as a pre-v1 local workflow engine and SDK, not as a production-safe multi-tenant workflow runner and not as a cleanly released public artifact. The missing `v0.2.0` tag/release evidence is the immediate blocker. The second blocker is the unfinished sub-workflow story: `workflow_file` and full nested workflow scope need to be completed or removed from the advertised surface.

Required actions to keep the verdict viable:

1. Fix release reality: publish/correct `v0.2.0`, then close Phase 2 with a real `v0.3.0` tag and release process.
2. Close or explicitly defer public-facing half-features: `workflow_file`, nested workflow scope, durable resume, and parallel regions.
3. Prove maintainability beyond the primary author: land non-author human PRs and reduce lint baseline below 50.

## What Would Change the Verdict

### To Strong Viable

1. `v0.3.0` is tagged on remote, release artifacts are published with checksums/signing, and docs match tag reality.
2. `make ci`, `make proto-check-drift`, `make docker-runtime-smoke`, and a Phase 2 unattended smoke all pass from a clean clone.
3. Lint baseline is below 50 entries and no cap increase occurred during Phase 2.
4. `workflow_file` works from the CLI with resolver tests, or the feature is removed from public docs until Phase 3.
5. Server-mode apply/resume/cancel paths have meaningful integration coverage and no 0% functions on hot paths.
6. At least two non-author human PRs are merged.

### To Marginal

1. The `v0.2.0`/`v0.3.0` tag mismatch persists after cleanup.
2. The lint baseline cap is raised instead of burned down.
3. W16 archives Phase 2 without resolving `workflow_file` messaging and release evidence.
4. Server-mode coverage remains effectively untested while new server-facing behavior continues to land.

### To Not Viable

1. Tests or lint stop passing on `main` and the project proceeds with feature work anyway.
2. Security docs start claiming multi-tenant safety without OS-level isolation.
3. The maintainer becomes unavailable before non-author maintainers can build, release, and debug the project.
