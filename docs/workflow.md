# Workflow Language Reference

The Criteria workflow language is a declarative HCL-based language for orchestrating multi-step processes with complex control flow. Workflows compile to finite state machines (FSMs) that the Criteria execution engine interprets.

## Overview

A Criteria workflow defines:

- **Nodes**: steps (adapter invocations), waits (time or signal gates), approvals (human decisions), branches (conditional routing), and for-each loops (iteration over lists).
- **States**: named terminal or intermediate targets. The workflow FSM transitions between nodes and states based on outcomes.
- **Variables**: read-only typed values that seed the workflow execution. Per-run variable overrides are a future enhancement.
- **Agents**: long-lived adapter sessions that maintain state across multiple steps.

### Architecture model

- **Criteria** compiles HCL workflows to FSM graphs and executes them by invoking adapters.
- **Adapters** are out-of-process plugins discovered from `$CRITERIA_PLUGINS` or `~/.criteria/plugins` (see [plugins.md](plugins.md)).
- **Server** (optional) is the orchestrator server that persists runs, enables resumption after crashes, and provides UI and approval RPCs.

### Execution modes

- **Local mode**: `criteria apply <workflow.hcl>` — runs in-process. Duration-based waits work; signal-based waits and approvals require `--server`.
- **Orchestrator mode**: `criteria apply <workflow.hcl> --server <url>` — connects to a server instance for persistence, crash recovery, and approval support.

