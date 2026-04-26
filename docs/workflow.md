# Workflow Language Reference

The Overseer workflow language is a declarative HCL-based language for orchestrating multi-step processes with complex control flow. Workflows compile to finite state machines (FSMs) that the Overseer execution engine interprets.

## Overview

An Overseer workflow defines:

- **Nodes**: steps (adapter invocations), waits (time or signal gates), approvals (human decisions), branches (conditional routing), and for-each loops (iteration over lists).
- **States**: named terminal or intermediate targets. The workflow FSM transitions between nodes and states based on outcomes.
- **Variables**: read-only typed values that seed the workflow execution. Per-run variable overrides are a future enhancement.
- **Agents**: long-lived adapter sessions that maintain state across multiple steps.

### Architecture model

- **Overseer** compiles HCL workflows to FSM graphs and executes them by invoking adapters.
- **Adapters** are out-of-process plugins discovered from `$OVERLORD_PLUGINS` or `~/.overlord/plugins` (see [plugins.md](plugins.md)).
- **Castle** (optional) is the orchestrator server that persists runs, enables resumption after crashes, and provides UI (Parapet) and approval RPCs.

### Execution modes

- **Local mode**: `overseer apply <workflow.hcl>` — runs in-process. Duration-based waits work; signal-based waits and approvals require `--castle`.
- **Orchestrator mode**: `overseer apply <workflow.hcl> --castle <url>` — connects to a Castle instance for persistence, crash recovery, and approval support.

