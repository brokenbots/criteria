# Plugins and Agent Workflows

This document is the reference for running agent-backed workflows with Criteria. For the full workflow language reference (variables, step outputs, branching, iteration, wait nodes, approval gates), see [workflow.md](workflow.md).

## What Plugins Are

A Criteria plugin is an out-of-process binary named `criteria-adapter-<name>`. Criteria discovers plugins in this order:

1. `${CRITERIA_PLUGINS}/criteria-adapter-<name>`
2. `~/.criteria/plugins/criteria-adapter-<name>`

Criteria does not look on `PATH`. The host starts the plugin with HashiCorp `go-plugin`; the plugin then speaks the shared gRPC adapter protocol over a local transport. The binary stays outside the Criteria process boundary, so adapter-specific runtime failures are isolated from the engine.

The first production plugin in this repo is `copilot`, shipped as `bin/criteria-adapter-copilot`.

## Installing a Plugin

Build the repo first:

```bash
make build
```

Install the plugin by copying the built binary into a plugin directory:

```bash
mkdir -p ~/.criteria/plugins
cp bin/criteria-adapter-copilot ~/.criteria/plugins/
chmod +x ~/.criteria/plugins/criteria-adapter-copilot
```

To use a temporary plugin directory instead, point Criteria at it explicitly:

```bash
tmpdir="$(mktemp -d)"
cp bin/criteria-adapter-copilot "$tmpdir/"
chmod +x "$tmpdir/criteria-adapter-copilot"
CRITERIA_PLUGINS="$tmpdir" ./bin/criteria status --server http://localhost:8080
```

For local Copilot-backed runs you also need the `copilot` CLI available. The repo helper script documents the expected setup:

```bash
gh extension install github/gh-copilot
```

If the CLI is installed somewhere non-standard, set `CRITERIA_COPILOT_BIN=/path/to/copilot`.

## HCL Surface — Shell Adapter

The built-in `shell` adapter runs `input.command` via `sh -c` (Unix) or `cmd /C` (Windows).

### New input attributes (W05 hardening)

All attributes below are optional. `CRITERIA_SHELL_LEGACY=1` disables the security defaults
(env allowlist, PATH sanitization, bounded capture, working-directory confinement) for a
time-boxed migration window; it will be removed in `v0.3.0`.

| Attribute | Type | Default | Description |
|---|---|---|---|
| `command` | `string` | (required) | Shell command to run. |
| `env` | `string` | `""` (inherit allowlist only) | JSON-encoded `map[string]string` of additional env vars to pass to the child. Values starting with `$` inherit from the parent (e.g. `"$GOFLAGS"` → `os.Getenv("GOFLAGS")`). `PATH` is reserved — use `command_path` instead. Use `jsonencode({...})` in HCL. |
| `command_path` | `string` | `""` (sanitized parent PATH) | OS path-list-separator delimited PATH for the child process (`:` on Unix, `;` on Windows). When set, replaces the inherited PATH entirely. When absent, the parent PATH is passed through with empty and non-absolute segments (including `.`) removed. |
| `timeout` | `string` | `"5m"` | Hard step timeout (e.g. `"10m"`, `"1h"`). Range: `1s`–`1h`. On timeout the spawned shell receives SIGTERM; after 5 s it receives SIGKILL. |
| `output_limit_bytes` | `string` | `"4194304"` (4 MiB) | Per-stream stdout/stderr capture limit. Range: `1024`–`67108864`. Overflow is non-fatal; a `_truncated_<stream>: "true"` sentinel is set in step outputs. |
| `working_directory` | `string` | `""` (inherit operator CWD) | CWD for the spawned process. Must resolve under `$HOME` or `CRITERIA_SHELL_ALLOWED_PATHS` (OS path-list-separator delimited env var). |

### Example with env and timeout

<!-- validator: skip: illustrative excerpt only -->
```hcl
step "build" {
  adapter = "shell"
  input {
    command = "make build"
    env     = jsonencode({GOFLAGS: "$GOFLAGS", CGO_ENABLED: "0"})
    timeout = "10m"
  }
  outcome "success" { transition_to = "test" }
  outcome "failure" { transition_to = "failed" }
}
```

### Security defaults

