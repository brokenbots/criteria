# Criteria

> **Status — active development.** The workflow HCL language and the adapter
> design are **stabilizing**: breaking changes are now rare but still possible
> before a 1.0 release. Adapters execute arbitrary code and should be treated as
> **trusted** — run them inside a container or a sandboxed environment.

Criteria is a workflow engine that runs multi-step processes — shell commands,
AI coding agents, MCP tools, or anything you wrap in an adapter — as
deterministic state machines, from a single binary with no service to deploy.

You describe a workflow in HCL; Criteria compiles it to a finite-state machine
and executes it with `criteria apply`. Each step drives a swappable
out-of-process **adapter**, and every run emits a schema-versioned ND-JSON event
stream you can pipe, store, or watch live.

## The problem it solves

Real workflows — CI-style build/test/deploy pipelines, or agentic loops where an
AI agent does work, gets reviewed, and retries — need durable state, retries,
branching, approvals, and observability. The usual tools (Temporal, Argo, and
friends) provide that, but they require standing up infrastructure that is
overkill for local development and fast iteration.

Criteria gives you the same execution model — FSM semantics, retries, waits,
branching, parallelism, sub-workflows — as a **single local binary**. When you
genuinely need durability, point the *same* workflow at a server-compatible
orchestrator over a published gRPC SDK for persistence, crash recovery, human
approval gates, and signal-based waits — or build your own server and verify it
against the bundled conformance suite.

Reproducibility and safety are built in: adapters are distributed as **signed
OCI artifacts**, pinned by digest in a lockfile, and can run directly, in a
sandbox, in a container, or on a remote host.

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

Binaries are published on [GitHub Releases](https://github.com/brokenbots/criteria/releases).

## Quickstart

The CLI ships without adapters — you reference the ones a workflow needs and
Criteria pulls, verifies, and pins them. Write a workflow:

```hcl
# hello.hcl
workflow {
  name          = "hello"
  version       = "1"
  initial_state = "greet"
  target_state  = "done"
}

adapter "shell" "default" {
  source = "ghcr.io/brokenbots/criteria-adapter-shell"
  config {}
}

step "greet" {
  target = adapter.shell.default
  input {
    command = "echo hello from criteria"
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```

Pin the adapters it references, then run:

```bash
criteria adapter lock   # resolve → pull → verify signature → pin in .criteria.lock.hcl
criteria apply hello.hcl
```

Each run streams structured ND-JSON events to stdout (or `--events-file`):

```
{"schema_version":1,"seq":1,...,"payload_type":"RunStarted","payload":{"workflowName":"hello","initialStep":"greet"}}
{"schema_version":1,"seq":2,...,"payload_type":"StepEntered","payload":{"step":"greet","adapter":"shell","attempt":1}}
{"schema_version":1,"seq":3,...,"payload_type":"StepLog","payload":{"step":"greet","stream":"LOG_STREAM_STDOUT","chunk":"hello from criteria\n"}}
{"schema_version":1,"seq":4,...,"payload_type":"StepOutcome","payload":{"step":"greet","outcome":"success","durationMs":"..."}}
{"schema_version":1,"seq":5,...,"payload_type":"StepTransition","payload":{"from":"greet","to":"done","viaOutcome":"success"}}
{"schema_version":1,"seq":6,...,"payload_type":"RunCompleted","payload":{"finalState":"done","success":true}}
```

## What's in the box

- **HCL → FSM compiler.** Workflows are HCL; the engine compiles them to
  finite-state machines before executing.
- **Local execution.** Run any workflow from a single binary with no external
  service.
- **Out-of-process adapters.** Drive shell commands, AI coding agents, an MCP
  bridge, or your own backend through a versioned plugin protocol. Adapters are
  distributed as signed OCI artifacts and pinned by digest in
  `.criteria.lock.hcl`.
- **Execution environments.** Run an adapter directly, in an OS sandbox, in a
  container, or on a remote host that phones home over mTLS.
- **Rich control flow.** Retries, duration-based waits, `switch` branching,
  parallel regions, `for_each` iteration, first-class sub-workflows, shared and
  local variables, and top-level outputs.
- **Structured event stream.** Every run emits schema-versioned ND-JSON events.
- **Orchestrator mode.** Connect to a server-compatible orchestrator for run
  persistence, crash recovery, human approval gates, and signal-based waits.
- **Published Go SDK.** Build a compatible orchestrator with
  `github.com/brokenbots/criteria/sdk` and validate it with the included
  conformance suite.

## Workflow language

```hcl
workflow {
  name          = "deploy"
  version       = "1"
  initial_state = "build"
  target_state  = "deployed"
}

adapter "shell" "default" {
  source = "ghcr.io/brokenbots/criteria-adapter-shell"
  config {}
}

step "build" {
  target = adapter.shell.default
  input { command = "go build ./..." }
  outcome "success" { next = state.test }
  outcome "failure" { next = state.failed }
}

step "test" {
  target = adapter.shell.default
  input { command = "go test ./..." }
  outcome "success" { next = state.deployed }
  outcome "failure" { next = state.failed }
}

state "deployed" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```

Full language reference: [docs/workflow.md](docs/workflow.md)

## Adapters

Adapters are out-of-process binaries distributed as signed OCI artifacts.
Reference one by `source` (version-decoupled) in your workflow and let Criteria
resolve, pull, verify, and pin it:

```bash
# Pin every adapter a workflow references (writes .criteria.lock.hcl) and run.
criteria adapter lock
criteria apply workflow.hcl
```

Adapters are pulled into a local cache, signature-verified, and pinned by digest
so the workflow reproduces identically anywhere. Manage the cache directly with
`criteria adapter pull|list|info|where|remove|prune`, and register a local
binary during development with `criteria adapter dev <binary>`.

Write your own from a starter template
([typescript](https://github.com/brokenbots/criteria-adapter-starter-typescript) /
[python](https://github.com/brokenbots/criteria-adapter-starter-python) /
[go](https://github.com/brokenbots/criteria-adapter-starter-go)) — each is a
buildable hello-world with a publish workflow. The in-tree
[`cmd/criteria-adapter-mcp`](cmd/criteria-adapter-mcp/) is a minimal reference
(it bridges any MCP server in as an adapter).

Full reference: [docs/adapters.md](docs/adapters.md)

## Talking to a server-compatible orchestrator

The `sdk/` sub-module publishes a Go SDK
(`github.com/brokenbots/criteria/sdk`) defining the orchestrator gRPC contract.
Any server implementing that contract can receive runs from
`criteria apply --server <url>`, stream events, handle approval gates, and resume
crashed runs.

The reference implementation is
[github.com/brokenbots/orchestrator](https://github.com/brokenbots/orchestrator).
Validate your own implementation with the included conformance suite:

```go
import "github.com/brokenbots/criteria/sdk/conformance"

func TestMyCriteria(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

See [`sdk/conformance/`](sdk/conformance/) for the full interface and the
in-memory reference Subject.

## License

See [LICENSE](LICENSE).
