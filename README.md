# Criteria

> **Status: work in progress — not production-ready.** Development is heavily
> AI-driven. The HCL language and adapter protocol are still changing, and large
> parts are lightly tested or unverified (see [Component status](#component-status)
> and [Language features](#language-features)). Adapters execute arbitrary code;
> treat them as trusted and isolate them in a container or sandbox.

Criteria is a workflow engine for agent-based workflows built on an extensible
adapter system. Workflows are written in HCL, compiled to a finite-state
machine, and executed from a single binary. Each step runs through a swappable
out-of-process adapter (a shell runner, an AI coding agent, an MCP bridge, or a
custom one). It is developed primarily as an AI-authorable workflow tool and as a
testbed for agentic development, security, and research workflows.

## Model

- Workflows are HCL, compiled to a finite-state machine: a directed graph that
  permits loops. The compiler requires a terminal state and enforces a per-run
  step budget and per-state visit bounds, so a run cannot loop unbounded.
- Steps execute through out-of-process adapters that speak a versioned gRPC
  protocol over a local socket.
- Adapters are distributed as OCI artifacts, cosign-signed, and pinned by digest
  in `.criteria.lock.hcl` for reproducible resolution.
- Execution is local by default; an optional, early server mode adds durability
  (see the note at the end).
- Every run emits schema-versioned ND-JSON events.

## Component status

Status legend: **Working** = implemented and exercised; **Experimental** =
implemented, lightly tested; **Untested** = implemented, essentially unverified;
**Partial** = incomplete; **Not implemented** = not functional yet.

| Component | Status | Notes |
|---|---|---|
| HCL compiler / FSM engine | Working | Most-exercised part of the codebase. |
| Local execution (`apply`) | Working | Single binary, no server. |
| Event stream (ND-JSON) | Working | Schema-versioned. |
| `compile` (JSON/DOT), `plan`, `validate` | Working | Graph output and previews. |
| `criteria spec` (language spec for LLMs) | Working | See [Authoring with AI](#authoring-workflows-with-ai). |
| `langserver` (LSP) | Experimental | Basic diagnostics/definitions. |
| Adapter protocol (v2) + Go SDK | Experimental | Protocol recently reworked; needs broad testing. |
| `copilot`, `shell` adapters | Experimental | The only adapters with real use. |
| `mcp` adapter (in-tree) | Experimental | Reference bridge for MCP servers. |
| Other adapters | Untested | Not validated beyond build. |
| TypeScript / Python SDKs + adapters | Untested | Smoke-tested at best inside a workflow. |
| Execution environments (sandbox/container/remote) | Untested | Implemented; minimal real testing. |
| Server / orchestrator mode + conformance suite | Experimental | Contract under development. |
| Pause / resume, crash recovery | Partial | Server-oriented; not battle-tested. |
| `criteria adapter dev` | Partial | Registers a binary but is not yet wired into `apply`. |

## Language features

| Construct | Status | Notes |
|---|---|---|
| `workflow`, `state`, `step`, `outcome` | Working | Core FSM. |
| `adapter` blocks + `target = adapter.<type>.<name>` | Working | Out-of-process adapters. |
| `target = subworkflow.<name>` | Working | First-class sub-workflows. |
| `switch` branching | Working | |
| `for_each` iteration | Working | |
| `parallel = [...]` regions | Working | List form only. |
| `variable`, `shared_variable`, local values, `output` | Working | |
| `wait { duration = ... }` | Working | Local. |
| `wait { signal = ... }`, `approval { ... }` | Partial | Oriented to server mode; local support is limited. |
| `environment` blocks | Untested | shell / sandbox / container / remote; see status table. |
| Secret inputs / tainting | Experimental | |
| `parallel` map/object form | Not supported | Use the list form. |
| Remote subworkflow sources (`url://`) | Not supported | |

The authoritative reference is `criteria spec` (and [docs/workflow.md](docs/workflow.md)).

## Install

One-line installer (macOS / Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/brokenbots/criteria/main/install.sh | /bin/sh
```

Pin to a specific release:

```sh
CRITERIA_VERSION=v0.5.6 curl -fsSL https://raw.githubusercontent.com/brokenbots/criteria/main/install.sh | /bin/sh
```

Install from source with Go 1.26 or later:

```bash
go install github.com/brokenbots/criteria/cmd/criteria@latest
```

Build from source:

```bash
git clone https://github.com/brokenbots/criteria.git
cd criteria && make build   # produces bin/criteria
```

Release binaries: [GitHub Releases](https://github.com/brokenbots/criteria/releases).

## Quickstart

The CLI ships without adapters; a workflow references the ones it needs and
Criteria pulls, verifies, and pins them.

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
  input { command = "echo hello from criteria" }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```

```bash
criteria adapter lock          # resolve, pull, verify, and pin → .criteria.lock.hcl
criteria apply hello.hcl       # execute; ND-JSON events to stdout (or --events-file)
criteria compile hello.hcl --format dot | dot -Tsvg > hello.svg   # inspect the graph
```

## Authoring workflows with AI

`criteria spec` prints the language specification for use as model context:

```bash
criteria spec                  # specification only
criteria spec --with-patterns  # specification + prompt-pack patterns (LLM system prompt)
```

A model given that context can author workflows directly; the compiler then
validates them before execution.

## Adapters

Adapters are out-of-process binaries distributed as cosign-signed OCI artifacts.
Reference one by `source` (version-decoupled); Criteria resolves, pulls,
verifies, and pins it by digest:

```bash
criteria adapter lock
criteria apply workflow.hcl
```

Cache management: `criteria adapter pull|list|info|where|remove|prune`.

Adapter authoring uses starter templates
([typescript](https://github.com/brokenbots/criteria-adapter-starter-typescript) /
[python](https://github.com/brokenbots/criteria-adapter-starter-python) /
[go](https://github.com/brokenbots/criteria-adapter-starter-go)); the TypeScript
and Python paths are untested (see [Component status](#component-status)). The
in-tree [`cmd/criteria-adapter-mcp`](cmd/criteria-adapter-mcp/) bridges an MCP
server in as an adapter and serves as a reference.

Reference: [docs/adapters.md](docs/adapters.md).

## License

See [LICENSE](LICENSE).

---

> **Note — server mode (early, subject to significant change).** Execution is
> local by default. An optional server can provide durability — run persistence,
> crash recovery, approval gates, and signal waits — via `criteria apply --server
> <url>`. The gRPC contract and a conformance suite live in the `sdk/` module
> (`github.com/brokenbots/criteria/sdk`). This contract is unstable and expected
> to change substantially.
