# Criteria

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

  step "greet" {
    adapter = "shell"
    input {
      command = "echo hello from criteria"
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

## Status

`v0.2.0` is the current release (tagged 2026-05-02). It bundles two phases of work that were merged but not separately tagged:

- **Phase 1 — Stabilization.** Deterministic CI (`-count=2`, `goleak`), golangci-lint with a documented [burn-down contract](docs/contributing/lint-baseline.md), shell adapter first-pass hardening (see [threat model](docs/security/shell-adapter-threat-model.md)), coverage/benchmark/GoDoc baselines, and four user-blocking capability additions (`file()`/`fileexists()`/`trimfrontmatter()` expression functions, step-level `for_each`/`count` with a `type = "workflow"` nested step, Copilot `reasoning_effort` propagation, and targeted diagnostics for misplaced agent-config fields).
- **Phase 2 — Maintainability + unattended MVP + Copilot tool-call finalization.** Local-mode approval and signal-wait (`CRITERIA_LOCAL_APPROVAL`), per-step `max_visits` to bound runaway loops, `~/.criteria/` directory mode hardened to `0o700`, the `CRITERIA_SHELL_LEGACY=1` shell-sandbox opt-out removed, the `copilot.go` adapter split into focused files with a permission-kind alias, the Copilot adapter's `result:` prose parsing replaced by a structured `submit_outcome` tool call (with `pb.ExecuteRequest.AllowedOutcomes` on the wire), an adapter-lifecycle sink hook for log clarity, a VS Code dev container and operator runtime image (`Dockerfile.runtime`), and release-candidate artifact uploads on RC PRs.

Phase 0 cleanup is complete: the project is renamed to Criteria, the HCL language and SDK contract are stabilizing, and the repo has full test coverage and public-repo hygiene. Binary releases will be published via GitHub Releases as they are cut; for now use `go install`.

## License

See [LICENSE](LICENSE).
