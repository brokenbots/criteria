# Changelog

All notable changes to Criteria are recorded here.

## [v0.2.0] — 2026-04-29

### Headline: stabilization phase — deterministic CI, golangci-lint, shell adapter hardening, and four user-blocking fixes (file(), step-level iteration with nested workflow step, Copilot agent defaults)

Phase 1 closes with this release. The focus was hardening CI, adopting a per-workstream lint burn-down contract, sandboxing the shell adapter, shipping coverage/benchmark/GoDoc baselines, and unblocking four user-reported gaps.

- **W01** — Deterministic CI: `go test -count=2` in CI (`goleak` for goroutine-leak checks). Flaky race in `internal/engine` and `internal/plugin` eliminated.
- **W02** — golangci-lint adoption with `.golangci.baseline.yml` and a per-workstream burn-down contract documented in [docs/contributing/lint-baseline.md](docs/contributing/lint-baseline.md). `make lint-go` is now a hard PR gate.
- **W03** — God-function refactor: `resumeOneRun`, `copilotPlugin.Execute`, `Engine.runLoop`, and `runApplyServer` each split into ≤ 50-line single-concern helpers. No behavior change.
- **W04** — Oversized-file splits in `workflow/compile.go`, `internal/adapter/conformance/`, and `internal/transport/server/`. No behavior change.
- **W05** — Shell adapter first-pass hardening: configurable allow/deny list, PATH restriction, env-var filtering. `CRITERIA_SHELL_LEGACY=1` opt-out available. Threat model at [docs/security/shell-adapter-threat-model.md](docs/security/shell-adapter-threat-model.md).
- **W06** — Coverage thresholds (`internal/cli` ≥ 60%, `internal/run` ≥ 60%, `cmd/criteria-adapter-mcp` ≥ 50%), benchmark baselines, and GoDoc on all public packages. Performance baseline at [docs/perf/baseline-v0.2.0.md](docs/perf/baseline-v0.2.0.md).
- **W07** — `file()`, `fileexists()`, `trimfrontmatter()` HCL expression functions. `CRITERIA_FILE_FUNC_MAX_BYTES` and `CRITERIA_WORKFLOW_ALLOWED_PATHS` env-var controls.
- **W08** — Multi-step `for_each` iteration bodies (top-level `for_each "name" { ... }` block). **Superseded within Phase 1 by W10**; the user story remains satisfied via W10's step-level model.
- **W09** — Copilot `reasoning_effort` no longer silently dropped; per-step override semantics; targeted diagnostic for misplaced agent-config fields.
- **W10** — `for_each` and `count` are now step-level fields (any step type); new `type = "workflow"` step holds a nested workflow body inline or via `workflow_file`; indexed outputs (`steps.foo[i]` / `steps.foo["k"]`); full `each.*` bindings (`value`, `key`, `_idx`, `_first`, `_last`, `_total`, `_prev`); `on_failure = "abort"|"continue"|"ignore"`; explicit `output { name=...; value=... }` blocks for encapsulation. **Removes** the W08 top-level `for_each` block syntax — existing W08 workflows must migrate to step-level `for_each`.

### Migration notes

- **W05**: Any shell workflow that relied on unrestricted PATH or broad env passthrough may need `CRITERIA_SHELL_LEGACY=1` while migrating to explicit allow-lists.
- **W09**: `reasoning_effort` on a step that specifies no `model` now produces a diagnostic and the field is rejected (previously silently dropped). Fix: add a `model` field or move `reasoning_effort` to the agent config block.
- **W10**: The W08 top-level `for_each "name" { ... }` block syntax is removed. Migrate by moving `for_each` (with the list value) to the step declaration: `step "name" { for_each = [...]; ... }`.

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
