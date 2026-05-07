# Changelog

All notable changes to Criteria are recorded here.

## [v0.3.0] — 2026-05-06 — Phase 3: HCL/runtime rework, subworkflow features, clean break from v0.2.0

**Headline**: Clean break from v0.2.0: HCL language rework, subworkflows first-class, automatic adapter lifecycle, parallel execution, shared variables, top-level outputs, and environment blocks. **All v0.2.0 workflows must be updated before running on v0.3.0.**

### Phase 3 workstreams completed (W01–W19; W20 skipped)

**W01 — Lint baseline burn-down.** Lint baseline reduced to 21 entries (down from 50+); no `errcheck` or `contextcheck` entries per architectural contract. Coverage floors raised: `internal/cli` ≥65%, `internal/engine` ≥80%, `internal/plugin` ≥70%, `workflow` ≥75%, `sdk` ≥75%, `sdk/conformance` ≥80%. Maintainability and Tech Debt grades lifted from C+ to B.

**W02 — Split CLI apply.** `internal/cli/apply.go` split into focused: `apply.go` (entry point), `apply_compile.go`, `apply_execute.go`. No behavior change.

**W03 — Split compile steps.** `workflow/compile_steps.go` split by step-kind lines into `compile_step_foreach.go`, `compile_step_workflow.go`, `compile_step_approval.go`, `compile_step_wait.go`. No behavior change.

**W04 — Server-mode apply test coverage.** Transport `server/` package coverage raised from 63.4% to 70%; previously 0% functions (`executeServerRun`, `runApplyServer`, `setupServerRun`, `drainResumeCycles`) now at ≥60% each.

**W05 — Tracked roadmap artifact.** `docs/roadmap/phase-2-summary.md` replaces local `~/.claude/...` plan reference. Phase 2 and earlier phases have permanent summary documents.

**W06 — Release process integrity.** Added `tag-claim-check` CI job validating that claimed tags in CHANGELOG, README, and release-notes match remote; workflow produces per-os/arch tarballs, runtime image, and cosigned SHA256SUMS on tag push. Real release workflow (was previously RC-only).

**W07 — Local variables and compile-time fold.** New `local "<name>" { value = ... }` block for compile-time constants. Compile pass folds `local.*` references and reports undeclared `var.*` as errors (no runtime inference). `file()` function broadened for more use cases.

**W08 — Schema unification.** Removed `WorkflowBodySpec` complexity; subworkflows ARE Specs. Implicit cross-scope `Vars` aliasing removed (`childSt.Vars = st.Vars` pattern eliminated). Breaking: undeclared variable references now compile errors instead of runtime nil-coercion.

**W09 — Top-level output block.** New `output "<name>" { type = ..., value = ... }` block at workflow root. Emitted as `run.outputs` event. Replaces step-only output model. Full type system (number, string, list(string), etc.).

**W10 — Environment block.** New `environment "<type>" "<name>" { variables = { ... } }` declaration; injected into adapter subprocess via env vars. Shell adapter passes as `VAR=value` in command environment.

**W11 — Agent to adapter hard rename.** **Breaking.** `agent "name"` block → `adapter "<type>" "<name>"` block. `step.agent = "name"` → `step.target = adapter.<type>.<name>`. Proto field rename `agent_name` → `adapter` (field number stable). Wire contract updated.

**W12 — Adapter lifecycle automation.** **Breaking.** `lifecycle = "open"|"close"` step attribute removed. Adapters now auto-open at scope start, auto-close on exit (LIFO). Closes managed exclusively by engine; explicit step-level control removed. Improves safety; enables session pooling in future.

**W13 — First-class subworkflow block.** New `subworkflow "<name>" { source = "path" }` top-level block. Inline `step.workflow { ... }` and `step.workflow_file = ...` removed. **Breaking.** Subworkflows resolved at compile time via `SubWorkflowResolver` interface; CLI wired via `--subworkflow-root` flag.

