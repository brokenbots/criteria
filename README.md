# Overseer

Overseer is a standalone workflow execution engine. Write a workflow in HCL, run it with `overseer apply` — no external service required. Each workflow compiles to a finite-state machine; execution drives through swappable adapter plugins and streams structured ND-JSON events to stdout or a file.

*Overseer targets teams who want a Temporal- or Argo-style execution model without the infrastructure dependency for everyday development, and orchestrator authors who need a well-defined client SDK to build against.*

## Install

Requires Go 1.26 or later.

```bash
go install github.com/brokenbots/overseer/cmd/overseer@latest
```

Or build from source:

```bash
git clone https://github.com/brokenbots/overseer.git
cd overseer && make build   # produces bin/overseer
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

  step "greet" {
    adapter = "shell"
    input {
      command = "echo hello from overseer"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
```

Run it:

```bash
overseer apply hello.hcl
```

Expected output:

```
{"schema_version":1,"seq":1,...,"payload_type":"RunStarted","payload":{"workflowName":"hello","initialStep":"greet"}}
{"schema_version":1,"seq":2,...,"payload_type":"StepEntered","payload":{"step":"greet","adapter":"shell","attempt":1}}
{"schema_version":1,"seq":3,...,"payload_type":"StepLog","payload":{"step":"greet","stream":"LOG_STREAM_STDOUT","chunk":"hello from overseer\n"}}
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
- **Orchestrator mode.** Connect to a Castle-compatible orchestrator for run persistence, crash recovery, human approval gates, and signal-based waits.
- **Published Go SDK.** Build a compatible orchestrator with `github.com/brokenbots/overseer/sdk` and validate it with the included conformance suite.

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

Adapter plugins are out-of-process binaries named `overseer-adapter-<name>`, discovered from `${OVERSEER_PLUGINS}/` or `~/.overseer/plugins/`.

```bash
# Build the bundled adapters (shell, noop, copilot, mcp)
make plugins

# Install the Copilot adapter
cp bin/overseer-adapter-copilot ~/.overseer/plugins/
```

Write your own plugin by following [docs/plugins.md](docs/plugins.md). Bundled adapters in `cmd/overseer-adapter-*` are the best starting reference — the plugin host contract (`internal/plugin`) is not importable by external modules.

Full plugin reference: [docs/plugins.md](docs/plugins.md)

## Talking to a Castle-compatible orchestrator

The `sdk/` sub-module publishes a Go SDK (`github.com/brokenbots/overseer/sdk`) defining the `OverseerService` gRPC contract. Any server implementing that contract can receive runs from `overseer apply --castle <url>`, stream events, handle approval gates, and resume crashed runs.

The reference implementation is [github.com/brokenbots/overlord](https://github.com/brokenbots/overlord). Validate your own implementation with the included conformance suite:

```go
import "github.com/brokenbots/overseer/sdk/conformance"

func TestMyOverseer(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

See [`sdk/conformance/`](sdk/conformance/) for the full interface and in-memory reference Subject.

## Status

Overseer is pre-release (`v0.x`), currently used internally. The HCL language and SDK contract are stabilizing. A public release is planned once the Phase 0 cleanup workstreams land; binary releases and Docker images will be published with the first tag.

## License

See [LICENSE](LICENSE).