The shell adapter applies five hardening defaults from W05. See
[`docs/security/shell-adapter-threat-model.md`](security/shell-adapter-threat-model.md)
for the full design.

1. **Environment allowlist** — only `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`,
   `LC_*`, `TZ`, and `TERM` (when stdin is a TTY) are inherited by default.
   All other parent vars are dropped unless declared in `input.env`.
2. **PATH sanitization** — empty and non-absolute segments (including `.`) are
   removed from the inherited PATH before the child sees it. Use `command_path`
   to declare an explicit PATH.
3. **Hard timeout** — default 5 minutes. The spawned shell process receives
   SIGTERM then (after 5 s) SIGKILL. Note that grandchildren spawned by `sh -c`
   are not joined to a separate process group and may not be signalled directly;
   pipe read-ends are closed on cancellation so capture goroutines unblock
   promptly. A `timeout` adapter event is emitted in the run stream.
4. **Bounded output capture** — default 4 MiB per stream. Overflow is truncated
   (not fatal); an `output_truncated` adapter event records `dropped_bytes`.
5. **Working-directory confinement** — `working_directory` must be under `$HOME`
   or explicitly allowed via `CRITERIA_SHELL_ALLOWED_PATHS`.

## HCL Surface — Agent-backed Workflows

Agent-backed workflows use three concepts:

1. Declare the agent once with `agent "name" { adapter = "copilot" }`.
2. Open and close the agent session explicitly with `lifecycle = "open"` and `lifecycle = "close"` steps.
3. Use the agent in normal execute-shape steps with `agent = "name"` plus plugin-specific `config` and `allow_tools`.

The canonical example is `examples/agent_hello.hcl`:

<!-- validator: skip: illustrative excerpt only; full workflow in examples/agent_hello.hcl -->
```hcl
workflow "agent_hello" {
  version       = "1"
  initial_state = "open_assistant"
  target_state  = "done"

  agent "assistant" {
    adapter = "copilot"
  }

  step "open_assistant" {
    agent     = "assistant"
    lifecycle = "open"

    outcome "success" { transition_to = "ask" }
    outcome "failure" { transition_to = "failed" }
  }

  step "ask" {
    agent       = "assistant"
    allow_tools = ["shell:git status"]
    input {
      max_turns = 4
      prompt    = "Run `git status` in the current directory. Summarize the result in one short paragraph. End your final line with exactly one of: RESULT: success | RESULT: needs_review | RESULT: failure. Use RESULT: success only if you successfully ran `git status`."
    }

    outcome "success"      { transition_to = "close_done" }
    outcome "needs_review" { transition_to = "close_needs_review" }
    outcome "failure"      { transition_to = "close_failed" }
  }
}
```

The important parts are:

- `agent "assistant"` binds a stable session name to the `copilot` plugin.
- `open_assistant` creates the session. The current Copilot plugin accepts plugin-specific config such as `model` or `working_directory`, but the hello example does not need any open-time options.
- `ask` is the only execute step. For the Copilot plugin, `input.prompt` is required (Phase 1.5: step-level input moved from `config` to `input` block). `max_turns` is optional and forces a `needs_review` outcome if the plugin hits that limit.
- The prompt uses the `RESULT: <outcome>` convention. The plugin parses the final assistant message and maps that line onto the step outcome.
- Separate close steps let the workflow clean up the session and still terminate in the right state for `success`, `needs_review`, or `failure`.

## Copilot Adapter Reference

### Agent-level configuration (`config {}` block)

These fields are declared on the `agent { config { ... } }` block and apply for the lifetime of the session:

| Field | Type | Default | Description |
|---|---|---|---|
| `model` | `string` | Copilot default | Model identifier (e.g. `"claude-sonnet-4.6"`). |
| `reasoning_effort` | `string` | Copilot default | Reasoning budget for the session. One of `low`, `medium`, `high`, `xhigh`. |
| `system_prompt` | `string` | `""` | System prompt injected at session open. |
| `max_turns` | `number` | Copilot default | Maximum conversation turns per step before a `needs_review` outcome is forced. |
| `working_directory` | `string` | CWD of the criteria process | Working directory for tool invocations inside the agent session. |

Example:

<!-- validator: skip: illustrative excerpt only -->
```hcl
agent "planner" {
  adapter = "copilot"
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "medium"
    system_prompt    = "You are a senior software engineer. Think carefully before writing code."
    max_turns        = 8
  }
}
```