**W14 — Universal step target.** **Breaking.** Unified `step.target = adapter.<type>.<name> | subworkflow.<name>` (replaces `step.adapter`, `step.workflow`, `step.agent`, `step.type`). Bareword adapter references and `type = "workflow"` removed.

**W15 — Outcome block with return and output projection.** **Breaking.** `outcome "name" { next = ..., output = {...} }` replaces `transition_to`. Reserved `return` outcome for early exit. `outcome.output` projection on matched outcomes. `default_outcome` attribute for fallthrough. Full type preservation through outcome outputs.

**W16 — Switch and if flow control.** **Breaking.** `branch { arm { ... } }` → `switch { condition { match = ..., next = ..., output = {...} } }`. `if` deferred to Phase 4. Condition outputs preserved; each arm is independent state transition.

**W17 — Directory-mode modules and workflow header.** **Breaking.** Single-file entry point removed; directory-only mode (all .hcl files in directory merged into module). Workflow-level attributes now wrapped in `workflow "<name>" { ... }` block (was: bare top-level `name`, `version`, `initial_state`, `target_state`).

**W18 — Shared variable block.** New `shared_variable "<name>" { type = ..., initial = ... }` block. Mutable scoped state locked by engine during concurrent step iterations. RPC `SetSharedVariable` deferred (in-memory implementation complete). Enables multi-iteration coordination.

**W19 — Parallel step modifier.** New `parallel = [list]` attribute on steps. Each iteration independent; results merged by outcome. Built-in `each.value`, `each.index` binding. Adapter sessions are per-iteration. Fully concurrent with race detector clean.

**W20 — Implicit input chaining (SKIPPED).** Deferred to Phase 4 due to architecture concerns about failed plan risk. Default `step.input` to previous step output deferred.

### Breaking changes (from v0.2.0)

Every entry below is a **hard error** on v0.3.0+ if used:

- **`agent` block** (removed): Use `adapter "<type>" "<name>" { ... }` instead.
- **`step.agent` attribute** (removed): Use `step.target = adapter.<type>.<name>` instead.
- **`step.adapter` attribute** — **Both forms removed**:
  - `step.adapter = "shell"` (bare type) → declare `adapter "shell" "default" { config { } }` then `step.target = adapter.shell.default`.
  - `step.adapter = "shell.default"` (quoted string) → same as above.
- **`step.lifecycle` attribute** (removed): Lifecycle is automatic. Remove the attribute.
- **`step.workflow { ... }` inline block** (removed): Use top-level `subworkflow` block instead.
- **`step.workflow_file` attribute** (removed): Use top-level `subworkflow` block instead.
- **`step.type = "workflow"` attribute** (removed): Use `step.target = subworkflow.<name>` instead.
- **`branch { arm { ... } }` block** (removed): Use `switch { condition { ... } }` instead.
- **`transition_to` attribute** (removed everywhere): Use `next` in `outcome` blocks instead.
- **Top-level workflow attributes outside `workflow` block** (removed): Wrap in `workflow "<name>" { version = ..., initial_state = ..., target_state = ... }` block.

### Migration guide: v0.2.0 → v0.3.0

#### Adapter model

**v0.2.0:**
```hcl
agent "reviewer" {
    adapter = "copilot"
    config { reasoning_effort = "high" }
}
step "review" {
    agent = "reviewer"
    input { ... }
    outcome "approved" { transition_to = "deploy" }
}
```

**v0.3.0:**
```hcl
adapter "copilot" "reviewer" {
    config = { reasoning_effort = "high" }
}
step "review" {
    target = adapter.copilot.reviewer
    input = { ... }
    outcome "approved" { next = "deploy" }
}
```

#### Outcomes and transitions

**v0.2.0:**
```hcl
outcome "success" { transition_to = "next_state" }
outcome "failure" { transition_to = "error_state" }
```

**v0.3.0:**
```hcl
outcome "success" { next = "next_state" }
outcome "failure" { next = "error_state" }
outcome "custom" { next = "return" }  # Early exit with reserved "return"
```

