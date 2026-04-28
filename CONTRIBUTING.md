# Contributing to Criteria

## Setup

**Prerequisites:**

- Go 1.26 or later
- [buf](https://buf.build/docs/installation) (required only for proto regeneration)
- git

```bash
git clone https://github.com/brokenbots/criteria.git
cd criteria
make bootstrap         # sync all three Go workspace modules
make build             # produces bin/criteria and the bundled adapter binaries
```

The repo is a Go workspace containing three modules: the root module (engine + CLI), `sdk/` (published Go SDK), and `workflow/` (HCL compiler). `make bootstrap` handles all three.

## Project layout

The CLI entrypoint is `cmd/criteria`; the engine, plugin loader, and adapters live under `internal/`; the HCL parser and FSM compiler are in `workflow/`; the published Go SDK is in `sdk/`; and out-of-process adapter plugins are in `cmd/criteria-adapter-*`. See [AGENTS.md](AGENTS.md) for the full component map, architecture notes, and agent-specific constraints.

## Development workflow

1. Fork the repo and create a feature branch.
2. Make your changes. Add or update tests as needed.
3. Run `make test` to verify all tests pass.
4. Run `make validate` to verify example workflows parse and compile cleanly.
5. Run `make lint-imports` to confirm module boundary rules are satisfied.
6. If you changed proto files, run `make proto` and commit the generated bindings alongside the `.proto` changes.
7. Open a pull request against `main`.

## Test lanes

| Command | What it covers | When to run |
|---|---|---|
| `make test` | All Go unit and integration tests across every module | Before every PR |
| `make test-conformance` | SDK conformance suite against the in-memory reference Subject | When touching `sdk/` or the proto contract |
| `make validate` | Example HCL workflows parse and compile without errors | When touching `workflow/` or any `examples/` file |
| `make lint-imports` | Module boundary rules (`internal/` may not import `sdk/` except `sdk/pb/...`) | When adding new cross-module imports |

## Proto changes

Proto source files live in `proto/criteria/v1/`. After editing them:

```bash
make proto       # regenerate sdk/pb/criteria/v1/ Go bindings
make proto-lint  # lint proto files with buf
```

Commit the `.proto` changes and the regenerated `sdk/pb/` files together in the same commit. CI checks for drift and will fail if they are out of sync.

## Workstream-driven workflow

Agent-executed work in this repo is organised by workstream files in `workstreams/`. Each PR corresponds to one workstream file:

- An **executor agent** reads the workstream file, implements the tasks, marks checklist items complete, and adds reviewer notes.
- A **reviewer agent** audits the implementation against the workstream checklist, quality bar, and exit criteria. The reviewer does not edit code; it requires the executor to remediate all findings before approval.
- The **W08 cleanup gate** handles cross-cutting documentation updates (README, PLAN.md, AGENTS.md) after all workstreams in a phase complete.

Human contributors follow the same convention: pick up a workstream file, implement its tasks, and open a PR scoped to that workstream's allowed files. See [AGENTS.md](AGENTS.md) for the full agent-execution rules.

## Published SDK contract

`sdk/` is a published Go sub-module at `github.com/brokenbots/criteria/sdk`. The following are **breaking SDK changes** that require a version bump:

- Any change to the `conformance.Subject` interface.
- Any change to `ServiceHandler` or `ServiceClient` method signatures.
- Any change to event proto field numbers in `proto/criteria/v1/events.proto` (field numbers are permanent once published).
- Removal or rename of exported SDK functions or types.

Additive changes (new fields, new events, new conformance test cases) are non-breaking at minor or patch level.

## Adapter plugins

Plugin binaries are named `criteria-adapter-<name>` and must be placed in `${CRITERIA_PLUGINS}/` or `~/.criteria/plugins/`. Build the bundled adapters with `make plugins`. See [docs/plugins.md](docs/plugins.md) for the plugin wire protocol and development guide.

## Code style

- Structured logging only: use `slog` (JSON output in production entrypoints).
- No CGO: use pure-Go alternatives (e.g., `modernc.org/sqlite` if storage is needed).
- Adapter plugin source lives in `cmd/criteria-adapter-*/`; the internal plugin loader lives in `internal/plugin/` and `internal/adapter*/`.
- `make lint-imports` enforces the import boundary: `sdk/pb/...` is the only permitted reach into the SDK tree from `internal/`.

