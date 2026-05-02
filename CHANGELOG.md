# Changelog

All notable changes to Criteria are recorded here.

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
