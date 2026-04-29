# AGENTS.md

Repository guidance for AI coding agents working in this workspace.

## What this repo is

Criteria is a standalone workflow execution engine: HCL → finite-state machine
→ runner. It runs locally (no orchestrator) or against a server-compatible
orchestrator over the published `criteria-sdk` Connect/gRPC contract. The
sibling orchestrator lives in
[github.com/brokenbots/orchestrator](https://github.com/brokenbots/orchestrator) and
consumes this repo's published SDK; nothing in that repo is editable from
here.

## Scope and priorities

- Keep changes small and targeted; avoid broad refactors unless requested.
- Treat `proto/criteria/v1/` as the source of truth for the wire contract.
- Prefer linking existing docs over duplicating details.

## Quick start commands

- Bootstrap dependencies: `make bootstrap`
- Build the binary: `make build` (output at `bin/criteria`)
- Build adapter plugins: `make plugins`
- Run all Go tests: `make test`
- Run the SDK conformance suite alone: `make test-conformance`
- Validate all standalone example workflows: `make validate`
- Enforce import boundaries: `make lint-imports`
- Regenerate proto bindings: `make proto` (requires `buf`)
- Lint protos: `make proto-lint`

## Project map

- Wire contract: `proto/criteria/v1/*.proto` — generated Go in
  `sdk/pb/criteria/v1/`. Managed with `buf`.
- CLI entrypoint: [cmd/criteria/main.go](cmd/criteria/main.go)
- CLI commands: [internal/cli/compile.go](internal/cli/compile.go),
  [internal/cli/plan.go](internal/cli/plan.go),
  [internal/cli/apply.go](internal/cli/apply.go),
  [internal/cli/validate.go](internal/cli/validate.go)
- Engine node interpreters:
  [internal/engine/node_step.go](internal/engine/node_step.go),
  [internal/engine/node_wait.go](internal/engine/node_wait.go),
  [internal/engine/node_branch.go](internal/engine/node_branch.go),
  [internal/engine/node_workflow.go](internal/engine/node_workflow.go),
  [internal/engine/node_approval.go](internal/engine/node_approval.go)
- HCL parser / FSM compiler (Go sub-module): [workflow/](workflow/)
- Published SDK (Go sub-module): [sdk/](sdk/) — see
  [sdk/doc.go](sdk/doc.go) for the contract surface.
- SDK conformance suite: [sdk/conformance/](sdk/conformance/) — the
  in-memory reference Subject lives at
  [sdk/conformance/inmem_subject_test.go](sdk/conformance/inmem_subject_test.go).
- Adapter plugin loader (host side): [internal/plugin/](internal/plugin/)
- Bundled adapter plugins: [cmd/criteria-adapter-noop/](cmd/criteria-adapter-noop/),
  [cmd/criteria-adapter-copilot/](cmd/criteria-adapter-copilot/),
  [cmd/criteria-adapter-mcp/](cmd/criteria-adapter-mcp/)
- Project planning: [PLAN.md](PLAN.md), [workstreams/README.md](workstreams/README.md)

## Conventions agents should follow

- Go workspace uses three modules — root, `sdk/`, `workflow/` — wired
  through [go.work](go.work) plus `replace` directives in the root `go.mod`.
  Run commands from repo root using `make` targets when possible.
- **Wire contract changes**: edit a file under `proto/criteria/v1/` first,
  run `make proto` to regenerate the Go bindings, then update the
  in-tree call sites. Any change to the `Subject`/`ServiceHandler`
  surface or to event field numbers is a **breaking SDK change** —
  see [CONTRIBUTING.md](CONTRIBUTING.md) for the bump policy.
- **Plugin model**: adapter plugins run out-of-process and are discovered
  as `criteria-adapter-<name>` from `${CRITERIA_PLUGINS}/` first, then
  `~/.criteria/plugins/`. Use `make plugins` to build all bundled adapter
  binaries. The plugin handshake cookie is `CRITERIA_PLUGIN`.
- **HCL workflow syntax**: step-level adapter input uses `input { ... }`
  blocks; agent-level configuration stays on the `agent { }` block.
  The legacy `config = {...}` shape for step input is not accepted.
- **Local mode constraints**: `wait { signal = "..." }` and `approval { ... }`
  nodes require a server-compatible orchestrator (`criteria apply --server ...`).
  Local-only execution rejects these node kinds with a clear error.
- **Workstream Reviewer role**: the reviewer agent is an audit-only
  quality gate and must not edit code; it enforces quality, security, and
  acceptance bars, validates that tests prove intended behavior (not just
  that they pass), and requires the executor to remediate all findings
  including nits before approval.
- **Files reviewer/executor agents may NOT modify**: `README.md`,
  `PLAN.md`, `AGENTS.md`, and any workstream files other than the one
  the agent is currently working on. The cleanup agent (or a human) is
  the only writer for these.
- Keep logs structured (`slog` JSON style in entrypoints).
- Preserve existing adapter boundaries (`internal/adapter`,
  `internal/adapters/*`, `internal/plugin`). Do not import `sdk/` from
  `internal/` — `sdk/pb/...` is the only permitted reach into the SDK
  tree (enforced by `make lint-imports`).

## Common pitfalls

- Copilot adapter execution requires installing `criteria-adapter-copilot`
  into `${CRITERIA_PLUGINS}/` or `~/.criteria/plugins/`, plus the
  `copilot` CLI on `PATH` (or pointed at via `CRITERIA_COPILOT_BIN`).
  There is no in-binary adapter code.
- Server run/event ordering depends on server-assigned monotonic `seq`
  per `run_id`; avoid client-side ordering assumptions.
- Avoid introducing CGO-only SQLite dependencies; current storage uses
  pure-Go `modernc.org/sqlite`.
- Prefer `make test` over ad-hoc partial test runs unless task scope is
  clearly limited.
- `proto/criteria/v1/server.proto` exists in this repo because the CLI
  client embeds ServerService stubs (`status`, `stop`); the orchestrator
  side of ServerService is implemented in the orchestrator repo.
