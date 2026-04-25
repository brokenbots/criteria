# Plugins and Agent Workflows

This document is the Phase 1.4 baseline for running agent-backed workflows in Overlord.

## What Plugins Are

An Overlord plugin is an out-of-process binary named `overlord-adapter-<name>`. Overseer discovers plugins in this order:

1. `${OVERLORD_PLUGINS}/overlord-adapter-<name>`
2. `~/.overlord/plugins/overlord-adapter-<name>`

Overseer does not look on `PATH`. The host starts the plugin with HashiCorp `go-plugin`; the plugin then speaks the shared gRPC adapter protocol over a local transport. The binary stays outside the Overseer process boundary, so adapter-specific runtime failures are isolated from the engine.

The first production plugin in this repo is `copilot`, shipped as `bin/overlord-adapter-copilot`.

## Installing a Plugin

Build the repo first:

```bash
make build
```

Install the plugin by copying the built binary into a plugin directory:

```bash
mkdir -p ~/.overlord/plugins
cp bin/overlord-adapter-copilot ~/.overlord/plugins/
chmod +x ~/.overlord/plugins/overlord-adapter-copilot
```

To use a temporary plugin directory instead, point Overseer at it explicitly:

```bash
tmpdir="$(mktemp -d)"
cp bin/overlord-adapter-copilot "$tmpdir/"
chmod +x "$tmpdir/overlord-adapter-copilot"
OVERLORD_PLUGINS="$tmpdir" ./bin/overseer status --castle http://localhost:8080
```

For local Copilot-backed runs you also need the `copilot` CLI available. The repo helper script documents the expected setup:

```bash
gh extension install github/gh-copilot
```

If the CLI is installed somewhere non-standard, set `OVERLORD_COPILOT_BIN=/path/to/copilot`.

## HCL Surface

Agent-backed workflows use three concepts:

1. Declare the agent once with `agent "name" { adapter = "copilot" }`.
2. Open and close the agent session explicitly with `lifecycle = "open"` and `lifecycle = "close"` steps.
3. Use the agent in normal execute-shape steps with `agent = "name"` plus plugin-specific `config` and `allow_tools`.

The canonical example is `examples/agent_hello.hcl`:

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
    config = {
      max_turns = "4"
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
- `ask` is the only execute step. For the Copilot plugin, `config.prompt` is required. `max_turns` is optional and forces a `needs_review` outcome if the plugin hits that limit.
- The prompt uses the `RESULT: <outcome>` convention. The plugin parses the final assistant message and maps that line onto the step outcome.
- Separate close steps let the workflow clean up the session and still terminate in the right state for `success`, `needs_review`, or `failure`.

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

```hcl
allow_tools = ["shell:git status"]
```

That allows exactly `git status` and nothing else.

## Running the Demo

The shortest manual path for `examples/agent_hello.hcl` is:

```bash
make build
mkdir -p ~/.overlord/plugins
cp bin/overlord-adapter-copilot ~/.overlord/plugins/
chmod +x ~/.overlord/plugins/overlord-adapter-copilot
./bin/castle --addr 127.0.0.1:8080 --db ./castle/demo.db
```

In a second terminal, run:

```bash
./bin/overseer run \
  --workflow examples/agent_hello.hcl \
  --castle http://127.0.0.1:8080 \
  --castle-codec proto
```

Expected result on the success path:

1. Overseer logs a `starting run` line with a `run_id`.
2. The Copilot plugin opens a session, requests permission for `shell:git status`, and receives a grant because the step allowlist matches.
3. The assistant reports the repository status and ends with `RESULT: success`.
4. Overseer closes the session and Castle records the run as `succeeded`.

For a one-command smoke check, use:

```bash
COPILOT_E2E=1 ./scripts/smoke-agent-hello.sh
```

That script builds the repo, installs the plugin into a temp directory, starts a local Castle, runs `agent_hello.hcl`, and asserts that the Castle run status becomes `succeeded`.

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

## Writing Your Own Plugin

The host-side plugin boundary lives in `overseer/internal/plugin/`. Adapter contract tests live in `overseer/internal/adapter/conformance/`.

The smallest plugin entrypoint is:

```go
package main

import pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"

func main() {
    pluginpkg.Serve(&MyPlugin{})
}
```

Use the existing plugin mains as references:

- `overseer/cmd/overlord-adapter-copilot/main.go`
- `overseer/cmd/overlord-adapter-mcp/main.go`
- `overseer/cmd/overlord-adapter-noop/main.go`

If you add a new plugin, wire it through the conformance harness before relying on it in a real workflow. That is the fastest way to confirm `Info`, `OpenSession`, `Execute`, `Permit`, and `CloseSession` all obey the host contract.