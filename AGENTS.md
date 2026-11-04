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
  [internal/engine/node_switch.go](internal/engine/node_switch.go),
  [internal/engine/node_workflow.go](internal/engine/node_workflow.go),
  [internal/engine/node_approval.go](internal/engine/node_approval.go)
- HCL parser / FSM compiler (Go sub-module): [workflow/](workflow/)
- Published SDK (Go sub-module): [sdk/](sdk/) — see
  [sdk/doc.go](sdk/doc.go) for the contract surface.
- SDK conformance suite: [sdk/conformance/](sdk/conformance/) — the
  in-memory reference Subject lives at
  [sdk/conformance/inmem_subject_test.go](sdk/conformance/inmem_subject_test.go).
- Host-side adapter loader, OCI cache, signing, manifest, environments: [internal/adapter/](internal/adapter/)
- Tunable timing/budget settings: [internal/tunables/](internal/tunables/) —
  one typed `Settings` + defaults + the registry of operator-facing env
  overrides; the generated table lives in [docs/env-vars.md](docs/env-vars.md).
- In-tree adapters: [cmd/criteria-adapter-mcp/](cmd/criteria-adapter-mcp/)
  (copilot, shell, and noop were extracted to their own repos)

## Conventions agents should follow

- Go workspace uses three modules — root, `sdk/`, `workflow/` — wired
  through [go.work](go.work) plus `replace` directives in the root `go.mod`.
  Run commands from repo root using `make` targets when possible.
- **Wire contract changes**: edit a file under `proto/criteria/v1/` first,
  run `make proto` to regenerate the Go bindings, then update the
  in-tree call sites. Any change to the `Subject`/`ServiceHandler`
  surface or to event field numbers is a **breaking SDK change** and
  requires an SDK major-version bump.
- **Adapter model**: adapters run out-of-process and are distributed as signed
  OCI artifacts, pulled into `$CRITERIA_HOME/cache/oci` (default
  `~/.local/criteria/cache/oci`) and pinned per workflow in `.criteria.lock.hcl`.
  The adapter wire protocol is **v2** and lives in the external
  [`criteria-adapter-proto`](https://github.com/brokenbots/criteria-adapter-proto)
  repo; the in-tree `proto/criteria/v1` is the unrelated server/run API. For
  local iteration, `criteria adapter dev <binary>` registers a binary directly.
- **HCL workflow syntax**: step-level adapter input uses `input { ... }`
  blocks; adapter-level configuration stays on the `adapter { }` block.
  The legacy `config = {...}` shape for step input is not accepted.
- **Local mode constraints**: `wait { signal = "..." }` and `approval { ... }`
  nodes require a server-compatible orchestrator (`criteria apply --server ...`).
  Local-only execution rejects these node kinds with a clear error.
- Keep logs structured (`slog` JSON style in entrypoints).
- **Commit subjects**: every squash-merge commit subject must be
  `KB-<n>: <one-line summary>` (merge tooling appends the PR number as
  `(#N)`). Precedents: `KB-57: stop dropping delivered adapter outcomes at
  teardown; emit real failure outcomes (#487)` and
  `KB-57c: compound command permission support (segment + all-segments-match) (#488)`.
  Bare `KB N (#N)` subjects are a rejected regression — the CHANGELOG and
  history/dependency sweeps depend on descriptive subjects without opening
  every PR. Existing history is not backfilled and shipped tags are never
  rewritten.
- **Comment hygiene**: code comments carry design facts (what the code does
  and why), never ticket narration — ticket identities (`CRI-`/`KB-`) live in
  durable docs (ADRs under [docs/adrs/](docs/adrs/), [CHANGELOG.md](CHANGELOG.md))
  and in PRs, not in production comments. Do not add new ticket-ID narrations
  to comments; state the design fact itself or cite the ADR instead.
- **Timing knobs** live in `internal/tunables`: no new raw timing literals
  or ad-hoc `os.Getenv` timing reads at consumer sites — consume the typed
  `Settings` (via `tunables.FromEnv()` at configuration points) or the
  exported `Default*` constants, and register any operator-facing override
  in the registry so [docs/env-vars.md](docs/env-vars.md) stays current.
- Preserve existing adapter boundaries (`internal/adapter`,
  `internal/adapters/*`, `internal/plugin`). Do not import `sdk/` from
  `internal/` — `sdk/pb/...` is the only permitted reach into the SDK
  tree (enforced by `make lint-imports`).
- **Test workflow construction**: Do not write raw HCL strings, multi-line
  print statements, or ad-hoc string formatting directly in test files to construct
  test workflows. Always use the test workflow builder in package
  `github.com/brokenbots/criteria/workflow/workflowtest`:
  - `workflowtest.New()` or `workflowtest.NewStandard()` to initialize a typed
    `WorkflowBuilder`.
  - Fluent configuration: `.WithAdapter(...)`, `.WithEnv(...)`, `.WithStep(...)`,
    `.WithParallel(...)`, `.WithSwitch(...)`, `.WithWait(...)`, `.WithApproval(...)`,
    `.WithSubWorkflow(...)`.
  - Output helpers: `.Build()` (generates canonical HCL), `.BuildToFile(t, path)`
    (writes directly to disk), or `.Compile(t)` (parses and compiles directly into
    a `*workflow.CompiledWorkflow`).
  - Standard test scaffolding and shared mock adapters live in `workflowtest`
    and `internal/testutil`.

## Common pitfalls

- Copilot adapter execution requires installing `criteria-adapter-copilot`
  into `${CRITERIA_ADAPTERS}/` (default `$CRITERIA_HOME/adapters`, i.e.
  `~/.local/criteria/adapters`), plus the `copilot` CLI on `PATH` (or pointed
  at via `CRITERIA_COPILOT_BIN`). There is no in-binary adapter code.
- Server run/event ordering depends on server-assigned monotonic `seq`
  per `run_id`; avoid client-side ordering assumptions.
- Avoid introducing CGO-only SQLite dependencies; current storage uses
  pure-Go `modernc.org/sqlite`.
- Do not interpolate raw HCL snippets or print statements into test files.
  Use `workflowtest.New()` / `workflowtest.Builder` to ensure type-safe,
  canonical test workflow generation.
- Prefer `make test` over ad-hoc partial test runs unless task scope is
  clearly limited.
- `proto/criteria/v1/server.proto` exists in this repo because the CLI
  client embeds ServerService stubs (`status`, `stop`); the orchestrator
  side of ServerService is implemented in the orchestrator repo.