### Step-level input overrides (`input {}` block)

Some fields can be overridden per step in the `input {}` block. The override applies only for that step; subsequent steps revert to the agent-level default.

| Field | Type | Description |
|---|---|---|
| `prompt` | `string` | **(Required)** The user message sent to the agent for this step. |
| `max_turns` | `number` | Per-step turn limit override. |
| `reasoning_effort` | `string` | Per-step reasoning effort override. One of `low`, `medium`, `high`, `xhigh`. |

Example with per-step `reasoning_effort` override:

<!-- validator: skip: illustrative excerpt only -->
```hcl
agent "planner" {
  adapter = "copilot"
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "medium"  # default for all steps
  }
}

# Planning step uses higher reasoning effort.
step "plan" {
  agent = "planner"
  input {
    prompt           = "Draft a step-by-step implementation plan."
    reasoning_effort = "high"   # overrides "medium" for this step only
  }
  outcome "success" { transition_to = "execute" }
  outcome "failure" { transition_to = "failed" }
}

# Execution steps inherit the agent default ("medium").
step "execute" {
  agent = "planner"
  input {
    prompt = "Implement the plan from the previous step."
  }
  outcome "success" { transition_to = "done" }
  outcome "failure" { transition_to = "failed" }
}
```

### Common mistake: agent config fields in step input

Fields like `system_prompt`, `model`, and `working_directory` belong in the `agent { config { ... } }` block, not in a step's `input {}` block. Placing them in `input {}` is a compile error. For the Copilot adapter the diagnostic names the correct location:

```
step "plan" input: field "system_prompt" is not valid in step input for adapter "copilot"; it belongs in the agent config block:
  agent "<name>" {
    adapter = "copilot"
    config {
      system_prompt = ...
    }
  }
```

The only step-overrideable Copilot fields are `prompt`, `max_turns`, and `reasoning_effort`.

## Permission Gating

Permission gating is deny-by-default.

- If a step does not declare `allow_tools`, every tool request is denied.
- `allow_tools` is only valid on execute-shape agent steps. It is a compile error on adapter-backed steps or lifecycle steps.
- Patterns use Go `filepath.Match` semantics. That makes exact matches and prefix globs useful:
  - `read_file`
  - `shell:git status`
  - `shell:go test*`
  - `shell:*`

The host evaluates plugin permission requests against those patterns. When a request matches, the run emits `permission.granted`; otherwise it emits `permission.denied` with reason `no matching allow_tools entry`. The Copilot plugin then surfaces the denied turn as `needs_review` instead of silently continuing.

The hello example uses the narrowest possible allowlist:

<!-- validator: skip: bare attribute snippet, not a standalone HCL workflow -->
```hcl
allow_tools = ["shell:git status"]
```

That allows exactly `git status` and nothing else.

## Running the Demo

The shortest manual path for `examples/agent_hello.hcl` is:

```bash
make build
mkdir -p ~/.criteria/plugins
cp bin/criteria-adapter-copilot ~/.criteria/plugins/
chmod +x ~/.criteria/plugins/criteria-adapter-copilot
# Start a Criteria-compatible orchestrator server (e.g., from github.com/brokenbots/orchestrator)
# listening on 127.0.0.1:8080
```

In a second terminal, run:

```bash
./bin/criteria apply examples/agent_hello.hcl --server http://127.0.0.1:8080 --server-codec proto
```

Expected result on the success path:

1. Criteria logs a `starting run` line with a `run_id`.
2. The Copilot plugin opens a session, requests permission for `shell:git status`, and receives a grant because the step allowlist matches.
3. The assistant reports the repository status and ends with `RESULT: success`.
4. Criteria closes the session and the server records the run as `succeeded`.

For a one-command smoke check, use:

```bash
COPILOT_E2E=1 ./scripts/smoke-agent-hello.sh
```

That script builds the repo, installs the plugin into a temp directory, starts a local server, runs `agent_hello.hcl`, and asserts that the server run status becomes `succeeded`.

## The Two-Agent Loop Pattern

`examples/two_agent_loop.hcl` demonstrates the executor/reviewer loop discussed in the Phase 1.4 plan.

