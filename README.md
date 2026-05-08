# Criteria

**Status: This project is under heavy development use with caution, run in a container for safety as adapter should be considered trusted code

Criteria is a standalone workflow execution engine. Write a workflow in HCL, run it with `criteria apply` — no external service required. Each workflow compiles to a finite-state machine; execution drives through swappable adapter plugins and streams structured ND-JSON events to stdout or a file.

*Criteria targets teams who want a Temporal- or Argo-style execution model without the infrastructure dependency for everyday development, and orchestrator authors who need a well-defined client SDK to build against.*

## Install

Requires Go 1.26 or later.

```bash
go install github.com/brokenbots/criteria/cmd/criteria@latest
```

Or build from source:

```bash
git clone https://github.com/brokenbots/criteria.git
cd criteria && make build   # produces bin/criteria
```

Pre-built binaries will be published with the first tagged release (see [Status](#status)).

## Quickstart

Create a workflow file:

```hcl
# hello.hcl
workflow "hello" {
  version       = "0.1"
  initial_state = "greet"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "greet" {
  target = adapter.shell.default
  input {
    command = "echo hello from criteria"
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```

Run it:

```bash
criteria apply hello.hcl
```

Expected output:

```
{"schema_version":1,"seq":1,...,"payload_type":"RunStarted","payload":{"workflowName":"hello","initialStep":"greet"}}
{"schema_version":1,"seq":2,...,"payload_type":"StepEntered","payload":{"step":"greet","adapter":"shell","attempt":1}}
{"schema_version":1,"seq":3,...,"payload_type":"StepLog","payload":{"step":"greet","stream":"LOG_STREAM_STDOUT","chunk":"hello from criteria\n"}}
{"schema_version":1,"seq":4,...,"payload_type":"StepOutcome","payload":{"step":"greet","outcome":"success","durationMs":"..."}}
{"schema_version":1,"seq":5,...,"payload_type":"StepTransition","payload":{"from":"greet","to":"done","viaOutcome":"success"}}
{"schema_version":1,"seq":6,...,"payload_type":"RunCompleted","payload":{"finalState":"done","success":true}}
```

## What's in the box

- **HCL → FSM compiler.** Workflows are HCL; the engine compiles them to finite-state machines before executing.
- **Local execution.** Run any workflow on your laptop with no external service.
- **Adapter plugin model.** Swap execution backends (shell, Copilot, MCP, or your own) via an out-of-process plugin protocol.
- **Structured event stream.** Every run emits schema-versioned ND-JSON events.
- **Duration-based waits, branching, and for-each loops.** Workflows can sleep, branch on conditions, and iterate over lists.
- **Orchestrator mode.** Connect to a server-compatible orchestrator for run persistence, crash recovery, human approval gates, and signal-based waits.
- **Published Go SDK.** Build a compatible orchestrator with `github.com/brokenbots/criteria/sdk` and validate it with the included conformance suite.

## Workflow language

```hcl
workflow "deploy" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "deployed"

  step "build" {
    adapter = "shell"
    input { command = "go build ./..." }
    outcome "success" { transition_to = "test" }
    outcome "failure" { transition_to = "failed" }
  }

  step "test" {
    adapter = "shell"
    input { command = "go test ./..." }
    outcome "success" { transition_to = "deployed" }
    outcome "failure" { transition_to = "failed" }
  }

  state "deployed" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
```

Full language reference: [docs/workflow.md](docs/workflow.md)

## Plugins

Adapter plugins are out-of-process binaries named `criteria-adapter-<name>`, discovered from `${CRITERIA_PLUGINS}/` or `~/.criteria/plugins/`.

```bash
# Build the bundled adapters (shell, noop, copilot, mcp)
make plugins

# Install the Copilot adapter
cp bin/criteria-adapter-copilot ~/.criteria/plugins/
```

Write your own plugin by following [docs/plugins.md](docs/plugins.md). Bundled adapters in `cmd/criteria-adapter-*` are the best starting reference — the plugin host contract (`internal/plugin`) is not importable by external modules.

Full plugin reference: [docs/plugins.md](docs/plugins.md)

## Talking to a server-compatible orchestrator

The `sdk/` sub-module publishes a Go SDK (`github.com/brokenbots/criteria/sdk`) defining the `CriteriaService` gRPC contract. Any server implementing that contract can receive runs from `criteria apply --server <url>`, stream events, handle approval gates, and resume crashed runs.

The reference implementation is [github.com/brokenbots/orchestrator](https://github.com/brokenbots/orchestrator). Validate your own implementation with the included conformance suite:

```go
import "github.com/brokenbots/criteria/sdk/conformance"

func TestMyCriteria(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

See [`sdk/conformance/`](sdk/conformance/) for the full interface and in-memory reference Subject.

## Migrating from v0.2.0 to v0.3.0

Phase 3 (v0.3.0) is a **clean break** from v0.2.0. The HCL language and adapter model were reworked to improve usability and architecture. No v0.2.0 workflows parse without updates.

**Key changes:**
- `agent` block → `adapter "<type>" "<name>"` block.
- `step.adapter = "<bare type>"` → `step.target = adapter.<type>.<name>`.
- `transition_to` → `next`.
- `branch` block → `switch` block.
- Top-level workflow attributes moved into `workflow "<name>" { ... }` block.
- Inline `step.workflow { ... }` replaced by first-class `subworkflow` blocks.
- `lifecycle = "open"|"close"` removed (auto-managed).

See the [v0.2.0 → v0.3.0 migration guide](CHANGELOG.md#v0.2.0--v0.3.0-migration-guide) for comprehensive before/after examples.

## Status

**v0.3.0** (tagged 2026-05-06) closes Phase 3 — the HCL/runtime rework. Key accomplishments:

- **Phase 3 — HCL and runtime rework.** Clean break from v0.2.0: `adapter` block model replaces `agent`; `switch` replaces `branch`; `next` replaces `transition_to`; workflow attributes wrap in a `workflow` block; subworkflows are first-class; adapter lifecycle is automatic; parallel execution, shared variables, top-level outputs, local variables, environment blocks, and universal step `target` attribute are all added. Lint baseline burn-down complete (≤ 50); Maintainability and Tech Debt both lifted to B. Release process integrity ([tag-claim-check](docs/contributing/release-process.md) CI guard) shipping.

Prior phases:
- **Phase 2** (v0.2.0, 2026-05-02) — Maintainability + unattended MVP + Copilot tool-call finalization. Local-mode approval, signal waits, `max_visits` loop bounding, `~/.criteria/` hardened, Copilot `submit_outcome` RPC replacing prose parsing, runtime Docker image.
- **Phase 1** (v0.2.0, 2026-04-29) — Stabilization and critical user fixes. Deterministic CI, golangci-lint, coverage/benchmark baselines, `file()` functions, `for_each`, Copilot `reasoning_effort`, step-level workflow nesting.
- **Phase 0** (v0.1.0, 2026-04-27) — Post-separation cleanup. Repo hygiene, public plugin SDK, shell adapter sandboxing, brand rename completion.

Binary releases are published on GitHub Releases. For installation, see [Install](#install).

## License

See [LICENSE](LICENSE).