#### Subworkflows

**v0.2.0:**
```hcl
step "run_sub" {
    type = "workflow"
    workflow_file = "subworkflows/process.hcl"
}
```

**v0.3.0:**
```hcl
subworkflow "process" {
    source = "./subworkflows/process"  # Directory mode
}
step "run_sub" {
    target = subworkflow.process
}
```

#### Branching

**v0.2.0:**
```hcl
branch "decide" {
    arm "if_approved" {
        match = condition.value == "yes"
        transition_to = "deploy"
    }
    arm "if_rejected" {
        match = condition.value == "no"
        transition_to = "closed"
    }
    default { transition_to = "review" }
}
```

**v0.3.0:**
```hcl
switch "decide" {
    condition "approved" {
        match = condition.value == "yes"
        next = "deploy"
    }
    condition "rejected" {
        match = condition.value == "no"
        next = "closed"
    }
    condition "default" {
        match = true
        next = "review"
    }
}
```

#### Workflow structure

**v0.2.0:**
```hcl
workflow "myflow" {
    version = "0.1"
    initial_state = "start"
    target_state = "end"
}
# Steps, states at top level
```

**v0.3.0:**
```hcl
workflow "myflow" {
    version = "0.1"
    initial_state = "start"
    target_state = "end"
}
# Steps, states at top level (no change)
# BUT: workflow must be in a directory with only .hcl files
```

### New features

- **Parallel execution**: `parallel = [list]` modifier on steps. Full concurrency, per-iteration adapter sessions, race-clean.
- **Shared variables**: `shared_variable` block for mutable scoped state. Engine-locked during concurrent iterations.
- **Top-level outputs**: `output` blocks with full type system. Emitted as `run.outputs` event.
- **Local variables**: `local` blocks for compile-time constants. Undeclared `var.*` are now errors.
- **Environment blocks**: `environment "<type>" "<name>"` for subprocess environment injection.
- **Subworkflows first-class**: `subworkflow` block replaces inline/attribute model.
- **Outcome output projection**: `outcome.output` carries data through transitions. Reserved `return` outcome.
- **Automatic adapter lifecycle**: Adapters open/close automatically; no explicit lifecycle control.
- **Directory-mode modules**: Single-file entry point removed; workflows must be in directories.
- **Workflow header block**: Attributes like `version`, `initial_state` now inside `workflow` block.

### SDK / Wire contract changes

- **Proto**: `pb.StepEntered.adapter` field (replaces `agent_name`, field number 2 unchanged for wire format).
- **SDK**: `SubWorkflowResolver` interface for pluggable subworkflow resolution.
- **Events**: New `run.outputs` event payload emitted at terminal state.
- **Fields**: `pb.Step.target` (new universal target) replaces `step.adapter`, `step.agent`, `step.workflow*`.

### Removed surface (clean break)

**HCL syntax no longer accepted:**
- Top-level `agent` block and `step.agent` attribute.
- `step.adapter` (both quoted-string and type-only forms).
- `step.lifecycle` attribute.
- Inline `step.workflow { ... }` and `step.workflow_file` attribute.
- `type = "workflow"` on steps.
- `branch` block and `arm` sub-block.
- `transition_to` attribute (everywhere).
- Top-level workflow attributes (`name`, `version`, `initial_state`, `target_state`) outside `workflow` block.
- Implicit cross-scope `Vars` aliasing.
- Single-file workflow entry point (directory-only now).

### Behavior changes

- Adapters open on scope entry, close on scope exit (LIFO). No explicit lifecycle control.
- Implicit cross-scope variable aliasing removed; all variable references must be in-scope.
- Workflows must be directories (all .hcl files merged); single .hcl files rejected.

### Tests and examples

- New example: `examples/phase3-marquee/` demonstrates all Phase 3 features (parallel, outputs, lifecycle, environment).
- Conformance suite: New `LifecycleAutomatic` test for adapter session events.
- All Phase 3 examples validated and working.

### Release artifacts