Key traits:

- Two named agents both bind to the `copilot` adapter.
- Both sessions are opened once per outer loop and explicitly closed on both success and failure paths.
- The executor gets a wider allowlist (`read_file`, `write_file`, `shell:git diff`, `shell:go build*`, `shell:go test*`).
- The reviewer gets a narrow allowlist (`read_file`, `shell:git diff`).
- The review step drives the loop with `approved`, `changes_requested`, or the conservative `needs_review` fallback used by the Copilot plugin when a turn needs more work or human attention.
- `policy { max_total_steps = 50 }` prevents an infinite reviewer loop.

The control flow is:

1. Open executor.
2. Open reviewer.
3. Execute implementation work.
4. Review.
5. If review returns `changes_requested` or `needs_review`, go back to execute.
6. If review returns `approved`, close reviewer, close executor, and finish.

This is the right pattern when you want long-lived agent context, distinct tool budgets per role, and an explicit safety brake on the conversation.

## Adapter Contract and Step Outputs (Phase 1.5)

Adapters implement the `AdapterPlugin` gRPC service defined in `proto/v1/adapter_plugin.proto`. The `Info()` RPC returns metadata about the adapter including:

- `ConfigSchema` — JSON schema for agent-level configuration (on the `agent { }` block)
- `InputSchema` — JSON schema for step-level input (in the `input { }` block on each step)
- `OutputSchema` — JSON schema for outputs the adapter may return after execution

When an adapter completes execution, it returns a `Result` containing:

- `Outcome` — the named outcome that determines the FSM transition (e.g., `"success"`, `"failure"`, `"needs_review"`)
- `Outputs` — a `map[string]string` of key-value pairs that flow into the run's variable scope

Outputs are accessible in downstream workflow expressions as `steps.<step_name>.<output_key>`. For example:

<!-- validator: fragment -->
```hcl
step "get_version" {
  adapter = "shell"
  input {
    command = "git describe --tags --always"
  }
  outcome "success" { transition_to = "check_version" }
}

branch "check_version" {
  arm {
    when          = startswith(steps.get_version.stdout, "v1.")
    transition_to = "deploy_v1"
  }
  default {
    transition_to = "deploy_next"
  }
}
```

In this example:
- The `get_version` step runs a shell command and captures its output
- The shell adapter returns `stdout` as an output key
- The `branch` node evaluates `steps.get_version.stdout` to decide which path to take
- HCL expression functions like `startswith()` work against step outputs

Step outputs also flow into `for_each` iteration contexts. See [workflow.md](workflow.md) for the full expression reference.

## Writing Your Own Plugin

The canonical third-party plugin example is [`examples/plugins/greeter/`](../examples/plugins/greeter/). It lives in its own Go module (no `replace` directive once an SDK tag exists), imports only `sdk/pluginhost` and the generated proto bindings, and demonstrates the full workflow from `go build` to `criteria apply`. Read that directory first — it is the minimum viable plugin.

The public plugin SDK lives in `sdk/pluginhost`. External authors import:

```
github.com/brokenbots/criteria/sdk/pluginhost
```

The smallest plugin entrypoint is:

```go
package main

import (
    "context"
    pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
    pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

type myPlugin struct{}

func (p *myPlugin) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) {
    return &pb.InfoResponse{Name: "my-plugin", Version: "0.1.0"}, nil
}

// ... implement OpenSession, Execute, Permit, CloseSession ...

func main() {
    pluginhost.Serve(&myPlugin{})
}
```

Implement `pluginhost.Service` and call `pluginhost.Serve` from `main()`. The
`Execute` method receives a `pluginhost.ExecuteEventSender`; send at least one
`*pb.ExecuteResult` event before returning `nil`, or return a non-nil error.

See [`examples/plugins/greeter/main.go`](../examples/plugins/greeter/main.go) for a complete, runnable example. For more complex references:

- `cmd/criteria-adapter-copilot/main.go`
- `cmd/criteria-adapter-mcp/main.go`
- `cmd/criteria-adapter-noop/main.go`

If you add a new plugin, wire it through the conformance harness before relying on it in a real workflow. That is the fastest way to confirm `Info`, `OpenSession`, `Execute`, `Permit`, and `CloseSession` all obey the host contract.
