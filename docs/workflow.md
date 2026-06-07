# Workflow Language Reference

The Criteria workflow language is a declarative HCL language for multi-step
processes with branching and iteration. Workflows compile to a finite-state
machine (FSM) that the engine interprets.

For the dense, normative reference (every block, attribute, function, and
binding) see [LANGUAGE-SPEC.md](LANGUAGE-SPEC.md), or run `criteria spec`. This
document is the prose companion.

## Overview

A workflow declares:

- **Steps** — adapter or subworkflow invocations. Iterate with `for_each`,
  `count`, `parallel`, or `while`.
- **States** — named nodes, usually terminal. The FSM transitions between nodes
  and states based on step outcomes.
- **Waits, approvals, switches** — time/signal gates, human decision gates, and
  conditional routing.
- **Variables, locals, data values, outputs** — typed values that seed and
  thread state through a run.
- **Adapters** — out-of-process sessions that execute steps. Declared with
  `adapter "<type>" "<name>" { }` and referenced via `target`. The engine opens
  and closes sessions automatically as steps enter and exit scope.

### Execution modes

- **Local** — `criteria apply <workflow.hcl|dir>`: runs in-process, no server.
  Duration waits work. Signal waits and approvals require either a server or
  `CRITERIA_LOCAL_APPROVAL` (see [Local-mode approval and signal wait](#local-mode-approval-and-signal-wait)).
- **Server** — `criteria apply <workflow.hcl|dir> --server <url>`: connects to an
  orchestrator for run persistence, crash recovery, and approval delivery. Server
  mode is early and server-oriented; see [README → Component status](../README.md#component-status).

See [Standalone CLI](#standalone-cli) for the command reference.

---

## Workflow Header

A Criteria workflow module consists of one or more `.hcl` files. In a **single-file workflow**, the file contains both the `workflow` header block and all content declarations. In a **multi-file (directory) module**, exactly one file contains the `workflow` header block and sibling files contain only content declarations (steps, states, adapters, etc.).

<!-- validator: skip: illustrative header showing structure only; initial_state and target_state reference nodes not defined in this excerpt -->
```hcl
workflow {
  name          = "deploy_pipeline"
  version       = "1"
  initial_state = "validate"
  target_state  = "deployed"
}

policy {
  max_total_steps  = 100
  max_step_retries = 3
}

permissions {
  allow_tools = ["shell:git*"]
}

# ... variables, adapters, steps, states, etc.
```

### Attributes

- **`version`** (required): Language version. Use `"1"`.
- **`initial_state`** (required): The starting node or state name.
- **`target_state`** (required): The intended terminal state. Must reference a terminal state.
- **`verification`** (optional): Signature-verification posture for OCI adapters — `"strict"`, `"warn"`, or `"off"`. Governs how a failed/missing adapter signature is handled at `lock`/`compile`/`apply`. The CLI override `--allow-unsigned` (or `CRITERIA_ALLOW_UNSIGNED=1`) takes precedence over this attribute. When omitted, the CLI transition default applies (currently `warn`; returns to `strict` once keyless verification is confirmed). See [adapters.md → Signing and trust](adapters.md).
- **`policy`** (optional, top-level block): Execution guards.
  - **`max_total_steps`** (default 100): Caps the total number of step executions across the run, including retries and iteration steps. Set this to a positive integer to override the cap. If unset, or set to `0`, the default cap of `100` applies. Acts as a coarse backstop; for fine-grained loop control, prefer `max_visits` on individual steps.
  - **`max_step_retries`** (default 0 = no retries): Per-step retry limit for transient failures.
  - **`max_visits_warn_threshold`** (default 200): Controls when the compiler emits a back-edge warning for steps without `max_visits`. When `max_total_steps` exceeds this threshold and a step has a back-edge (can reach itself via outcome transitions) but no `max_visits`, the compiler emits a warning suggesting `max_visits` be set. Supported values: omit (or leave unset) to use the default threshold of 200; set to `0` to disable warnings entirely; set to a positive integer to override the default. Negative values are invalid and cause a compile error.
- **`permissions`** (optional, top-level block): Workflow-level permission allowlist.
  - **`allow_tools`**: List of glob patterns for tool invocations. Step-level `allow_tools` is unioned with this list.

### Directory mode (multi-file workflows)

A workflow can be split across multiple `.hcl` or `.chcl` files in a directory. When `criteria apply` receives a directory path, it reads all `.hcl` and `.chcl` files and merges their declarations:

```
my-workflow/
  workflow.hcl    # contains the workflow header block (`.chcl` is equally valid)
  adapters.hcl    # adapter declarations (`.chcl` is equally valid)
  variables.hcl   # variable declarations (`.chcl` is equally valid)
  steps.hcl       # step, state, and other declarations (`.chcl` is equally valid)
```

Each file must be a valid standalone HCL document. The `workflow { name = "..." }` header block (with `version`, `initial_state`, `target_state`) must appear in **exactly one** file in the directory; all other files are content-only (no workflow block). All top-level blocks are merged across all files in alphabetical order. Duplicate name declarations across files produce a compile error.

See `examples/subworkflow/` for a working multi-file example.

#### File path entry points

Passing a `.hcl` or `.chcl` file path (e.g. `criteria apply my-workflow/workflow.hcl`) is equivalent to passing the parent directory: all `.hcl` and `.chcl` files in the parent directory are merged together as one module. This lets you point any CLI command at a specific file within a split module.

Every workflow must live in its own directory — a directory may contain exactly one `workflow` header block across all its `.hcl` and `.chcl` files. If the parent directory contains multiple `workflow` header blocks, the command fails with a "duplicate workflow block" error.

Only `.hcl` and `.chcl` files are accepted as file-path entry points. Passing a non-HCL file is an error.

---

## Variables

Variables are typed, read-only values declared at the workflow level. The
`default` attribute is the usual value source; override per run with `--var` or
`--var-file` (see [CLI reference](#standalone-cli)).

<!-- validator: fragment -->
```hcl
variable "env" {
  type        = string
  default     = "staging"
  description = "Target deployment environment"
}

variable "retries" {
  type    = number
  default = 3
}

variable "enabled" {
  type    = bool
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

The `default` attribute is optional. If omitted, the variable must be supplied at
runtime via `--var` or `--var-file`.

**Note**: In HCL, literal list syntax `["a", "b"]` produces a tuple. The compiler accepts tuple literals where a list type is declared and the element types are compatible — no explicit `tolist()` cast is needed.

### Usage in expressions

Reference variables with `var.<name>`:

<!-- validator: skip: illustrative fragment; adapter block not included in this excerpt -->
```hcl
adapter "shell" "default" {
  config {}
}

step "deploy" {
  target = adapter.shell.default
  input {
    command = "deploy --env ${var.env}"
  }
  outcome "success" { next = state.done }
}
```

See [Expressions](#expressions) for interpolation rules.

---

## Environments

> **Status: Untested.** Environment blocks are implemented but have had minimal
> real testing (see [README → Component status](../README.md#component-status)).
> The `shell` type is the only one exercised; `sandbox`, `container`, and
> `remote` isolation are described in [adapters.md → Environments](adapters.md#environments).

Environments declare typed execution contexts bound to adapter steps. They inject
environment variables and select an isolation boundary for the adapter process.

### Declaring environments

<!-- validator: fragment -->
```hcl
environment "shell" "production" {
  variables = {
    CI = "true"
    LOG_LEVEL = "info"
    SERVICE_ENVIRONMENT = "prod"
  }
  config = {
    timeout_seconds = 300
    retry_strategy = "exponential"
  }
}

environment "shell" "staging" {
  variables = {
    CI = "true"
    LOG_LEVEL = "debug"
    SERVICE_ENVIRONMENT = "staging"
  }
  config = {
    timeout_seconds = 120
  }
}
```

### Attributes

- **`<type>`** (required label): The environment type — `shell`, `sandbox`, `container`, or `remote`. Only `shell` is exercised; see [adapters.md → Environments](adapters.md#environments) for the isolation semantics of the others.
- **`<name>`** (required label): The environment name. Must match `^[a-zA-Z][a-zA-Z0-9_-]*$` (starts with a letter; then letters, digits, underscores, hyphens).
- **`variables`** (optional): Map of environment variable names to string values. Numbers and booleans are coerced to strings. All values must fold at compile time (no runtime-only references like `each.value` or `steps.X.outputs.Y`).
- **`working_directory`** (optional): Launch directory (cwd) for the adapter process. Resolved at adapter-session init, not folded at compile time, so it can be set from run variables and locals (e.g. `working_directory = var.worktree`). References that cannot resolve at init (e.g. `steps.X.outputs.Y`) produce a runtime error. Accepted by `shell`, `sandbox`, and `remote`; **not** `container` (which isolates paths rather than relocating cwd). Under `sandbox`, the path must also be permitted by the filesystem policy.
- **`config`** (optional): Map of type-specific configuration, parsed and stored. Shape is not validated.

### Default environment

If a workflow declares exactly one environment, that environment becomes the default and is automatically bound to all adapter steps. If multiple environments are declared, you must explicitly set the default:

<!-- validator: skip: workflow header with environment attribute; states not defined in excerpt -->
```hcl
workflow {
  name          = "multi_env_workflow"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
  environment   = shell.production

  # ... environments, steps, etc.
}
```

In the workflow header, the `environment = <type>.<name>` attribute serves as the explicit default environment for the workflow. If no environment is set and multiple environments are declared, the workflow is valid at compile time, but runtime execution may fail if steps expect an environment to be bound.

### Runtime behavior

When a step runs under an environment, the environment's `variables` map is
injected into the adapter subprocess. For the shell adapter these become shell
environment variables:

```hcl
step "deploy" {
  target = adapter.shell.default
  input {
    command = "echo $LOG_LEVEL"  # prints "debug" or "info" per env
  }
  outcome "success" { next = state.done }
}
```

The controlled-environment allowlist is preserved; injected variables are added
to the safe set. If an injected variable conflicts with a security-critical
variable (e.g. `PATH`), the controlled set wins and the compiler emits a warning.

---

## Adapters

Adapters are out-of-process plugin sessions declared at the workflow level and referenced from steps via `step.target`. The engine opens a session automatically when the first step that uses the adapter is entered and closes it automatically when the last step exits scope (LIFO order). No explicit open or close steps are needed.

<!-- validator: skip: illustrative excerpt; workflow header and state blocks omitted -->
```hcl
adapter "copilot" "assistant" {
  source   = "ghcr.io/brokenbots/criteria-adapter-copilot"
  on_crash = "fail"
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "medium"
    max_turns        = 10
  }
}

step "list_files" {
  target      = adapter.copilot.assistant
  allow_tools = ["shell:ls*", "shell:cat*"]
  input {
    prompt = "List files in the current directory and summarize their purpose."
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
```

### Adapter block attributes

- **`<type>`** (first label, required): Adapter type (e.g. `shell`, `copilot`).
- **`<name>`** (second label, required): Instance name. Multiple instances of one type may be declared with different names.
- **`source`** (optional): OCI location of the adapter artifact (registry/repo path or registry alias), decoupled from version. Required for OCI-backed adapters; omit when registering a binary with `criteria adapter dev`.
- **`version`** (optional): Semver constraint resolved at lock time — exact (`"1.2.3"`), caret (`"^1.2"`), tilde (`"~1.2.0"`), wildcard (`"1.x"`), or `"latest"`. The lockfile pins the resolved digest.
- **`on_crash`** (optional): Crash policy: `"fail"` (default), `"respawn"`, `"abort_run"`.
- **`config`** (optional): Session-open configuration. Attributes are adapter-specific. See [adapters.md](adapters.md) for the distribution, signing, and per-adapter config model.

### Automatic lifecycle

The engine manages the full adapter session lifecycle without any explicit workflow steps:

- **Open**: the session is opened before the first step targeting this adapter executes.
- **Close**: the session is closed after the last step targeting this adapter in the current scope exits (including error paths).
- **LIFO order**: when multiple adapters are declared, they close in reverse declaration order.

### Resolution and distribution

Adapters are out-of-process binaries distributed as cosign-signed OCI artifacts.
A workflow references one by `source`; `criteria adapter lock` resolves, pulls,
verifies, and pins it by digest in `.criteria.lock.hcl`. For local iteration,
`criteria adapter dev <binary>` registers a binary directly (skipping the
lockfile and signature checks). See [adapters.md](adapters.md) for the full
distribution, signing, and wire-protocol model.

---

## Steps

Steps are the primary execution units. Each step invokes an adapter (or a subworkflow) and transitions to the next node based on the outcome.

<!-- validator: fragment -->
```hcl
step "build" {
  target  = adapter.shell.default
  timeout = "5m"
  input {
    command = "go build ./..."
  }
  outcome "success" { next = step.test }
  outcome "failure" { next = state.failed }
}
```

### Step attributes

- **`target`** (required): The execution target for this step. Two forms are accepted:
  - `adapter.<type>.<name>` — invokes the named adapter instance (e.g. `adapter.shell.default`).
  - `subworkflow.<name>` — invokes a `subworkflow` block declared in the same workflow file (e.g. `subworkflow.setup`).
  Subworkflow steps always produce a `"success"` outcome on completion or `"failure"` on error.
- **`timeout`** (optional): Duration string (e.g., `"30s"`, `"5m"`). Step aborts if exceeded.
- **`max_visits`** (optional, default 0 = unlimited): Maximum number of adapter invocation attempts this step may consume in a single run. Each adapter invocation — including the initial attempt and each retry attempt within a `max_step_retries` budget — counts as one visit. For iterating steps (`for_each`/`count`), entering an iteration consumes one visit for that iteration's initial adapter invocation; any retries within that same iteration consume additional visits and also count against `max_visits`. When the visit count would exceed this limit, the run fails immediately with `step "<name>" exceeded max_visits (<N>)`. A value of `0` (default) means unlimited. Negative values are rejected at compile time. This is the preferred mechanism for bounding tight review loops; see also `max_total_steps` in the policy block for a coarser run-wide cap.
- **`allow_tools`** (optional, adapter execution steps only): List of glob patterns for permitted tool invocations. Unioned with workflow-level `allow_tools`.
- **`input`** (optional): Input block for adapter configuration. Attributes are adapter-specific.
- **`outcome`** (required): At least one outcome mapping adapter outcome names to transition targets.
- **`on_crash`** (optional): Per-step crash policy; overrides adapter-level or global default.

### Input block

The `input { }` block passes adapter-specific configuration. Attributes support string interpolation for variables and step outputs:

<!-- validator: fragment -->
```hcl
step "publish" {
  target = adapter.shell.default
  input {
    command = "echo Build ID: ${steps.build.stdout}"
  }
  outcome "success" { next = state.done }
}
```

See [Expressions](#expressions) for interpolation syntax.

### Step-level environment override

Adapter-targeted steps can bind to a specific declared environment at the step level, overriding the workflow default:

```hcl
step "deploy" {
  target      = adapter.shell.default
  environment = shell.production
  input {
    command = "deploy.sh"
  }
  outcome "success" { next = state.done }
}
```

Key points:
- **Bare traversal required**: `environment = shell.production` (no quotes). Quoted strings are rejected at compile time with a migration hint.
- **Validated at compile time**: the referenced environment (`<type>.<name>`) must be declared in the same workflow; a missing reference is a compile error.
- **Adapter steps only**: `environment` on a subworkflow-targeted step (`target = subworkflow.<name>`) is a compile error. To bind a subworkflow to an environment, set it on the subworkflow declaration: `subworkflow "inner" { environment = shell.ci }`.

### Adapter outputs

Adapters return outputs via the `Result.Outputs` map. Common outputs:

- **`exit_code`**: Command exit code (shell adapter).
- **`stdout`**, **`stderr`**: Captured streams.

Outputs are available to downstream steps and switch conditions as `steps.<name>.<output>`.

### Outcomes

Each `outcome` block maps an adapter-emitted outcome name to a transition target (step, state, wait, approval, switch, or another iterating step). For steps inside an iteration body, the synthetic `_continue` target signals iteration continuation.

#### Outcome block attributes

- **`next`** (required): The name of the next node to transition to. Two reserved values have special semantics:
  - A step, state, wait, approval, or switch name — standard transition.
  - **`"return"`** — halts the current scope (the workflow body or a subworkflow invocation) and returns to the caller. In a subworkflow, the caller's step outcome is then applied. At the top level, `return` terminates the run as successful with any projected outputs. See **Return semantics** below.
- **`output`** (optional): An HCL object expression that projects a custom output map for this outcome. When present, the projected map replaces the step's full adapter output for the purpose of downstream `steps.<name>.*` references and subworkflow return values. When absent, the step's full adapter output passes through unchanged.

#### Return semantics (`next = return`)

When a step outcome specifies `next = return`, the engine exits the current scope immediately:

- **In a subworkflow**: the subworkflow exits and the parent step sees a `"success"` outcome (or `"failure"` if the return was triggered by an error path). The `output` projection from the triggering outcome becomes the subworkflow step's outputs, accessible as `steps.<step_name>.*` in the parent scope.
- **At the top level**: the run terminates as successful. If the outcome includes `output = { ... }`, that projection IS the run's output set — it overrides any top-level `output` blocks declared in the workflow.

**Precedence**: `outcome.output` always wins over top-level `output` block declarations when `next = return` is used. Top-level `output` blocks provide the default output set for normal terminal-state exits.

#### `outcome "default"`

The optional `outcome "default"` block provides a fallback when an adapter returns an outcome name that is not in the step's declared outcome set:

- If declared, the unknown outcome name is silently mapped to the default block. A `step.outcome.defaulted` event is emitted with both the original and mapped names so operators can audit the mapping.
- If not declared, an unknown outcome is a runtime error (`step.outcome.unknown` event).

The `outcome "default"` block is declared just like any other outcome block, using the reserved name `"default"`. It may include `next`, `output`, and `write` the same way every other outcome does.

```hcl
step "call_agent" {
  target = adapter.copilot.reviewer

  outcome "approved" {
    next = step.deploy
  }
  outcome "default" {
    next   = "return"
    output = { reason = "review required" }
  }
}
```

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
- **`requires`** (optional): Names a prerequisite state. **Not enforced** — parsed and stored but the engine does not yet gate on it.

Terminal states must be reachable from `initial_state` (enforced by compiler reachability analysis).

---

## Wait

Wait nodes pause execution for a duration or external signal.

### Duration-based wait

<!-- validator: fragment -->
```hcl
wait "cool_down" {
  duration = "10s"
  outcome "elapsed" { next = step.retry_deploy }
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
  outcome "approved" { next = step.deploy }
  outcome "rejected" { next = state.aborted }
}
```

- **`signal`** (required if no `duration`): Signal name to wait for. External caller sends signal via server RPC.
- **`outcome`**: Map signal values to transition targets.

**Orchestrator mode required**: Signal waits require `--server` for external signal delivery. See **Local-mode approval and signal wait** below for running without a server.

---

## Approval

Approval nodes are human decision gates. Paused runs wait for an approver to submit a decision via the server (UI or RPC).

<!-- validator: fragment -->
```hcl
approval "ship_to_prod" {
  approvers = ["alice", "bob"]
  reason    = "Production deployment requires approval"
  outcome "approved" { next = step.deploy_prod }
  outcome "rejected" { next = step.cancel_deploy }
}
```

### Attributes

- **`approvers`** (required): List of authorized approver identifiers (user IDs or roles).
- **`reason`** (required): Human-readable prompt displayed in the approval UI.
- **`outcome "approved"`**, **`outcome "rejected"`** (both required): Transition targets for approve/reject decisions.

**Orchestrator mode required** (default): Approvals require `--server`. Without `CRITERIA_LOCAL_APPROVAL` set, local-mode runs abort during apply validation before execution starts. See **Local-mode approval and signal wait** below.

---

## Local-mode approval and signal wait

By default, `approval` and `wait { signal }` nodes require an orchestrator (`--server`). Set the env var `CRITERIA_LOCAL_APPROVAL` to one of four values to run them locally without a server.

### Modes

| `CRITERIA_LOCAL_APPROVAL` | Behavior |
|---|---|
| `stdin` | Interactive TTY prompt: prints approvers, reason, and `Approve? (y/n)` to stderr. `y`/`yes` → approved; `n`/`no` → rejected; EOF → rejected with reason "non-interactive input". For signal waits, expects JSON on stdin: `{"outcome":"<name>"}`. |
| `file` | CLI writes the request path to stderr and polls for the operator to write a decision file. Approval format: `{"decision":"approved"}` or `{"decision":"rejected","reason":"..."}`. Signal format: `{"outcome":"<name>"}`. CLI deletes the file after consumption. Default poll interval: 2s; default timeout: 1h. |
| `env` | Reads `CRITERIA_APPROVAL_<NODE>` (uppercase, dots/hyphens → underscores) for approvals (`approved` or `rejected`). Reads `CRITERIA_SIGNAL_<NODE>` for signal waits (outcome name, e.g. `received`). Missing or invalid → fail the run with a clear error. |
| `auto-approve` | Logs a warning and returns `approved` for approvals; synthesizes `outcome="success"` for signal waits. **For unattended CI pipelines only — do not use in production environments.** |

### File paths

- **Request file** (operator writes): `$CRITERIA_STATE_DIR/runs/<run_id>/approval-<node>.json`
- **Decision record** (written after consumption): `$CRITERIA_STATE_DIR/runs/<run_id>/approvals/<node>.json`

### Reattach safety

After a decision is captured, it is persisted to the decision record file. On reattach (e.g., after a CLI crash), the persisted decision is reused without re-prompting the operator.

### File timeout

Override the default 1-hour file-mode timeout:

```sh
CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT=30m criteria apply workflow.hcl
```

### Orchestrator-backed runs

When `--server` is set, `CRITERIA_LOCAL_APPROVAL` is ignored; the orchestrator continues to drive resume.

### Examples

```sh
# Interactive (stdin mode)
CRITERIA_LOCAL_APPROVAL=stdin criteria apply workflow.hcl

# Unattended pipeline (auto-approve)
CRITERIA_LOCAL_APPROVAL=auto-approve criteria apply workflow.hcl

# CI with env-var decision
CRITERIA_LOCAL_APPROVAL=env CRITERIA_APPROVAL_SHIP_TO_PROD=approved criteria apply workflow.hcl

# Operator writes decision out-of-band (file mode)
CRITERIA_LOCAL_APPROVAL=file criteria apply workflow.hcl &
echo '{"decision":"approved"}' > ~/.criteria/runs/<run_id>/approval-ship_to_prod.json
```

---

## Switch

Switch nodes evaluate conditions in order and transition to the first matching
arm, or fall back to the `default` block when one is present. The `branch` block from
earlier releases has been replaced by `switch`; `branch` is now rejected at
parse time.

<!-- validator: skip: switch conditions reference var.env and steps.build which are declared outside this excerpt -->
```hcl
switch "check_env" {
  match {
    condition = var.env == "prod"
    next  = state.deploy_prod
  }
  match {
    condition = var.env == "staging"
    next  = state.deploy_staging
  }
  match {
    condition = steps.build.exit_code == "0"
    next  = state.deploy_dev
  }
  default {
    next = state.skip_deploy
  }
}
```

### Attributes

- **`match`** (zero or more): Conditional arms evaluated in order. First match wins.
  - **`condition`**: Boolean HCL expression. See [Expressions](#expressions).
  - **`next`**: Target node in traversal form (`step.name`, `state.name`, `wait.name`,
    `approval.name`, `switch.name`) or bare keyword `return`.
  - **`output`** (optional): Object expression whose key/value pairs are stored under
    `steps.<switch_name>.*` before the target is entered.
- **`default`** (recommended): Fallback when no condition matches. Omitting `default` is a compile warning unless one condition is provably always true; at runtime, a switch with no matching condition and no default block fails the run.
  - **`next`**: Same form as condition `next`.
  - **`output`** (optional): Same as condition `output`.

### Expression scope

Switch conditions may reference:

- **`var.<name>`**: Workflow variables.
- **`steps.<name>.<output>`**: Outputs from completed steps (e.g., `steps.build.exit_code`).

See [Expressions](#expressions) for syntax rules.

> **Note:** Whether an `if` shorthand will be added is undecided. For now, use a
> two-condition `switch` (one `condition` + `default`) wherever a simple
> true/false fork is needed.

---

## Step-level iteration

Steps iterate over a list, tuple, map, or a fixed count using `for_each`,
`count`, or `parallel` fields. The sequential modifiers (`for_each`/`count`)
run the step body once per item in order; `parallel` runs all items
concurrently. The step acts as its own iteration container — there is no
separate `for_each` block type.

### `for_each` — iterate over a collection

<!-- validator: skip: illustrative fragment; adapter block not included in this excerpt -->
```hcl
step "deploy_services" {
  target   = adapter.shell.default
  for_each = ["api", "web", "worker"]
  input {
    command = "deploy ${each.value} --index ${each._idx}"
  }
  outcome "all_succeeded" { next = step.verify }
  outcome "any_failed"    { next = step.rollback }
}
```

- **`for_each`**: Expression evaluating to a list, tuple, or object/map. For
  maps the iteration order is key-sorted; `each.key` is the map key and
  `each.value` is the value.

### `count` — iterate N times

<!-- validator: skip: illustrative fragment; adapter block not included in this excerpt -->
```hcl
step "batch" {
  target = adapter.noop.default
  count  = 5
  input {
    index = "${each._idx}"
  }
  outcome "all_succeeded" { next = state.done }
}
```

- **`count`**: Expression evaluating to a non-negative integer. Items are the
  integers `0` through `count - 1`.

### `parallel` — run iterations concurrently

`parallel` is a fan-out modifier: the step body runs **concurrently** for all
items in the list, bounded by `parallel_max` goroutines. Results are collected
in declaration order regardless of completion order.

`parallel` is mutually exclusive with `for_each` and `count`.

<!-- validator: fragment -->
```hcl
step "fetch" {
  target       = adapter.noop.default
  parallel     = ["auth", "catalog", "billing"]
  parallel_max = 2   # at most 2 concurrent executions; default = GOMAXPROCS
  on_failure   = "continue"

  input {
    service = each.value
  }

  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = step.handle_errors }
}
```

- **`parallel`**: Expression evaluating to a list or tuple. Each item is bound
  to `each.value` (and `each.index`) within its goroutine. Object/map syntax is
  rejected at compile time; use `for_each` for key/value iteration.
- **`parallel_max`** (optional): Maximum number of goroutines that may execute
  concurrently. Defaults to `GOMAXPROCS`. Must be >= 1; rejected at compile time
  if set to 0 or negative.

**on_failure semantics for `parallel`:**

| Value | Behaviour |
|---|---|
| `""` or `"abort"` (default) | Cancel remaining goroutines on the first failure. Route to `any_failed`. |
| `"continue"` | All goroutines run to completion. Route to `any_failed` if any failed. |
| `"ignore"` | All goroutines run; failures are treated as successes. Always route to `all_succeeded`. |

Note: `on_failure` default for `parallel` is **abort** (cancel outstanding work
immediately on first failure), unlike sequential `for_each`/`count` where the
default is `continue`.

**Adapter concurrency requirements:**

When a step uses `parallel`, its adapter's `Execute` method is called
concurrently from multiple goroutines. Adapter implementations must be
goroutine-safe: avoid shared mutable state, or protect it with a mutex.
Adapters that are safe for concurrent `Execute` calls must declare the
`"parallel_safe"` capability in their `InfoResponse.Capabilities`. The engine
rejects `parallel = [...]` steps that target an adapter lacking this
declaration — at compile time when the adapter binary is resolvable, at runtime
otherwise. See [docs/adapters.md](adapters.md) for details on declaring
capabilities.

Subworkflow steps that use `parallel` receive fully isolated adapter sessions
per iteration — each goroutine's subworkflow opens and closes its own sessions
independently.

**Data values in `parallel` steps:**

When a `parallel` step's per-iteration outcomes declare `write`, the
engine applies them **after all iterations complete**, in declaration order
(index 0, 1, 2, …). Every goroutine reads a **snapshot of data values
taken before any goroutine starts** — there is no live-read between goroutines.

Consequences:

- **Last-index-wins**: when multiple iterations write the same variable, the
  value after the step is the value written by the highest-index iteration that
  reached that outcome.
- **Accumulation is broken**: a pattern that reads `data.internal.counter.value`, increments
  it, and writes it back will not produce `initial + N` — every goroutine reads
  the same snapshot value, so the result is `initial + 1` regardless of N.

For safe parallel accumulation, collect results into indexed outputs and compute
the final value in an aggregate outcome's `output = { ... }` projection:

<!-- validator: fragment -->
```hcl
step "fetch_all" {
  target       = adapter.noop.default
  parallel     = var.items
  parallel_max = 4

  outcome "success" {
    next = continue
    # No write blocks here — collect in aggregate
  }

  # After all goroutines complete, aggregate in the output projection.
  outcome "all_succeeded" {
    next   = "done"
    output = {
      total = length(steps.fetch_all.outputs)
    }
      write {
    target = data.internal.item_count.value
    value  = output.total
  }
  }
}
```

The compiler emits a warning when `write` appears on a `parallel`
step's per-iteration outcome (`next = continue`).

**`each.*` bindings in `parallel`:**

All standard `each.*` bindings are available per goroutine (see table below).
`each._prev` is always `null` in `parallel` mode — there is no defined
"previous" iteration when goroutines run concurrently.

### `while` — condition-driven iteration

`while` causes a step to be re-executed as long as a boolean expression is
true. The condition is evaluated **before each iteration** (pre-condition loop);
if false on the very first evaluation the step body is never invoked and the
aggregate outcome fires immediately.

```hcl
step "poll" {
  target     = adapter.shell.default
  while      = data.internal.queue_empty.value == false
  on_failure = "abort"

  input {
    command = "poll-queue --attempt ${while.index}"
  }

  outcome "success"       { next = continue }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.error }
}
```

`while` is mutually exclusive with `for_each`, `count`, and `parallel`.

#### `while.*` bindings

| Name | Type | Description |
|---|---|---|
| `while.index` | number | Zero-based iteration counter (`0`, `1`, `2`, …). |
| `while.first` | bool | `true` on the first iteration. |
| `while._prev` | object or null | Adapter output object from the previous iteration. `null` on the first iteration. |

`while.*` is only available inside `while`-modified steps. Referencing
`while.*` outside such a step is a compile error.

#### `while` condition expression

The condition is any HCL expression that evaluates to `bool` at runtime.
Typical patterns:

```hcl
while = data.internal.attempts.value > 0          # shared-variable counter
while = while.index < 10             # bounded by iteration index
while = var.flag == true             # static variable (loop runs or skips)
while = true                         # infinite — requires max_visits or max_total_steps
```

If the expression references `while.*` bindings (e.g. `while.index`) the
compiler cannot type-check it statically; type errors are reported at runtime
on the first iteration where the expression produces a non-boolean value.

#### Crash-resume under `while`

The engine persists the iteration cursor — including the current index, failure
status, and `while._prev` — as part of the run's variable scope. On resume the
`while` condition is re-evaluated at the persisted index; the step resumes from
where it left off without re-executing past iterations.

### `each.*` bindings

All `each.*` names are available in `input { }` blocks (and `when` conditions)
within the iterating step and any nested body steps.

| Name | Type | Description |
|---|---|---|
| `each.value` | any | Current item value. For `count`, the integer index. |
| `each.key` | string | For `for_each` over a map: the map key. For lists/count: string representation of the index. |
| `each._idx` | number | Zero-based index of the current iteration (`0`, `1`, …). |
| `each._total` | number | Total number of items. |
| `each._first` | bool | `true` on the first iteration. |
| `each._last` | bool | `true` on the last iteration. |
| `each._prev` | object or null | Output object of the immediately preceding iteration. `null` on the first iteration. For adapter steps, contains the adapter response outputs; for subworkflow-targeted steps, contains the subworkflow return outputs. Persisted across crash-resume. |

> **`each._prev` under failure**: under `on_failure = "continue"`, `each._prev` on iteration N+1
> contains the output object from iteration N **regardless of whether iteration N succeeded or
> failed**. `_prev` is always the previous iteration's outputs, not a success indicator.
> Under `on_failure = "abort"`, the loop terminates on the first failure so `_prev` is never
> re-read after a failure.

Referencing `each.*` outside any iterating step is a compile error.

#### Reduce / scan with `each._prev`

`each._prev` enables accumulation patterns across iterations. Because `_prev` is `null`
on the first iteration, guard with `each._first` or a null check:

<!-- validator: skip: illustrative fragment; adapter block not included in this excerpt -->
```hcl
step "running_total" {
  target   = adapter.shell.default
  for_each = var.amounts
  input {
    accumulator = each._first ? 0 : each._prev.total
    addend      = each.value
  }
  outcome "all_succeeded" { next = step.summarize }
  outcome "any_failed"    { next = state.failed }
}
```

The final iteration's output (`each._prev.total` in the step that follows) holds
the accumulated value.

### Aggregate outcomes

After all iterations complete (or early exit via `on_failure`):

- **`outcome "all_succeeded"`** (required): Fires when no iteration produced a non-success outcome, or when `on_failure = "ignore"`.
- **`outcome "any_failed"`** (optional but recommended): Fires when at least one iteration produced a non-success outcome.

If `any_failed` is absent, failed iterations fall through to `all_succeeded` (compiler emits a warning).

### `on_failure` — failure policy

Controls what happens when an iteration produces a non-success outcome.

| Value | Behaviour |
|---|---|
| `"continue"` (default) | Run all remaining iterations. Route to `any_failed` at the end. |
| `"abort"` | Stop immediately after the first failure. Route to `any_failed`. |
| `"ignore"` | Run all iterations; treat all failures as successes. Always route to `all_succeeded`. |

<!-- validator: skip: illustrative fragment; adapter block not included in this excerpt -->
```hcl
step "deploy" {
  target     = adapter.shell.default
  for_each   = var.targets
  on_failure = "abort"
  input { command = "deploy ${each.value}" }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = step.rollback }
}
```

### Multi-step iteration via subworkflow

For iterations that need multiple steps per item, declare a `subworkflow`
block with the multi-step body and target it from an iterating step.
Each iteration runs the subworkflow to completion; its terminal state
determines success or failure for that item.

<!-- validator: skip: subworkflow source path is illustrative; not present in this repo -->
```hcl
subworkflow "process_one" {
  source = "./subworkflows/process_one"
}

step "process_items" {
  target   = subworkflow.process_one
  for_each = var.items
  input = {
    item = each.value
  }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = step.handle_errors }
}
```

**Rules for subworkflow iteration:**
- The subworkflow must reach a terminal state; non-terminal completion is a runtime error.
- Subworkflow steps inherit `var.*` from the parent scope for keys passed via the parent step's `input = { ... }` map.
- `variable { }` blocks can be declared inside the subworkflow; required variables must be bound via the parent step's `input = { ... }` map.
- Nesting is supported up to a depth of 4 levels.

### Per-iteration outputs

After an iterating subworkflow step completes, downstream steps can access per-iteration outputs through indexed expressions:

- **List / `count`** sources use **numeric** indexes:
  ```
  steps.deploy[0].summary   # first iteration
  steps.deploy[1].summary   # second iteration
  ```

- **Map** (`for_each = { a = "x", b = "y" }`) sources use **string keys**:
  ```
  steps.deploy["a"].summary
  steps.deploy["b"].summary
  ```

- **Non-iterating** steps use the flat form:
  ```
  steps.deploy.summary
  ```

`length(steps.deploy)` returns the total iteration count for list/count sources.

### The `_continue` target

`_continue` is a reserved terminal state name for per-iteration outcomes that signal the engine to advance the cursor to the next item. It is available in iterating steps (both adapter-targeted and subworkflow-targeted) but cannot be used as a transition target in non-iterating steps (compile error). The preferred bare keyword form is `continue`.

### Crash-resume

The engine persists the iteration cursor — including the current index,
failure status, map keys, and `each._prev` — as part of the run variable
scope. On resume, the `for_each`/`count` expression is re-evaluated from the
saved scope (items are not persisted to keep the checkpoint compact). The
`each.*` bindings including `_prev` are fully restored.

---

## Expressions

Expressions appear in `input { }` attribute values, `switch`/`while` conditions,
`for_each`/`count`/`parallel` collections, `output` projections, and `write`
values.

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
- **`steps.<name>.<output>`**: References outputs from completed steps (e.g., `exit_code`, `stdout`). For iterating steps, `steps.<name>[idx].<output>` accesses a specific iteration's outputs.
- **`each.value`**, **`each.key`**, **`each._idx`**, **`each._total`**, **`each._first`**, **`each._last`**, **`each._prev`**: Available within iterating steps and their bodies. See [Step-level iteration](#step-level-iteration) for the full binding table.

### Type rules

- Comparison operators (`==`, `!=`, `<`, `>`, `<=`, `>=`) follow HCL semantics.
- Boolean operators: `&&`, `||`, `!`.
- String concatenation is implicit in interpolated strings.

### Compile-time vs. runtime evaluation

- **Compile-time**: Variable defaults, static list literals.
- **Runtime**: step outputs, data values, and `each.*` / `while.*` scope (evaluated per iteration).

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

#### `templatefile(path, vars)`

Reads the file at `path` (same path confinement and `CRITERIA_FILE_FUNC_MAX_BYTES` size cap as `file()`), then renders it as a Go [`text/template`](https://pkg.go.dev/text/template) template with `vars` as the data context. Returns the rendered string.

```hcl
step "draft" {
  target = adapter.copilot.editor
  input {
    prompt = templatefile("prompts/draft.tmpl", {
      topic   = var.topic
      example = steps.outline.summary
    })
  }
}
```

**Template syntax:** `{{ .fieldName }}`, `{{ range .list }}`, `{{ if .flag }}`, etc. — standard Go `text/template` syntax.

**Constraints:**
- `vars` must be an object or map value. Primitives and lists are rejected with a descriptive error.
- Missing keys in `vars` are a render-time error (`missingkey=error` is set). Use `{{ if .key }}{{ .key }}{{ end }}` for optional keys.
- Null values in `vars` become `nil` in the template context and render as `<no value>` (Go `text/template`'s default for nil map entries).
- Numbers prefer integer rendering when exactly representable; `42.0` and `42` both render as `42`. Use `{{ printf "%.1f" .x }}` for explicit decimal output.
- Same path confinement and size-cap rules as [`file()`](#filePath).

> **Differences from Terraform's `templatefile`:** Terraform's `templatefile` uses HCL native template syntax (`${field}`). Criteria's uses Go `text/template` syntax (`{{ .field }}`). This is intentional — `text/template` is in the Go stdlib and does not auto-escape output, which is desirable for LLM prompt content.

#### `fileset(path, pattern)`

Lists regular files inside `path` (resolved relative to the workflow directory) whose basenames match the glob `pattern`. Returns a sorted `list(string)` of paths **relative to the workflow directory**, suitable for passing directly to `file()` or `templatefile()` via `each.value`.

```hcl
step "process_prompts" {
  for_each = fileset("prompts", "*.md")
  target   = adapter.copilot.editor
  input {
    prompt = file(each.value)
  }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}
```

**Pattern syntax:** Go [`filepath.Match`](https://pkg.go.dev/path/filepath#Match) — `*` matches any sequence of non-slash characters, `?` matches a single non-slash character, `[a-z]` matches a character class. There is no `**` recursive syntax in v1.

**Constraints:**
- `path` must be relative and must not escape the workflow directory (same confinement rules as `file()`).
- Returns an empty list when no files match. This is intentionally different from Terraform, which also returns an empty list for a missing directory; Criteria errors on a missing directory so failures are loud.
- Symlinks inside `path` are **excluded** (v1 behavior: only `entry.Type().IsRegular()` entries are returned). Users who need symlink following can open a follow-up workstream.
- Subdirectories are not recursed — `fileset` is shallow-only in v1.
- Sort order is lexicographic (e.g. `a1, a10, a2`). For natural sort, post-process the result.

**Example — enumerate and read all prompts in a directory:**

```hcl
step "run_prompts" {
  for_each = fileset("prompts", "*.md")
  target   = adapter.copilot.editor
  input {
    prompt = trimfrontmatter(file(each.value))
  }
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed"    { next = state.failed }
}
```

`each.value` is a path relative to the workflow directory, so it can be passed directly to `file()` without further manipulation.

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

**Environment variables:**

| Variable | Effect |
|---|---|
| `CRITERIA_FILE_FUNC_MAX_BYTES` | Integer; maximum bytes `file()` and `templatefile()` will read. Default 1 MiB. Clamped to [1024, 67108864]. |
| `CRITERIA_WORKFLOW_ALLOWED_PATHS` | Colon-separated list of directories `file()`, `fileexists()`, `fileset()`, and `templatefile()` may access outside the workflow directory. |

#### Hash functions

The following functions compute hex-encoded digests of a UTF-8 string. All four mirror the Terraform equivalents exactly, so existing muscle memory transfers.

> **File hashing:** To hash a file's contents, compose with `file()`: `sha256(file("./data.bin"))`.

##### `sha256(value)`

Returns the hex-encoded SHA-256 digest of `value`.

```hcl
local "fingerprint" {
  value = sha256(var.input)  # e.g. "ba7816bf…"
}
```

##### `sha1(value)`

Returns the hex-encoded SHA-1 digest of `value`.

> ⚠️ **Security notice:** SHA-1 is cryptographically broken. Use only for non-security purposes such as cache keys or content identity. Never use for passwords, signatures, or integrity checks in a security context.

```hcl
local "cache_key" {
  value = sha1(var.content)
}
```

##### `sha512(value)`

Returns the hex-encoded SHA-512 digest of `value` (128 hex characters).

```hcl
local "strong_hash" {
  value = sha512(var.secret)
}
```

##### `md5(value)`

Returns the hex-encoded MD5 digest of `value`.

> ⚠️ **Security notice:** MD5 is cryptographically broken. Use only for non-security purposes such as cache keys or content identity. Never use for passwords, signatures, or integrity checks in a security context.

```hcl
local "etag" {
  value = md5(var.body)
}
```

#### Encoding functions

##### `base64encode(value)`

Returns the standard Base64 encoding (RFC 4648) of `value`.

```hcl
local "encoded" {
  value = base64encode("hello world")  # "aGVsbG8gd29ybGQ="
}
```

##### `base64decode(value)`

Decodes a standard Base64-encoded string. Errors if `value` is not valid Base64.

```hcl
local "decoded" {
  value = base64decode("aGVsbG8gd29ybGQ=")  # "hello world"
}
```

##### `jsonencode(value)`

JSON-encodes any value (string, number, bool, list, object, or null) and returns the JSON string.

```hcl
local "envelope" {
  value = jsonencode({ payload = var.input, ts = timestamp() })
}
```

##### `jsondecode(value)`

Decodes a JSON string and returns the appropriate cty value (string, number, bool, list, or object). The return type depends on the JSON content; use attribute access or list indexing downstream.

> **Type stability:** Because the return type is inferred from the JSON content at call time, consumers who need type-stable access should use attribute access (`jsondecode(s).key`) rather than relying on the whole value being a specific type.

```hcl
local "parsed" {
  value = jsondecode(steps.fetch.body)
}
```

##### `urlencode(value)`

URL-encodes `value` using query-component encoding (`net/url.QueryEscape`). Spaces are encoded as `+`; special characters are percent-encoded.

> **Note:** This matches Terraform's `urlencode` semantics (spaces → `+`). If you need path encoding (spaces → `%20`), post-process with a template expression.

```hcl
local "query" {
  value = urlencode("hello world")  # "hello+world"
}
```

##### `yamlencode(value)`

YAML-encodes any value (string, number, bool, list, or object) and returns the YAML string. The JSON → Go → YAML round-trip preserves structure but not YAML-specific types (e.g. timestamps become strings; comments are not supported).

```hcl
local "manifest" {
  value = yamlencode({ name = "my-workflow", version = "1" })
}
```

##### `yamldecode(value)`

Decodes a YAML string and returns the appropriate cty value. Uses a JSON → cty round-trip internally, so YAML-specific types (timestamps, comments, anchors) are normalized to strings or standard JSON types.

```hcl
local "config" {
  value = yamldecode(file("./config.yaml"))
}
```

#### Dynamic functions

> ⚠️ **Non-determinism and crash-resume:** Both functions below produce a new value on every call. If your workflow may crash and resume, capture the result into a step output and reference `steps.<name>.<key>` downstream so that the same value is used across resumes rather than generating a new one on re-evaluation.

##### `uuid()`

Returns a randomly-generated RFC 4122 v4 UUID string (36 characters, format `xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx`). **Non-deterministic** — each call returns a unique value.

```hcl
local "run_id" {
  value = uuid()  # e.g. "f47ac10b-58cc-4372-a567-0e02b2c3d479"
}
```

##### `timestamp()`

Returns the current UTC time in RFC 3339 format (e.g. `"2024-01-15T12:34:56Z"`). **Non-deterministic** — successive calls return different values.

```hcl
local "started_at" {
  value = timestamp()  # e.g. "2024-01-15T12:34:56Z"
}
```

---

## Permissions

Criteria enforces a deny-by-default permission model for tool invocations (adapter-backed steps; future: all adapter tool use).

### Workflow-level permissions

<!-- validator: skip: workflow-level permissions example references states not defined in excerpt -->
```hcl
workflow {
  name          = "secure_build"
  version       = "1"
  initial_state = "build"
  target_state  = "done"
}

permissions {
  allow_tools = ["shell:git*", "shell:make*"]
}
```

Applies to all adapter steps unless overridden.

### Step-level permissions

<!-- validator: skip: step targets adapter.copilot.assistant which is declared outside this excerpt -->
```hcl
step "build" {
  target      = adapter.copilot.assistant
  allow_tools = ["shell:go*build*"]
  input { prompt = "Run go build" }
  outcome "success" { next = state.done }
}
```

The effective allowlist is the union of workflow-level and step-level patterns.

### Pattern matching

Tool names are matched against glob patterns using `filepath.Match` semantics:

- `shell:git*` permits `shell:git status`, `shell:git commit`, etc.
- `shell:*` permits all shell commands.
- `*` permits all tools (use with caution).

See [adapters.md](adapters.md) for the tool invocation wire protocol.

---

## Standalone CLI

A workflow path may be a single `.hcl`/`.chcl` file or a directory module. Run
`criteria <command> --help` for the full flag set.

| Command | Purpose |
|---|---|
| `criteria validate <wf>` | Parse and type-check without executing (`--diag-json` for structured output). |
| `criteria compile <wf>` | Emit the FSM graph (`--format json` default, or `--format dot`; `--out <path>`). |
| `criteria plan <wf>` | Human-readable execution preview. |
| `criteria apply <wf>` | Execute the workflow. |
| `criteria spec` | Print the language specification (`--with-patterns` appends the LLM prompt pack). |
| `criteria adapter …` | Manage adapters: `lock`, `pull`, `publish`, `list`, `info`, `where`, `remove`, `prune`, `dev`. |
| `criteria pause` / `resume` / `inspect` / `status` / `stop` | Run-lifecycle and introspection (server-oriented). |
| `criteria langserver` | LSP server over stdin/stdout (experimental). |

Variable overrides (on `plan` and `apply`):

- **`--var key=value`** (repeatable): Override a single variable.
- **`--var-file <path>`** (repeatable): Load overrides from a `.chcl`, `.hcl`, or `.json` file. Multiple files merge left-to-right; later files win. `--var` takes precedence over any `--var-file` entry.

### `criteria compile`

```bash
bin/criteria compile examples/tour/tour.hcl
bin/criteria compile examples/tour/tour.hcl --format dot --out workflow.dot
```

- **JSON** (default): FSM graph with nodes, outcomes, and metadata.
- **DOT**: Graphviz-compatible directed graph for visualization.

### `criteria apply`

Execute the workflow.

```bash
# Local (no server): streams ND-JSON events to stdout.
bin/criteria apply examples/build_and_test/build_and_test.hcl

# Server mode: persists run state, supports resume and approvals.
bin/criteria apply <workflow.hcl> --server http://localhost:8080
```

Notable flags: `--server <url>`, `--server-tls disable|tls|mtls`,
`--events-file <path>` (write events to a file instead of stdout),
`--output auto|concise|json`, `--name <id>` (server-mode agent name),
`--subworkflow-root <path>`.

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
- `ForEachEntered`, `StepIterationStarted`, `StepIterationCompleted`, `StepIterationItem`
- `WaitEntered`, `WaitResumed`
- `ApprovalRequested`, `ApprovalDecided`
- `BranchEvaluated`

See [`proto/criteria/v1/`](../proto/criteria/v1/) for proto definitions and event schemas.

### Local-mode constraints

- Duration-based waits work.
- Signal-based waits and approval nodes require `CRITERIA_LOCAL_APPROVAL` (see **Local-mode approval and signal wait**) or `--server`.
- Local runs write step checkpoints and persisted approval/signal decisions under `$CRITERIA_STATE_DIR` (default `~/.criteria`) so a restarted run can reuse captured decisions without re-prompting. For full crash recovery and distributed persistence, use `--server`.

For examples demonstrating each command, see:
- Linear shell pipeline: [examples/build_and_test/build_and_test.hcl](../examples/build_and_test/build_and_test.hcl)
- Feature tour: [examples/tour/tour.hcl](../examples/tour/tour.hcl)

---

## Doc-example validation

The `make validate-docs` gate ([`tools/validate-docs.sh`](../tools/validate-docs.sh))
extracts every full-workflow ` ```hcl ` block (one containing a `workflow { }`
header) from [LANGUAGE-SPEC.md](LANGUAGE-SPEC.md) and runs `criteria validate` on
each, stubbing any referenced subworkflow directories. Keep the worked examples
in that file compiling.

Snippets in this document are mostly illustrative fragments (a step, adapter, or
node in isolation) and are not individually compiled; the
`<!-- validator: ... -->` comments preceding some blocks are authoring hints, not
an enforced gate.


## Data Values

`data "internal" "<name>"` blocks declare workflow-scoped mutable state that
steps can read from eval expressions and write via the `write` outcome
block. The engine manages locking — step code never sees a partial write.

### Declaring a data value

```hcl
data "internal" "counter" {
  type  = number
  value = 0
}

data "internal" "status_msg" {
  type  = string
  value = "pending"
}
```

`type` accepts the same type surface as `variable` declarations: `string`,
`number`, `bool`, `list(string)`, `list(number)`, `list(bool)`, and
`map(string)`.

`value` sets the initial value; it must be a literal (no expression references).
If `value` is omitted the variable starts as a **typed `null`** for its declared
type. Reading `data.internal.<name>.value` before any `write` has applied will yield
`null`; expressions that require a concrete value (e.g. arithmetic on a `null
number`) will produce a runtime error. Provide an explicit `value` if you need a
non-null default.

### Reading a data value

Inside any HCL expression (step input, condition, output projection) use the
`data.internal.<name>.value` namespace:

```hcl
step "notify" {
  target = adapter.noop.default
  input {
    message = "counter is ${data.internal.counter.value}"
  }
  outcome "done" { next = state.done }
}
```

The snapshot is captured **once per step entry** so all expressions within a
step see a consistent point-in-time view, even if another concurrent step
updates the variable during execution.

### Writing a data value (write blocks)

Use `write` on an outcome block to write one or more data values when
that outcome is reached. In the simplest form the value is an output
key from the step's adapter output:

```hcl
step "count_lines" {
  target = adapter.noop.default
  outcome "done" {
    next = state.done
    write {
      target = data.internal.counter.value
      value  = output.line_count
    }
  }
}
```

`counter` is a declared `data "internal"`; `"line_count"` is the key in the
adapter's output map. All writes on one outcome are committed
atomically — partial writes are never observable.

When an `output = { ... }` projection is also declared on the outcome, the
engine validates at **compile time** that every `output.<key>` reference in a
`write` value appears in the projection. When no projection is present but the
adapter declares an output schema, the compiler validates against that schema
instead. If neither is available the check is deferred to runtime.

#### The write `value` expression has the full outcome context

A `write` value is not limited to `output.<key>`. It is an arbitrary HCL
expression evaluated against the **same context as the outcome's
`output = { ... }` projection**, so it may reference:

- `var.*`, `local.*` — workflow inputs and locals
- `data.<kind>.<name>.value` — other data values (see snapshot semantics below)
- `step.output.<key>` — the current step's raw adapter outputs (strings)
- `output.<key>` — keys from this outcome's `output = { ... }` projection
- `subworkflow.<key>` — return values of a subworkflow step
- the standard functions

This means you can compute a write inline without first routing everything
through a projection:

```hcl
outcome "success" {
  next = state.done
  write {
    target = data.internal.counter.value
    value  = data.internal.counter.value + var.bump + step.output.delta
  }
}
```

This is the recommended way to update a data value after a `for_each`, `while`,
or `parallel` step: read the accumulated state in the write value directly,
rather than relying on a fan-in step (which today often lands on a `noop`
adapter that produces no output to project).

#### Snapshot semantics — reads see the step-entry snapshot

> **Important.** When a write `value` reads `data.<kind>.<name>.value`, it
> always observes the value as of **step entry** — the point-in-time snapshot
> captured before the step ran — *not* any value written earlier in the same
> step.

The `data.*` namespace is refreshed once per step entry. Writes produced by a
step are resolved against that snapshot and then committed together (atomically)
*after* the step finishes. Consequently, within a single step:

- A write that reads the data value it is updating sees the **previous** value,
  not its own pending write — so `value = data.internal.counter.value + 1`
  increments relative to the step-entry value.
- When one write reads a data value that **another write in the same step** is
  also updating, the read still sees the step-entry snapshot, never the sibling
  write's new value. The full write set is atomic against the snapshot, so write
  order within the step never affects the result.

This makes writes deterministic and order-independent. The compiler emits an
informational warning when a write reads a data value the same step also writes,
so the behavior is easy to spot while troubleshooting; it is not an error.

For ordering that *must* be observed (each write seeing the prior write's
result), split the writes across separate steps, since the snapshot is
refreshed on each step entry.

### Type enforcement

The declared `type` is enforced on every write. If the adapter output value
cannot be coerced to the declared type the step fails with a clear error message
and the workflow is aborted. No partial write occurs.

There are two write paths, with different type capabilities:

**Typed output projection** (`output = { ... }` declared on the outcome): the
projection is evaluated as an HCL expression, producing a fully-typed cty value.
All declared `data "internal"` types are supported — including `list(string)`,
`list(number)`, `list(bool)`, and `map(string)`. Use this path for non-scalar
accumulation:

```hcl
outcome "success" {
  next   = state.done
  output = { tag_list = [step.output.tag1, step.output.tag2] }
  write {
    target = data.internal.tags.value
    value  = output.tag_list
  }
}
```

Here `step.output.<key>` exposes the raw adapter output strings for the current step. Each value is a `string`, so `[step.output.tag1, step.output.tag2]` constructs a `list(string)` that the engine converts to the declared type of the data value.

**Raw adapter string coercion** (no `output = { ... }` projection, or the key is
absent from the projection): the engine coerces the adapter's raw string output
to the declared type. Only scalar types are supported this way. For `"number"`
variables, the string must be a valid numeric literal with no trailing
non-numeric characters (`"42"` and `"3.14"` are accepted; `"7abc"` and `"1e2x"`
are rejected). For `"bool"` variables, accepted values are `"true"`, `"false"`,
`"1"`, and `"0"`. Declaring a non-scalar data value and writing to it via
raw coercion is a runtime error; use an output projection instead.

### Isolation across subworkflow bodies

Each subworkflow body gets its own isolated shared-variable store. Writes
inside a subworkflow body do not propagate to the parent, and parent writes
made after the subworkflow starts are not visible inside it. This prevents
accidental coupling between a subworkflow and its caller.

---

## Subworkflows

The `subworkflow "<name>"` block declares a reusable workflow fragment to be resolved from a local directory and deep-compiled into the parent workflow's FSM graph at compile time.

### Declaring a subworkflow

<!-- validator: skip: subworkflow source path ./subworkflows/smoke is illustrative; not present in this repo -->
```hcl
workflow {
  name          = "deploy_pipeline"
  version       = "1"
  initial_state = "lint"
  target_state  = "done"
}

variable "env" {
  type    = string
  default = "staging"
}

adapter "shell" "default" {
  config { }
}

subworkflow "smoke_test" {
  source      = "./subworkflows/smoke"
  environment = shell.ci
  input = {
    target_env = var.env
    retries    = 3
  }
}

step "lint" {
  target = adapter.shell.default
  input {
    command = "run-lint"
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
```

### Sub-workflow directory layout

Each `source` path must point to a **directory** (not a file) containing at least one `.hcl` file:

```
./subworkflows/smoke/
  main.hcl        # workflow block, states, steps, outputs
  variables.hcl   # optional: additional declarations (no workflow header)
```

Multiple `.hcl` files in the directory are merged at compile time. Only one file (typically `main.hcl`) carries the `workflow { name = "..." }` header block; all other files contain declaration blocks only. Declaration lists (states, steps, variables, outputs) from all files are combined; the `version`, `initial_state`, and `target_state` are taken from the file that carries the workflow header. Duplicate name declarations across files produce a compile error.

### Input binding

The `input = { ... }` map binds parent-scope expressions to the callee's `variable` blocks:

- Every callee variable without a `default` value **must** have a corresponding input key.
- Extra input keys that don't match any callee variable produce a compile error.
- Input values are parent-scope HCL expressions; `var.*`, `local.*`, and literal values are all valid.

### Output access

A subworkflow step's return values are exposed through the `subworkflow.<key>`
namespace, available **only** in that step's own outcome `output = { ... }`
projection and `write` expressions. Project them to make them visible downstream
as `steps.<step>.*`:

<!-- validator: skip: illustrative excerpt; subworkflow source and states omitted -->
```hcl
step "run_smoke" {
  target = subworkflow.smoke_test
  outcome "success" {
    next   = step.report
    output = { status = subworkflow.status }   # project the callee's return value
  }
}

step "report" {
  target = adapter.shell.default
  input {
    result = steps.run_smoke.status            # read the projected output
  }
  outcome "success" { next = state.done }
}
```

### Compilation semantics

1. **Deep-compile**: The parent workflow's `compileSubworkflows` pass resolves each `source`, reads all `.hcl` files in the directory, merges them, and recursively compiles the callee into a child `FSMGraph`. All validation (type errors, undeclared adapters, missing variables) happens at compile time before any step executes.
2. **Cycle detection**: If a subworkflow's source path already appears in the current compilation chain, a compile error is produced listing the full cycle path.
3. **Scope isolation**: The callee declares its own adapters; sessions are isolated and torn down when the callee reaches a terminal state.

### CLI flags

- `--subworkflow-root <path>` (repeatable): Restrict subworkflow source resolution to paths under this root. By default (empty), any local path is allowed. Use this in CI pipelines to prevent workflows from loading subworkflows from outside a trusted directory tree.

  ```sh
  criteria apply --subworkflow-root ./workflows my_workflow.hcl
  ```

  The flag is also supported by `criteria validate` and `criteria compile`.

### Source schemes

Only local filesystem paths (`./relative/path` or `/absolute/path`) are
supported. Remote schemes (`git://`, `https://`, `url://`) are **not supported**.

---

### Repository layout

- **`github.com/brokenbots/criteria`** — workflow engine, compiler, and standalone CLI (this document); the in-tree `cmd/criteria-adapter-mcp` adapter lives here too.
- **`github.com/brokenbots/criteria/sdk`** — published Go SDK; the server transport contract and event schemas live under `sdk/pb/criteria/v1`.

The orchestrator is developed separately at [github.com/brokenbots/orchestrator](https://github.com/brokenbots/orchestrator) and consumes the published SDK.

