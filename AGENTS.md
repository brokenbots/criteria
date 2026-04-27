# AGENTS.md

Repository guidance for AI coding agents working in this workspace.

## Scope and priorities

- Keep changes small and targeted; avoid broad refactors unless requested.
- Treat `proto/overlord/v1/` as the source of truth for cross-component behavior.
- Prefer linking existing docs over duplicating details.

## Quick start commands

- Bootstrap dependencies: `make bootstrap`
- Build all components: `make build`
- Run all Go tests: `make test`
- Validate all example workflows: `make validate`

Component-specific:

- Run Castle (dev): `make dev-castle`
- Run Overseer (dev example): `make dev-overseer`
- Run Parapet dev server: `make dev-parapet`
- Build UI only: `cd parapet && npm run build`

## Project map

- Contracts: `proto/overlord/v1/*.proto` (source of truth; generated Go in `shared/pb/`, TS in `parapet/src/gen/`). Managed with `buf`. See [api/README.md](api/README.md) for protocol notes.
- Castle server (Connect/gRPC + SQLite): [castle/](castle)
- Overseer CLI and execution engine: [overseer/](overseer)
- Workflow parser/compiler (HCL -> FSM): [workflow/](workflow)
- Shared event helpers over generated protobuf: [shared/events/types.go](shared/events/types.go)
- Frontend app (Vite/React/TS, `@connectrpc/connect-web`): [parapet/](parapet)
- Architecture and phase details: [README.md](README.md), [PLAN.md](PLAN.md), [WORKSTREAM.md](WORKSTREAM.md)

## Conventions agents should follow

- Go workspace uses multiple modules via [go.work](go.work); run commands from repo root using `make` targets when possible.
- **Wire contract changes**: edit `proto/overlord/v1/*.proto` first; run `make proto` to regenerate Go and TS clients; then update Castle handlers, Overseer client, and Parapet call sites.
- **Plugin model**: adapter plugins run out-of-process and are discovered as `overlord-adapter-<name>` from `${OVERLORD_PLUGINS}/` first, then `~/.overlord/plugins/`; use `make plugins` to build all adapter binaries.
- **HCL workflow syntax (Phase 1.5)**: step-level adapter input uses `input { ... }` blocks; agent-level configuration stays on the `agent { }` block. The legacy `config = {...}` shape for step input was removed in a hard break and is no longer accepted.
- **Workstream Reviewer role**: the reviewer agent is an audit-only quality gate and must not edit code; it enforces quality, security, and acceptance bars, validates that tests prove intended behavior (not just that they pass), and requires the executor to remediate all findings including nits before approval.
- Keep logs structured (`slog` JSON style in backend entrypoints).
- Preserve existing adapter boundaries in Overseer (`internal/adapter`, `internal/adapters/*`, `internal/dispatcher`).

## Common pitfalls

- Copilot adapter execution requires installing `overlord-adapter-copilot` into `${OVERLORD_PLUGINS}/` or `~/.overlord/plugins/`; do not assume in-binary adapter code.
- Castle run/event ordering depends on server-assigned monotonic `seq` per `run_id`; avoid client-side ordering assumptions.
- **Local mode constraints (Phase 1.5)**: `wait { signal = "..." }` and `approval { ... }` nodes require a Castle instance (`overseer apply --castle`). Local-only execution (`overseer apply` with no `--castle`) rejects these node types with a clear error message.
- Prefer `make test` over ad-hoc partial test runs unless task scope is clearly limited.
- Avoid introducing CGO-only SQLite dependencies; current storage uses pure-Go `modernc.org/sqlite`.

## High-value files for orientation

- Build and dev workflows: [Makefile](Makefile)
- Castle entrypoint: [castle/cmd/castle/main.go](castle/cmd/castle/main.go)
- Overseer entrypoint: [overseer/cmd/overseer/main.go](overseer/cmd/overseer/main.go)
- Overseer CLI commands (Phase 1.5): [overseer/internal/cli/compile.go](overseer/internal/cli/compile.go), [overseer/internal/cli/plan.go](overseer/internal/cli/plan.go), [overseer/internal/cli/apply.go](overseer/internal/cli/apply.go)
- Engine node execution (Phase 1.5): [overseer/internal/engine/node_step.go](overseer/internal/engine/node_step.go), [overseer/internal/engine/node_wait.go](overseer/internal/engine/node_wait.go), [overseer/internal/engine/node_branch.go](overseer/internal/engine/node_branch.go), [overseer/internal/engine/node_for_each.go](overseer/internal/engine/node_for_each.go), [overseer/internal/engine/node_approval.go](overseer/internal/engine/node_approval.go)
- Workflow evaluation context (Phase 1.5): [workflow/eval.go](workflow/eval.go)
- Plugin wire contract: [proto/overlord/v1/adapter_plugin.proto](proto/overlord/v1/adapter_plugin.proto)
- Plugin loader/session manager: [overseer/internal/plugin/](overseer/internal/plugin)
- Workflow schema/compiler surface: [workflow/schema.go](workflow/schema.go)
- UI scripts/deps: [parapet/package.json](parapet/package.json)
