# Criteria

> **⚠️ Early work in progress — not production-ready.** Criteria is under active,
> heavily AI-driven development. The workflow language and adapter model are
> stabilizing, but large parts are only lightly tested — see
> [Project status](#project-status) for an honest breakdown. Adapters execute
> arbitrary code and should be treated as **trusted**: run them inside a
> container or a sandboxed environment.

Criteria is a workflow engine for building **agent-based workflows** on top of an
extensible adapter system. You describe a workflow in a small HCL language;
Criteria compiles it to a state machine and runs it from a single binary,
driving each step through a swappable out-of-process **adapter** — a shell
runner, an AI coding agent, an MCP tool bridge, or your own.

It is two things at once: an **exploration** of how to build fully agentic
development, security, and research workflows, and an **effort** to grow that
into a production-quality workflow tool that is easy to use and easy for AI to
author.

## What it's trying to be

These are the design goals. Some are working today, some are still ahead:

- **A small, deliberately limited language that compiles to real state
  machines.** Not just a DAG — a fully directed graph, loops included, with
  built-in safety mechanics (visit bounds, required terminal states) so a
  workflow can't quietly run away.
- **Safe and reproducible by construction.** The compiler prioritizes
  consistency and stability: it rejects ambiguous or unsafe graphs *before*
  anything runs, and adapters are pinned by signed digest so a workflow
  reproduces identically anywhere.
- **Technology-agnostic, reusable end-to-end workflows.** Every step is just an
  adapter, so the same workflow shape composes shell commands, AI agents, and
  bespoke tools without coupling to any one of them.
- **AI-friendly authoring.** Workflows should be buildable *by* AI. `criteria
  spec` prints the full language specification (optionally a ready-to-use LLM
  system prompt) so an agent can author correct workflows directly, and a
  `langserver` (LSP) gives humans the same assistance.
- **Verifiable and debuggable graphs.** `criteria compile` emits the compiled
  graph as JSON or DOT, `criteria plan` previews execution, and every run streams
  schema-versioned ND-JSON events.
- **Durable, pausable runs (aspirational).** Long-term, a compiled workflow
  should pause and resume cleanly. Orchestrator mode has the beginnings of this —
  pause/resume, crash recovery, approval gates — but it is not yet battle-tested.

## Project status

This is research-grade software. An honest state of the world:

- **Engine + compiler** — the core HCL → state-machine path is the most
  exercised part and is reasonably solid for the features it covers.
- **Adapters** — only **copilot** and **shell** have seen real use. The adapter
  model was **recently reworked** and needs substantial testing; the other
  adapters are largely unproven.
- **TypeScript & Python SDKs / adapters** — smoke-tested at best inside a
  workflow, not yet trustworthy for real work.
- **Execution environments** (sandbox / container / remote) — implemented but
  only lightly tested.

Expect rough edges, gaps, and breaking changes. Issues, findings, and test cases
are very welcome.

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

Inspect the compiled graph instead of running it:

```bash
criteria compile hello.hcl --format dot | dot -Tsvg > hello.svg   # visualize
criteria plan hello.hcl                                           # execution preview
```

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

The language supports retries, duration waits, `switch` branching, parallel
regions, `for_each` iteration, first-class sub-workflows, shared and local
variables, and top-level outputs. Full reference:
[docs/workflow.md](docs/workflow.md).

## Authoring workflows with AI

Criteria is meant to be driven by AI as much as by humans. The tooling hands the
model everything it needs to write correct workflows:

```bash
criteria spec                  # the full language specification, as Markdown
criteria spec --with-patterns  # spec + prompt-pack patterns = an LLM system prompt
```

Pipe that into an agent's context and it can author workflows directly; the
compiler then verifies the result before anything runs.

## Adapters

Adapters are out-of-process binaries distributed as signed OCI artifacts.
Reference one by `source` (version-decoupled) and let Criteria resolve, pull,
verify, and pin it:

```bash
criteria adapter lock      # pin every adapter a workflow references
criteria apply workflow.hcl
```

Adapters are pulled into a local cache, signature-verified, and pinned by digest
so the workflow reproduces identically anywhere. Manage the cache with
`criteria adapter pull|list|info|where|remove|prune`, and register a local
binary during development with `criteria adapter dev <binary>`.

Write your own from a starter template
([typescript](https://github.com/brokenbots/criteria-adapter-starter-typescript) /
[python](https://github.com/brokenbots/criteria-adapter-starter-python) /
[go](https://github.com/brokenbots/criteria-adapter-starter-go)) — each is a
buildable hello-world with a publish workflow. The in-tree
[`cmd/criteria-adapter-mcp`](cmd/criteria-adapter-mcp/) is a minimal reference
that bridges any MCP server in as an adapter.

Full reference: [docs/adapters.md](docs/adapters.md). (Note: the TypeScript and
Python paths are early and lightly tested — see [Project status](#project-status).)

## Orchestrator mode (optional, early)

By default everything runs locally. For durability — run persistence, crash
recovery, human approval gates, and signal-based waits — a workflow can target a
server-compatible orchestrator:

```bash
criteria apply workflow.hcl --server <url>
```

The `sdk/` sub-module publishes a Go SDK
(`github.com/brokenbots/criteria/sdk`) defining the orchestrator gRPC contract,
with a conformance suite so an implementation can verify itself:

```go
import "github.com/brokenbots/criteria/sdk/conformance"

func TestMyServer(t *testing.T) {
    conformance.Run(t, &mySubject{})
}
```

This path is early; treat it as a contract under development rather than a
finished feature.

## License

See [LICENSE](LICENSE).