See [Standalone CLI](#standalone-cli) for command reference.

---

## Workflow Header

Every workflow begins with a `workflow` block:

<!-- validator: skip: illustrative header showing structure only; initial_state and target_state reference nodes not defined in this excerpt -->
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
  - **`max_total_steps`** (default 100): Caps the total number of step executions across the run, including retries and `for_each` iterations. Set to `0` for no cap (use with care for unbounded `for_each` or recursive workflows).
  - **`max_step_retries`** (default 0 = no retries): Per-step retry limit for transient failures.
- **`permissions`** (optional): Workflow-level permission allowlist.
  - **`allow_tools`**: List of glob patterns for tool invocations. Step-level `allow_tools` is unioned with this list.

---

## Variables

Variables are typed, read-only values declared at the workflow level and optionally overridden at runtime (per-run override support is a future enhancement in v1.5; currently defaults are the only source).

<!-- validator: fragment -->
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

<!-- validator: fragment -->
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

<!-- validator: fragment -->
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

Agents (and standalone adapter steps) resolve to plugin binaries named `criteria-adapter-<name>`. Discovery order:

1. `$CRITERIA_PLUGINS/<name>`
2. `~/.criteria/plugins/<name>`

See [plugins.md](plugins.md) for the plugin wire protocol and adapter development guide.

---

## Steps

Steps are the primary execution units. Each step invokes an adapter (either directly or via an agent) and transitions to the next node based on the adapter's outcome.

<!-- validator: fragment -->
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

<!-- validator: fragment -->
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

<!-- validator: fragment -->
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

<!-- validator: fragment -->
```hcl
wait "cool_down" {
  duration = "10s"
  outcome "elapsed" { transition_to = "retry_deploy" }
}
```

- **`duration`** (required if no `signal`): Duration string (e.g., `"5s"`, `"2m"`).
- **`outcome "elapsed"`**: Fires after the duration elapses.

**Local mode**: Duration waits work in `criteria apply` (no server required).

### Signal-based wait

<!-- validator: fragment -->
```hcl
wait "approval_gate" {
  signal = "deploy_approved"
  outcome "approved" { transition_to = "deploy" }
  outcome "rejected" { transition_to = "aborted" }
}
```

- **`signal`** (required if no `duration`): Signal name to wait for. External caller sends signal via server RPC.
- **`outcome`**: Map signal values to transition targets.

**Orchestrator mode required**: Signal waits require `--server` for external signal delivery.

---

## Approval

Approval nodes are human decision gates. Paused runs wait for an approver to submit a decision via the server (UI or RPC).

<!-- validator: fragment -->
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

**Orchestrator mode required**: Approvals require `--server`. Local-mode runs abort at compile time if approval nodes are present.

---

## Branch

Branch nodes evaluate conditions and transition to the first matching arm or the default.

<!-- validator: skip: branch arms reference var.env and steps.build which are declared outside this excerpt -->
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

<!-- validator: fragment -->
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

<!-- validator: skip: bare input block; sub-block of step, not valid at workflow level -->
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

### Expression functions

The following built-in functions are available in `input { }` blocks, `when` conditions, `items` lists, and anywhere else an expression is accepted.

#### `file(path)`

Reads the file at `path` (resolved relative to the workflow `.hcl` file's directory) and returns its content as a UTF-8 string. Equivalent to inlining a static file.

```hcl
input {
  prompt = file("./prompts/classify.md")
}
```

**Constraints:**
- `path` must be relative to the workflow directory (absolute paths and `..` traversal that escapes the workflow directory are rejected). To permit access outside the workflow directory, add directories to the `CRITERIA_WORKFLOW_ALLOWED_PATHS` environment variable (colon-separated).
- Files larger than `CRITERIA_FILE_FUNC_MAX_BYTES` bytes are rejected (default: 1 MiB; clamped to [1 KiB, 64 MiB]).
- The file content must be valid UTF-8.
- Compile-time validation: when the argument is a string literal (no variable references), `file()` is validated at `criteria compile`/`criteria validate`/`criteria apply` time. Missing or path-escaping files produce compile errors with source ranges.
- When the argument contains variable references (e.g. `file(var.path)`), validation is deferred to runtime.

#### `fileexists(path)`

Returns `true` if `path` resolves to a readable regular file under the workflow directory; `false` for missing paths or directories. Path confinement rules are the same as `file()`.

```hcl
input {
  use_custom = fileexists("./custom_prompt.md") ? "yes" : "no"
}
```

#### `trimfrontmatter(content)`

Strips a YAML frontmatter block from `content` and returns the remainder. If no frontmatter is present, or the closing `---` delimiter does not appear within the first 64 KiB, the input is returned unchanged.

```hcl
input {
  command = trimfrontmatter(file("./run_script.md"))
}
```

The frontmatter block must begin with `---\n` and be closed by a `\n---\n` within 64 KiB. Everything after the closing delimiter is returned.

#### Combining functions

`file()` and `trimfrontmatter()` compose naturally to load Markdown prompts with YAML metadata:

```hcl
input {
  prompt = trimfrontmatter(file("./prompts/task.md"))
}
```

The `examples/file_function.hcl` workflow demonstrates this pattern end-to-end.

**Environment variables:**

| Variable | Effect |
|---|---|
| `CRITERIA_FILE_FUNC_MAX_BYTES` | Integer; maximum bytes `file()` will read. Default 1 MiB. Clamped to [1024, 67108864]. |
| `CRITERIA_WORKFLOW_ALLOWED_PATHS` | Colon-separated list of directories `file()` and `fileexists()` may access outside the workflow directory. |

---

## Permissions

Criteria enforces a deny-by-default permission model for tool invocations (currently agent-based steps only; future: all adapter tool use).

### Workflow-level permissions

<!-- validator: skip: incomplete workflow block, missing version/initial_state/target_state -->
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

<!-- validator: skip: step uses agent = "assistant" which is declared outside this excerpt -->
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

Criteria provides three commands for workflow operations:

### `criteria compile`

Parses and validates a workflow, outputs JSON or DOT graph.

```bash
bin/criteria compile examples/demo_tour_local.hcl
bin/criteria compile examples/demo_tour_local.hcl --format dot --out workflow.dot
```

**Outputs**:
- **JSON** (default): FSM graph with nodes, outcomes, and metadata.
- **DOT**: Graphviz-compatible directed graph for visualization.

### `criteria plan`

Human-readable summary of the workflow structure.

```bash
bin/criteria plan examples/demo_tour_local.hcl
```

Prints:
- Variables, agents, steps (in declaration order).
- States, wait nodes, approval nodes, branches, for-each loops.
- Plugins required.

### `criteria apply`

Executes the workflow.

**Local mode** (no server):

```bash
bin/criteria apply examples/build_and_test.hcl
```

Streams ND-JSON events to stdout. Duration waits work; signal waits and approvals abort.

**Orchestrator mode** (with server):

```bash
bin/criteria apply <workflow.hcl> --server http://localhost:8080
```

Connects to the server, persists run state, supports resumption and approvals.

**Flags**:
- **`--server <url>`: Server base URL (orchestrator mode).
- **`--events-file <path>`**: Write events to file instead of stdout (local mode).
- **`--name <name>`: Criteria instance identifier (defaults to hostname).
- **`--server-tls <mode>`: TLS mode (`disable`, `tls`, `mtls`).

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

See [`proto/criteria/v1/`](../proto/criteria/v1/) for proto definitions and event schemas.

### Local-mode constraints

- Duration-based waits work.
- Signal-based waits abort with "signal waits require --server".
- Approval nodes abort at workflow validation (before execution starts).
- No crash recovery or run persistence.

For examples demonstrating each command, see:
- Local-only workflow: [examples/build_and_test.hcl](../examples/build_and_test.hcl)
- Full-featured local demo: [examples/demo_tour_local.hcl](../examples/demo_tour_local.hcl)

---

## Doc-Example Validation

The `make validate-docs` CI gate extracts every fenced HCL code block from `docs/*.md` and runs `bin/criteria validate` against each. This catches syntax regressions before they reach users.

### Directives

Place these HTML comment directives on the line immediately before the opening ` ```hcl ` fence (no blank line between the directive and the fence):

- **`<!-- validator: fragment -->`** — the block is a partial workflow (a step, state, agent, or other node declaration without a surrounding `workflow { }` block). The validator wraps it in a synthetic `workflow "doc_example" { ... }` shell and adds state stubs for any transition targets not defined in the fragment.

- **`<!-- validator: skip: <reason> -->`** — skip this block entirely. Use sparingly. Always document why each skip exists. Valid reasons: the block is an incomplete `workflow { }` excerpt that references undeclared nodes; the block is a bare attribute or sub-block not valid at workflow level; the block shows a future language feature not yet implemented.

### Examples

Fragment wrapping (most step/state/agent snippets):

```
<!-- validator: fragment -->
` ``` `hcl
step "build" {
  adapter = "shell"
  ...
}
` ``` `
```

Explicit skip (when fragment wrapping cannot resolve references):

```
<!-- validator: skip: branch references var.env declared outside this excerpt -->
` ``` `hcl
branch "check_env" {
  ...
}
` ``` `
```

Blocks with no directive and a top-level `workflow { }` are validated as-is. Blocks with no directive and no top-level `workflow { }` are automatically treated as fragments.

---

## Future Shape (Appendix)

This section outlines language features planned for post-1.5 phases. **None of these are implemented in v1.5**; they are noted here to set expectations and demonstrate forward-thinking design.

### Parallel regions (future)

Parallel execution of independent step sequences:

<!-- validator: skip: not implemented in v1.5; parallel block is not a recognized workflow node type -->
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

<!-- validator: skip: not implemented in v1.5; sub_workflow block is not a recognized workflow node type -->
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

Currently, variable defaults are the only source. Per-run overrides (e.g., `criteria apply --var env=prod`) are planned post-1.5.

### Repository layout

The criteria project ships as a single repository:

- **`github.com/brokenbots/criteria`** — workflow engine, compiler, and standalone CLI (this document); the `cmd/criteria-adapter-*` plugin binaries live here too.
- **`github.com/brokenbots/criteria/sdk`** — published Go SDK; shared protobuf contracts and event schemas live under `sdk/pb/criteria/v1`.

The orchestrator side is developed separately at [github.com/brokenbots/orchestrator](https://github.com/brokenbots/orchestrator) and consumes the published SDK. Parallel regions and sub-workflow composition are targeted as future language work — see [PLAN.md](../PLAN.md).