- Per-os/arch tarballs (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64).
- `criteria-runtime-v0.3.0.tar` (Docker runtime image).
- `SHA256SUMS` with cosign signature.
- GitHub Release with all artifacts and signed checksums.

## [v0.2.0] — 2026-05-02 — Phase 1 + Phase 2 combined release
- **Backward compatibility**: Wire format is stable (same field number). Readers upgrade automatically; writers must rebuild. Considered a **minor** version bump for pre-1.0 projects.

## [v0.2.0] — 2026-05-02

### Headline: combined Phase 1 (stabilization) + Phase 2 (maintainability + unattended MVP + Copilot tool-call finalization)

This is the first tag pushed to remote since `v0.1.0`. It bundles two phases of work that merged into `main` over April–May 2026 but were never separately tagged. Earlier project documentation referenced `v0.2.0` as if it had shipped at the Phase 1 close; that tag had not been pushed at that time and is created here at HEAD with the combined Phase 1 + Phase 2 scope.

#### Phase 1 — Stabilization (W01–W11, closed 2026-04-29)

Hardening CI, adopting a per-workstream lint burn-down contract, sandboxing the shell adapter, shipping coverage/benchmark/GoDoc baselines, and unblocking four user-reported gaps.

- **P1-W01** — Deterministic CI: `go test -count=2` in CI (`goleak` for goroutine-leak checks). Flaky race in `internal/engine` and `internal/plugin` eliminated.
- **P1-W02** — golangci-lint adoption with `.golangci.baseline.yml` and a per-workstream burn-down contract documented in [docs/contributing/lint-baseline.md](docs/contributing/lint-baseline.md). `make lint-go` is now a hard PR gate.
- **P1-W03** — God-function refactor: `resumeOneRun`, `copilotPlugin.Execute`, `Engine.runLoop`, and `runApplyServer` each split into ≤ 50-line single-concern helpers. No behavior change.
- **P1-W04** — Oversized-file splits in `workflow/compile.go`, `internal/adapter/conformance/`, and `internal/transport/server/`. No behavior change.
- **P1-W05** — Shell adapter first-pass hardening: configurable allow/deny list, PATH restriction, env-var filtering. `CRITERIA_SHELL_LEGACY=1` opt-out available *(removed in this same release by P2-W10 below)*. Threat model at [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md).
- **P1-W06** — Coverage thresholds (`internal/cli` ≥ 60%, `internal/run` ≥ 60%, `cmd/criteria-adapter-mcp` ≥ 50%), benchmark baselines, and GoDoc on all public packages. Performance baseline at [docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md).
- **P1-W07** — `file()`, `fileexists()`, `trimfrontmatter()` HCL expression functions. `CRITERIA_FILE_FUNC_MAX_BYTES` and `CRITERIA_WORKFLOW_ALLOWED_PATHS` env-var controls.
- **P1-W08** — Multi-step `for_each` iteration bodies (top-level `for_each "name" { ... }` block). **Superseded within Phase 1 by P1-W10**; the user story remains satisfied via P1-W10's step-level model.
- **P1-W09** — Copilot `reasoning_effort` no longer silently dropped; per-step override semantics; targeted diagnostic for misplaced agent-config fields.
- **P1-W10** — `for_each` and `count` are now step-level fields (any step type); new `type = "workflow"` step holds a nested workflow body inline or via `workflow_file`; indexed outputs (`steps.foo[i]` / `steps.foo["k"]`); full `each.*` bindings (`value`, `key`, `_idx`, `_first`, `_last`, `_total`, `_prev`); `on_failure = "abort"|"continue"|"ignore"`; explicit `output { name=...; value=... }` blocks for encapsulation. **Removes** the P1-W08 top-level `for_each` block syntax — existing P1-W08 workflows must migrate to step-level `for_each`.
- **P1-W11** — Phase 1 cleanup gate.

#### Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization (W01–W15, closed 2026-05-02)