See [Standalone CLI](#standalone-cli) for command reference.

---

## Workflow Header

Every workflow begins with a `workflow` block:

```hcl
workflow "deploy_pipeline" {
  version       = "1"
  initial_state = "validate"
  target_state  = "deployed"

  policy {
    max_total_steps  = 100
    max_step_retries = 3
  }

  permissions {
    allow_tools = ["shell:git*"]
  }

  # ... variables, agents, steps, states, etc.
}
```

### Attributes

- **`version`** (required): Schema version. Use `"1"` for v1.5 workflows.
- **`initial_state`** (required): The starting node or state name.
- **`target_state`** (required): The intended terminal state. Must reference a terminal state.
- **`policy`** (optional): Execution guards.
  - **`max_total_steps`** (default 0 = unlimited): Abort run if total step count exceeds this limit.
  - **`max_step_retries`** (default 0 = no retries): Per-step retry limit for transient failures.
- **`permissions`** (optional): Workflow-level permission allowlist.
  - **`allow_tools`**: List of glob patterns for tool invocations. Step-level `allow_tools` is unioned with this list.

---

## Variables

Variables are typed, read-only values declared at the workflow level and optionally overridden at runtime (per-run override support is a future enhancement in v1.5; currently defaults are the only source).

```hcl
variable "env" {
  type        = "string"
  default     = "staging"
  description = "Target deployment environment"
}

variable "retries" {
  type    = "number"
  default = 3
}

variable "enabled" {
  type    = "bool"
  default = true
}
```

### Supported types

- **`string`**: Text value.
- **`number`**: Numeric value (integers or floating-point).
- **`bool`**: Boolean (`true` or `false`).
- **`list(string)`**, **`list(number)`**, **`list(bool)`**: Lists of the specified element type.
- **`map(string)`**: String-keyed map with string values.

### Default values

The `default` attribute is optional. If omitted, the variable must be provided at runtime (future enhancement; currently default-only semantics apply).

**Note**: In HCL, literal lists like `["a", "b"]` are tuples. For `list(string)` variables, the compiler currently requires an exact type match. Use inline list literals in `for_each` or `input` blocks rather than variable defaults for now, or wait for the tuple-to-list coercion enhancement.

### Usage in expressions

Reference variables with `var.<name>`:

```hcl
step "deploy" {
  adapter = "shell"
  input {
    command = "deploy --env ${var.env}"
  }
  outcome "success" { transition_to = "done" }
}
```

See [Expressions](#expressions) for interpolation rules.

---

## Agents

Agents are long-lived adapter sessions that maintain state across multiple step executions. Declare agents at the workflow level and reference them from steps.

```hcl
agent "assistant" {
  adapter  = "copilot"
  on_crash = "fail"
  config {
    max_turns = 10
  }
}

step "open_assistant" {
  agent     = "assistant"
  lifecycle = "open"
  outcome "success" { transition_to = "ask_question" }
  outcome "failure" { transition_to = "failed" }
}

step "ask_question" {
  agent       = "assistant"
  allow_tools = ["shell:ls*", "shell:cat*"]
  input {
    prompt = "List files in the current directory and summarize their purpose."
  }
  outcome "success" { transition_to = "close_assistant" }
  outcome "failure" { transition_to = "failed" }
}

step "close_assistant" {
  agent     = "assistant"
  lifecycle = "close"
  outcome "success" { transition_to = "done" }
  outcome "failure" { transition_to = "failed" }
}
```

### Agent attributes

- **`adapter`** (required): Adapter name (e.g., `"copilot"`, `"mcp"`).
- **`on_crash`** (optional): Crash recovery policy: `"fail"` (default), `"respawn"`, `"abort_run"`.
- **`config`** (optional): Session-open configuration block passed to the adapter when the agent is opened. Attributes depend on the adapter's schema.

### Lifecycle steps

Agent-backed steps support three lifecycle modes:

- **`lifecycle = "open"`**: Opens the agent session. Must not include `input` or `allow_tools`.
- **`lifecycle = "close"`**: Closes the agent session. Must not include `input` or `allow_tools`.
- **Execution steps** (no `lifecycle`): Invoke the agent with input. May include `input` and `allow_tools`.

A workflow that uses an agent must open it before use and close it when done. The engine enforces session state at runtime.

### Plugin discovery

Agents (and standalone adapter steps) resolve to plugin binaries named `overlord-adapter-<name>`. Discovery order:

1. `$OVERLORD_PLUGINS/<name>`
2. `~/.overlord/plugins/<name>`

See [plugins.md](plugins.md) for the plugin wire protocol and adapter development guide.

---

## Steps

Steps are the primary execution units. Each step invokes an adapter (either directly or via an agent) and transitions to the next node based on the adapter's outcome.

```hcl
step "build" {
  adapter = "shell"
  timeout = "5m"
  input {
    command = "go build ./..."
  }
  outcome "success" { transition_to = "test" }
  outcome "failure" { transition_to = "failed" }
}
```

### Step attributes

- **`adapter`** or **`agent`** (required, mutually exclusive): Adapter name or agent reference.
- **`lifecycle`** (optional, agent-only): `"open"` or `"close"`. See [Agents](#agents).
- **`timeout`** (optional): Duration string (e.g., `"30s"`, `"5m"`). Step aborts if exceeded.
- **`allow_tools`** (optional, agent execution steps only): List of glob patterns for permitted tool invocations. Unioned with workflow-level `allow_tools`.
- **`input`** (optional): Input block for adapter configuration. Attributes are adapter-specific.
- **`outcome`** (required): At least one outcome mapping adapter outcome names to transition targets.
- **`on_crash`** (optional): Per-step crash policy; overrides agent-level or global default.

### Input block

The `input { }` block passes adapter-specific configuration. Attributes support string interpolation for variables and step outputs:

```hcl
step "publish" {
  adapter = "shell"
  input {
    command = "echo Build ID: ${steps.build.stdout}"
  }
  outcome "success" { transition_to = "done" }
}
```

See [Expressions](#expressions) for interpolation syntax.

### Adapter outputs

Adapters return outputs via the `Result.Outputs` map. Common outputs:

- **`exit_code`**: Command exit code (shell adapter).
- **`stdout`**, **`stderr`**: Captured streams.

Outputs are available to downstream steps and branch conditions as `steps.<name>.<output>`.

### Outcomes

Each `outcome` block maps an adapter-emitted outcome name to a transition target (step, state, wait, approval, branch, or for_each). For `for_each` child steps, the synthetic `_continue` target signals iteration continuation.

---

## States

States are named targets, typically terminal nodes:

```hcl
state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
```

### Attributes

- **`terminal`** (default `false`): If `true`, reaching this state ends the run.
- **`success`** (default = `terminal`): If `true`, terminal state counts as successful. Non-terminal states ignore this attribute.
- **`requires`** (optional, future): Human approval or condition gate (future enhancement).

Terminal states must be reachable from `initial_state` (enforced by compiler reachability analysis).

---

## Wait

Wait nodes pause execution for a duration or external signal.

### Duration-based wait

```hcl
wait "cool_down" {
  duration = "10s"
  outcome "elapsed" { transition_to = "retry_deploy" }
}
```

- **`duration`** (required if no `signal`): Duration string (e.g., `"5s"`, `"2m"`).
- **`outcome "elapsed"`**: Fires after the duration elapses.

**Local mode**: Duration waits work in `overseer apply` (no Castle required).

### Signal-based wait

```hcl
wait "approval_gate" {
  signal = "deploy_approved"
  outcome "approved" { transition_to = "deploy" }
  outcome "rejected" { transition_to = "aborted" }
}
```

- **`signal`** (required if no `duration`): Signal name to wait for. External caller sends signal via Castle RPC.
- **`outcome`**: Map signal values to transition targets.

**Orchestrator mode required**: Signal waits require `--castle` for external signal delivery.

---

## Approval

Approval nodes are human decision gates. Paused runs wait for an approver to submit a decision via Castle (Parapet UI or RPC).

```hcl
approval "ship_to_prod" {
  approvers = ["alice", "bob"]
  reason    = "Production deployment requires approval"
  outcome "approved" { transition_to = "deploy_prod" }
  outcome "rejected" { transition_to = "cancel_deploy" }
}
```

### Attributes

- **`approvers`** (required): List of authorized approver identifiers (user IDs or roles).
- **`reason`** (required): Human-readable prompt displayed in the approval UI.
- **`outcome "approved"`**, **`outcome "rejected"`** (both required): Transition targets for approve/reject decisions.

**Orchestrator mode required**: Approvals require `--castle`. Local-mode runs abort at compile time if approval nodes are present.

---

## Branch

Branch nodes evaluate conditions and transition to the first matching arm or the default.

```hcl
branch "check_env" {
  arm {
    when          = var.env == "prod"
    transition_to = "deploy_prod"
  }
  arm {
    when          = var.env == "staging"
    transition_to = "deploy_staging"
  }
  arm {
    when          = steps.build.exit_code == "0"
    transition_to = "deploy_dev"
  }
  default {
    transition_to = "skip_deploy"
  }
}
```

### Attributes

- **`arm`** (one or more): Conditional branches evaluated in order. First match wins.
  - **`when`**: Boolean expression. See [Expressions](#expressions).
  - **`transition_to`**: Target node if `when` is true.
- **`default`** (required): Fallback transition if no arm matches.

### Expression scope

Branch conditions may reference:

- **`var.<name>`**: Workflow variables.
- **`steps.<name>.<output>`**: Outputs from completed steps (e.g., `steps.build.exit_code`).

See [Expressions](#expressions) for syntax rules.

---

## For-each

For-each nodes iterate over a list, executing a child step once per item.

```hcl
for_each "deploy_services" {
  items = ["api", "web", "worker"]
  do    = "deploy_one"
  outcome "all_succeeded" { transition_to = "verify" }
  outcome "any_failed"    { transition_to = "rollback" }
}

step "deploy_one" {
  adapter = "shell"
  input {
    command = "deploy ${each.value} --index ${each.index}"
  }
  outcome "success" { transition_to = "_continue" }
  outcome "failure" { transition_to = "_continue" }
}
```

### Attributes

- **`items`** (required): Expression that evaluates to a list or tuple (e.g., `["a", "b"]`, `var.services`).
- **`do`** (required): Step name to execute for each item.
- **`outcome "all_succeeded"`** (required): Fires if all iterations succeed.
- **`outcome "any_failed"`** (optional but recommended): Fires if at least one iteration fails.

### Iteration scope

Within the `do` step:

- **`each.value`**: Current item value (e.g., `"api"`, `"web"`).
- **`each.index`**: Zero-based iteration index (`"0"`, `"1"`, `"2"`).

Both are available in `input { }` blocks via string interpolation.

### The `_continue` target

The child step must transition to the synthetic `_continue` target to signal iteration completion. `_continue` is not a declared node; it's an engine-internal marker that advances the for-each cursor.

If the child step transitions to any other target, the for-each loop terminates early.

### Aggregate outcomes

After all iterations complete (or early termination):

- **`all_succeeded`**: All iterations' final outcomes were non-error.
- **`any_failed`**: At least one iteration failed.

If `any_failed` is not declared, failed iterations fall through to `all_succeeded` (compiler emits a warning).

---

## Expressions

Expressions are used in `when` conditions, `items` lists, and `input { }` attribute values.

### String interpolation

Use `${...}` inside string literals:

```hcl
input {
  command = "deploy --env ${var.env} --build ${steps.build.stdout}"
}
```

### Available scopes

- **`var.<name>`**: References workflow variables.
- **`steps.<name>.<output>`**: References outputs from completed steps (e.g., `exit_code`, `stdout`).
- **`each.value`**, **`each.index`**: Available within for-each child steps.

### Type rules

- Comparison operators (`==`, `!=`, `<`, `>`, `<=`, `>=`) follow HCL semantics.
- Boolean operators: `&&`, `||`, `!`.
- String concatenation is implicit in interpolated strings.

### Compile-time vs. runtime evaluation

- **Compile-time**: Variable defaults, static list literals.
- **Runtime**: Variable overrides (future), step outputs, `each.*` scope (evaluated per iteration).

Expressions that reference step outputs or `each.*` are stored as raw HCL expressions in the compiled graph and evaluated at step entry.

---

## Permissions

Overseer enforces a deny-by-default permission model for tool invocations (currently agent-based steps only; future: all adapter tool use).

### Workflow-level permissions

```hcl
workflow "secure_build" {
  permissions {
    allow_tools = ["shell:git*", "shell:make*"]
  }
  # ...
}
```

Applies to all agent steps unless overridden.

### Step-level permissions

```hcl
step "build" {
  agent       = "assistant"
  allow_tools = ["shell:go*build*"]
  input { prompt = "Run go build" }
  outcome "success" { transition_to = "done" }
}
```

The effective allowlist is the union of workflow-level and step-level patterns.

### Pattern matching

Tool names are matched against glob patterns using `filepath.Match` semantics:

- `shell:git*` permits `shell:git status`, `shell:git commit`, etc.
- `shell:*` permits all shell commands.
- `*` permits all tools (use with caution).

See [plugins.md](plugins.md) for the tool invocation wire protocol.

---

## Standalone CLI

Overseer provides three commands for workflow operations:

### `overseer compile`

Parses and validates a workflow, outputs JSON or DOT graph.

```bash
bin/overseer compile examples/demo_tour.hcl
bin/overseer compile examples/demo_tour.hcl --format dot --out workflow.dot
```

**Outputs**:
- **JSON** (default): FSM graph with nodes, outcomes, and metadata.
- **DOT**: Graphviz-compatible directed graph for visualization.

### `overseer plan`

Human-readable summary of the workflow structure.

```bash
bin/overseer plan examples/demo_tour.hcl
```

Prints:
- Variables, agents, steps (in declaration order).
- States, wait nodes, approval nodes, branches, for-each loops.
- Plugins required.

### `overseer apply`

Executes the workflow.

**Local mode** (no Castle):

```bash
bin/overseer apply examples/build_and_test.hcl
```

Streams ND-JSON events to stdout. Duration waits work; signal waits and approvals abort.

**Orchestrator mode** (with Castle):

```bash
bin/overseer apply examples/demo_tour.hcl --castle http://localhost:8080
```

Connects to Castle, persists run state, supports resumption and approvals.

**Flags**:
- **`--castle <url>`**: Castle base URL (orchestrator mode).
- **`--events-file <path>`**: Write events to file instead of stdout (local mode).
- **`--name <name>`**: Overseer instance identifier (defaults to hostname).
- **`--castle-tls <mode>`**: TLS mode (`disable`, `tls`, `mtls`).

### ND-JSON event stream

All events are schema-versioned ND-JSON objects:

```json
{"schema_version":1,"seq":1,"run_id":"...","payload_type":"RunStarted","payload":{...}}
{"schema_version":1,"seq":2,"run_id":"...","payload_type":"StepEntered","payload":{...}}
{"schema_version":1,"seq":3,"run_id":"...","payload_type":"StepLog","payload":{...}}
```

**Event types**:
- `RunStarted`, `RunCompleted`
- `StepEntered`, `StepOutcome`, `StepOutputCaptured`, `StepTransition`, `StepLog`
- `ForEachEntered`, `ForEachIteration`, `ForEachOutcome`
- `WaitEntered`, `WaitResumed`
- `ApprovalRequested`, `ApprovalDecided`
- `BranchEvaluated`

See [api/README.md](../api/README.md) for proto definitions and event schemas.

### Local-mode constraints

- Duration-based waits work.
- Signal-based waits abort with "signal waits require --castle".
- Approval nodes abort at workflow validation (before execution starts).
- No crash recovery or run persistence.

For examples demonstrating each command, see:
- Local-only workflow: [examples/build_and_test.hcl](../examples/build_and_test.hcl)
- Orchestrator-required workflow: [examples/demo_tour.hcl](../examples/demo_tour.hcl)

---

## Future Shape (Appendix)

This section outlines language features planned for post-1.5 phases. **None of these are implemented in v1.5**; they are noted here to set expectations and demonstrate forward-thinking design.

### Parallel regions (future)

Parallel execution of independent step sequences:

```hcl
parallel "build_and_test" {
  region "build" {
    steps = ["compile", "package"]
  }
  region "test" {
    steps = ["unit_tests", "integration_tests"]
  }
  outcome "all_succeeded" { transition_to = "deploy" }
  outcome "any_failed"    { transition_to = "failed" }
}
```

**Not implemented in v1.5**. Requires engine scheduler enhancements and cross-region synchronization primitives.

### Sub-workflow composition (future)

Embed reusable workflow fragments:

```hcl
sub_workflow "smoke_test" {
  source = "workflows/smoke.hcl"
  inputs = {
    env = var.env
  }
  outcome "success" { transition_to = "deploy_prod" }
  outcome "failure" { transition_to = "rollback" }
}
```

**Not implemented in v1.5**. Requires workflow registry, input/output contracts, and nested execution context.

### Variable overrides at runtime (future enhancement)

Currently, variable defaults are the only source. Per-run overrides (e.g., `overseer apply --var env=prod`) are planned post-1.5.

### Immediately-next work: Phase 1.6 repo split

Phase 1.6 splits the monorepo into:

- **`overlord-overseer`**: Workflow engine, compiler, and standalone CLI (this document).
- **`overlord-castle`**: Orchestrator server, Parapet UI, and Castle RPC.
- **`overlord-proto`**: Shared protobuf contracts and event schemas.

See [PLAN.md §1.6](../PLAN.md) for the split roadmap. Parallel regions and sub-workflow composition are targeted for post-split language work (likely Phase 1.7+).