Lifting Maintainability and Tech Debt from C+/C toward B, the smallest set of capabilities that allow unattended end-to-end execution (local-mode approval + per-step `max_visits`), replacing the Copilot adapter's brittle prose-parsed outcome with a structured `submit_outcome` tool call, establishing Docker as the interim runtime sandbox, honoring the `CRITERIA_SHELL_LEGACY=1` removal commitment, and absorbing five deferred user-feedback items (UF#02, UF#03, UF#05, UF#06, UF#08).

Two original Phase 2 workstreams were cancelled on 2026-04-30:

- **P2-W05** (`SubWorkflowResolver` CLI wiring) — deferred to Phase 3. The compile-time gap remains a known forward-pointer; the example `examples/workflow_step_compose.hcl` does not ship with this release.
- **P2-W11** (reviewer outcome aliasing — host-side `outcome_aliases` HCL block) — cancelled. UF#03 is now addressed at the source by P2-W14 + P2-W15 below.

Active set:

- **P2-W01** — Lint baseline mechanical burn-down (gofmt/goimports/unused; reclassify proto-generated `revive`).
- **P2-W02** — Lint CI gate (baseline-stays-flat enforcement via `tools/lint-baseline/cap.txt`).
- **P2-W03** — Split `copilot.go` (793 LOC) into focused siblings; Copilot permission-kind alias (UF#02).
- **P2-W04** — `~/.criteria/` directory mode hardened to `0o700`.
- **P2-W06** — Local-mode approval and signal wait via `CRITERIA_LOCAL_APPROVAL` (UF#05).
- **P2-W07** — Per-step `max_visits` to bound runaway loops (UF#08).
- **P2-W08** — Contributor on-ramp: `docs/contributing/your-first-pr.md`, `good-first-issue` labels, numeric Phase 2 contributor goal in PLAN.
- **P2-W09** — VS Code dev container + operator runtime image (`Dockerfile.runtime`) as the interim runtime sandbox.
- **P2-W10** — `CRITERIA_SHELL_LEGACY=1` shell-sandbox opt-out **removed**, honoring the v0.2.0 threat-model commitment. Setting the env var no longer affects sandbox enforcement. Behavior change disclosed in [docs/security/shell-adapter-threat-model.md §6](docs/security/shell-adapter-threat-model.md).
- **P2-W12** — Adapter lifecycle log clarity; new `OnAdapterLifecycle` sink hook (UF#06).
- **P2-W13** — Release-candidate artifact upload on PRs marked `release/*` or with `-rc<N>` titles.
- **P2-W14** — Copilot tool-call wire contract: additive `pb.ExecuteRequest.allowed_outcomes` (field 4); SDK bump per [sdk/CHANGELOG.md](sdk/CHANGELOG.md).
- **P2-W15** — Copilot `submit_outcome` adapter: structured tool-call outcome finalization with 3-attempt reprompt; `result:` prose parsing **removed** (UF#03). **Behavior change** — invalid finalize / max-turns / permission-denied now return `failure` rather than `needs_review`.

### Migration notes

- **P1-W05**: Any shell workflow that relied on unrestricted PATH or broad env passthrough must migrate to explicit allow-lists. The `CRITERIA_SHELL_LEGACY=1` escape hatch existed in Phase 1 but is **removed** in this same release by P2-W10 — there is no transitional path on a single release boundary.
- **P1-W09**: `reasoning_effort` on a step that specifies no `model` now produces a diagnostic and the field is rejected (previously silently dropped). Fix: add a `model` field or move `reasoning_effort` to the agent config block.
- **P1-W10**: The P1-W08 top-level `for_each "name" { ... }` block syntax is removed. Migrate by moving `for_each` (with the list value) to the step declaration: `step "name" { for_each = [...]; ... }`.
- **P2-W10**: `CRITERIA_SHELL_LEGACY=1` is no longer recognized. The Phase 1 sandbox defaults are now unconditional. Audit existing shell workflows for unrestricted-PATH or env-passthrough assumptions before upgrading; see [docs/security/shell-adapter-threat-model.md §6](docs/security/shell-adapter-threat-model.md) for the full migration checklist.
- **P2-W15**: Copilot adapter terminal outcomes are now derived from a structured `submit_outcome` tool call, not from `result:` prose. Workflows whose Copilot steps used an outcome name not declared in the workflow's `step.outcome` set will now finalize with `failure` (after three reprompt attempts) rather than `needs_review`. Declare every outcome the model is allowed to choose in the step's `outcome` blocks.

### Install

```sh
go install github.com/brokenbots/criteria/cmd/criteria@v0.2.0
```

Requires Go 1.26 or later.

[v0.2.0]: https://github.com/brokenbots/criteria/releases/tag/v0.2.0

## [v0.1.0] — 2026-04-27

### Headline: project renamed from Overseer to Criteria

Phase 0 of the post-separation cleanup closes with this release. The
key change is an ADR-0001-driven full brand rename. Everything visible
to users — binary names, env vars, module path, proto package, state
directory — has moved from the `overseer` namespace to `criteria`.

### New module path

```
github.com/brokenbots/criteria
```

All three Go modules (`root`, `sdk/`, `workflow/`) updated.

### Binary names

| Old | New |
|-----|-----|
| `overseer` | `criteria` |
| `overseer-adapter-noop` | `criteria-adapter-noop` |
| `overseer-adapter-copilot` | `criteria-adapter-copilot` |
| `overseer-adapter-mcp` | `criteria-adapter-mcp` |

### Environment variables

All `OVERSEER_*` variables renamed to `CRITERIA_*`:

| Old | New |
|-----|-----|
| `OVERSEER_PLUGINS` | `CRITERIA_PLUGINS` |
| `OVERSEER_PLUGIN` (handshake cookie) | `CRITERIA_PLUGIN` |
| `OVERSEER_COPILOT_BIN` | `CRITERIA_COPILOT_BIN` |
| `OVERSEER_SERVER` | `CRITERIA_SERVER` |
| `OVERSEER_RUN_ID` | `CRITERIA_RUN_ID` |
| `OVERSEER_EVENTS_FILE` | `CRITERIA_EVENTS_FILE` |
| `OVERSEER_STATE_DIR` | `CRITERIA_STATE_DIR` |

### State directory

Local run state moved from `~/.overseer/` to `~/.criteria/`.

To migrate existing run state:

```sh
mv ~/.overseer ~/.criteria
```

### Proto package

```
overseer.v1  →  criteria.v1
```

Generated Go bindings: `sdk/pb/criteria/v1/`.
Connect service name: `criteria.v1.CriteriaService`.

### What else shipped in Phase 0

- **W02** — README and CONTRIBUTING rewrites for general-purpose audience.
- **W03** — Public plugin-author SDK (`sdk/pluginhost`) extracted from
  `internal/plugin/`; external authors no longer need to import internal packages.
- **W04** — Shell adapter sandboxing: configurable allow/deny list, PATH
  restriction, and env-var filtering.
- **W05** — Copilot adapter E2E suite in the default test lane.
- **W06** — Standalone third-party plugin example (`examples/plugins/greeter`)
  demonstrating the `sdk/pluginhost` public API.
- **W07** — LICENSE (MIT), SECURITY.md, CODEOWNERS, issue/PR templates,
  Dependabot config.

### Install

```sh
go install github.com/brokenbots/criteria/cmd/criteria@v0.1.0
```

Requires Go 1.26 or later. Note: the GitHub repository rename from
`brokenbots/overseer` to `brokenbots/criteria` is a pending operator
action; GitHub serves redirects from the old URL.

### SDK conformance note

The in-repo conformance suite (`sdk/conformance`) passes against the
in-memory reference Subject. Cross-repo conformance against the
orchestrator-side implementation is transiently affected until the
orchestrator's paired rename PR lands — tracked separately and does
not block this release.

[v0.1.0]: https://github.com/brokenbots/criteria/releases/tag/v0.1.0
