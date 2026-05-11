# Criteria Workflow Language — Specification (v0.3)

## Purpose & Audience

This document is the normative reference for the Criteria HCL workflow language, targeting language models and tools that generate or validate workflow files. It is a complete, dense specification: every block type, every attribute, every expression function, every namespace binding, and every outcome rule is listed here. Human-readable prose context lives in [docs/workflow.md](workflow.md).

## File structure

A workflow module is either:

1. **Single-file:** one `.hcl` file containing all declarations.
2. **Directory module:** a directory of `.hcl` files; exactly one must contain a `workflow` header block. All files are merged before compilation.

File names are arbitrary; the `.hcl` extension is required. A module must contain exactly one `workflow` block across all files; zero or more than one is a compile error.

Encoding: UTF-8. Max file size: implementation-defined (default 64 MiB for file() reads; no hard limit on source files).

## Grammar (EBNF-ish)

```ebnf
workflow_module  := content_decl*
content_decl     := workflow_block | variable_block | local_block | shared_var_block
                  | environment_block | output_block | adapter_block | subworkflow_block
                  | step_block | state_block | wait_block | approval_block
                  | switch_block | policy_block | permissions_block

workflow_block   := "workflow" STRING "{" workflow_attr* "}"
workflow_attr    := "version" "=" STRING
                  | "initial_state" "=" STRING
                  | "target_state" "=" STRING
                  | "environment" "=" STRING

variable_block   := "variable" STRING "{" variable_attr* "}"
local_block      := "local" STRING "{" local_attr* "}"
shared_var_block := "shared_variable" STRING "{" shared_var_attr* "}"
environment_block:= "environment" STRING STRING "{" "}"
output_block     := "output" STRING "{" output_attr* "}"
adapter_block    := "adapter" STRING STRING "{" adapter_attr* config_block? "}"
subworkflow_block:= "subworkflow" STRING "{" subworkflow_attr* "}"
step_block       := "step" STRING "{" step_attr* input_block? outcome_block* "}"
state_block      := "state" STRING "{" state_attr* "}"
wait_block       := "wait" STRING "{" wait_attr* outcome_block* "}"
approval_block   := "approval" STRING "{" approval_attr* outcome_block* "}"
switch_block     := "switch" STRING "{" condition_block* default_block? "}"
policy_block     := "policy" "{" policy_attr* "}"
permissions_block:= "permissions" "{" permissions_attr* "}"

outcome_block    := "outcome" STRING "{" "next" "=" STRING "}"
input_block      := "input" "{" (STRING "=" expr)* "}"
config_block     := "config" "{" (STRING "=" expr)* "}"
condition_block  := "condition" "{" "match" "=" expr "next" "=" STRING "}"
default_block    := "default" "{" "next" "=" STRING "}"

expr             := STRING | NUMBER | BOOL | hcl_template | traversal
                  | func_call | binary_op | unary_op | tuple | object
```

Rules:
- All block keywords are lowercase.
- STRING values are double-quoted HCL string literals; template interpolation (`${...}`) is supported in most attribute values.
- Block labels (the quoted strings after the keyword) are identifiers for cross-referencing; they must be unique within their block kind.
- Attributes not marked optional are required unless stated otherwise; omission is a compile error.

## Blocks

The following block types are defined. Tables are auto-generated from [`workflow/schema.go`](../workflow/schema.go).

<!-- BEGIN GENERATED:blocks -->
### `workflow "name" { ... }`

- **Source:** [`workflow/schema.go:81`](../workflow/schema.go#L81)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `version` | string | no | _(no description)_ |
| `initial_state` | string | no | _(no description)_ |
| `target_state` | string | no | _(no description)_ |
| `environment` | string | no | _(no description)_ |


### `variable "name" { ... }`

- **Source:** [`workflow/schema.go:116`](../workflow/schema.go#L116)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `type` | string | no | _(no description)_ |
| `description` | string | no | _(no description)_ |


### `local "name" { ... }`

- **Source:** [`workflow/schema.go:14`](../workflow/schema.go#L14)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `description` | string | no | _(no description)_ |


### `shared_variable "name" { ... }`

- **Source:** [`workflow/schema.go:27`](../workflow/schema.go#L27)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `description` | string | no | _(no description)_ |
| `type` | string | no | _(no description)_ |


### `environment "type" "name" { ... }`

- **Source:** [`workflow/schema.go:54`](../workflow/schema.go#L54)
- **Labels:** `type` `name`

### `output "name" { ... }`

- **Source:** [`workflow/schema.go:234`](../workflow/schema.go#L234)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `description` | string | no | _(no description)_ |
| `type` | string | no | _(no description)_ |


### `adapter "type" "name" { ... }`

- **Source:** [`workflow/schema.go:141`](../workflow/schema.go#L141)
- **Labels:** `type` `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `environment` | string | no | _(no description)_ |
| `on_crash` | string | no | _(no description)_ |

- **Nested blocks:** [`config`](#config---)

### `subworkflow "name" { ... }`

- **Source:** [`workflow/schema.go:244`](../workflow/schema.go#L244)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `source` | string | yes | _(no description)_ |
| `environment` | string | no | _(no description)_ |


### `step "name" { ... }`

- **Source:** [`workflow/schema.go:150`](../workflow/schema.go#L150)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `on_crash` | string | no | _(no description)_ |
| `on_failure` | string | no | OnFailure controls iteration failure behaviour: "continue" (default for sequential for_each/count steps), "abort" (stop on first failure; default for parallel steps), or "ignore" (treat all as success). |
| `max_visits` | number | no | MaxVisits limits how many times this step may be evaluated in a single run. 0 (default) means unlimited. Negative values are rejected at compile time. |
| `config` | map(string) | no | Config is the legacy map attribute; retained for parse-time detection so the compiler can emit a helpful "use input { } block" diagnostic. |
| `timeout` | string | no | _(no description)_ |
| `allow_tools` | list(string) | no | _(no description)_ |
| `default_outcome` | string | no | DefaultOutcome, when set, is the fallback outcome name used when an adapter returns an outcome name not in the declared set. Must refer to a declared outcome; validated at compile time. |

- **Nested blocks:** [`input`](#input---), [`outcome`](#outcome-name---)

### `state "name" { ... }`

- **Source:** [`workflow/schema.go:307`](../workflow/schema.go#L307)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `terminal` | bool | no | _(no description)_ |
| `success` | bool | no | _(no description)_ |
| `requires` | string | no | _(no description)_ |


### `wait "name" { ... }`

- **Source:** [`workflow/schema.go:290`](../workflow/schema.go#L290)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `duration` | string | no | _(no description)_ |
| `signal` | string | no | _(no description)_ |

- **Nested blocks:** [`outcome`](#outcome-name---)

### `approval "name" { ... }`

- **Source:** [`workflow/schema.go:299`](../workflow/schema.go#L299)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `approvers` | list(string) | yes | _(no description)_ |
| `reason` | string | yes | _(no description)_ |

- **Nested blocks:** [`outcome`](#outcome-name---)

### `switch "name" { ... }`

- **Source:** [`workflow/schema.go:318`](../workflow/schema.go#L318)
- **Labels:** `name`
- **Nested blocks:** [`condition`](#condition---), [`default`](#default---)

### `policy { ... }`

- **Source:** [`workflow/schema.go:338`](../workflow/schema.go#L338)
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `max_total_steps` | number | no | _(no description)_ |
| `max_step_retries` | number | no | _(no description)_ |
| `max_visits_warn_threshold` | number | no | MaxVisitsWarnThreshold controls when the engine emits a warning for excessive revisits while executing a workflow. |


### `permissions { ... }`

- **Source:** [`workflow/schema.go:357`](../workflow/schema.go#L357)
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `allow_tools` | list(string) | no | AllowTools is the workflow-wide list of glob patterns for permitted tool invocations. Step-level allow_tools is unioned with this list. See StepSpec.AllowTools for matching semantics. |


### `config { ... }`

- **Source:** [`workflow/schema.go:126`](../workflow/schema.go#L126)

### `input { ... }`

- **Source:** [`workflow/schema.go:134`](../workflow/schema.go#L134)

### `outcome "name" { ... }`

- **Source:** [`workflow/schema.go:283`](../workflow/schema.go#L283)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `next` | string | yes | _(no description)_ |


### `condition { ... }`

- **Source:** [`workflow/schema.go:327`](../workflow/schema.go#L327)

### `default { ... }`

- **Source:** [`workflow/schema.go:333`](../workflow/schema.go#L333)
<!-- END GENERATED:blocks -->

### Notes on specific blocks

**`workflow`** — Exactly one per module. `version` must be `"1"`. `initial_state` names the starting state; defaults to the first declared state if absent. `target_state` names the expected terminal success state used by `make validate`.

**`variable`** — Compile-time typed inputs. Type must be one of `string`, `bool`, `number`, `list(string)`, or `map(string)`. A `default` expression may follow the declared attributes; absence makes the variable required.

**`local`** — Compile-time constant. Evaluate a single `value` expression; the result is frozen for the run. No side effects.

**`shared_variable`** — Runtime-mutable, workflow-scoped value. `type` declares the cty type; `value` is the optional initial expression. Reads via `shared.<name>`; writes via `shared_writes` in outcome blocks.

**`environment`** — Declares an execution environment. First label is type (e.g. `shell`), second is name. Attributes are free-form and type-specific; no fixed schema beyond the two labels.

**`output`** — Declares a named output value surfaced at run completion. `value` expression is evaluated at termination time.

**`adapter`** — Declares a long-lived adapter session. `type`/`name` labels route steps; `config` sub-block provides adapter-specific configuration as string key-value pairs. `on_crash` controls crash semantics: `abort` (default) or `ignore`.

**`subworkflow`** — Declares a reusable sub-workflow. `source` is a local directory path. Invoked via a step with `target = subworkflow.<name>`.

**`step`** — The primary execution node. `target` (captured via remain) references the adapter or subworkflow to invoke: `adapter.<type>.<name>` or `subworkflow.<name>`. `input` sub-block provides per-invocation key-value inputs. `outcome` sub-blocks map adapter return values to next nodes. `for_each` / `count` (captured via remain) enable iteration.

**`state`** — A named non-executing node. `terminal = true` marks a terminal state. `success = true/false` marks the run outcome. `requires` names a prerequisite state that must be visited first.

**`wait`** — Pauses execution. `duration` is an HCL duration string (e.g. `"5m"`); `signal` names an external signal (requires server mode). Exactly one of `duration` or `signal` must be set.

**`approval`** — Requires human approval (server mode only). `approvers` is a list of identity strings; `reason` is a human-readable prompt.

**`switch`** — Conditional routing. `condition` sub-blocks are evaluated in declaration order; the first truthy `match` expression wins. `default` is the fallback; absence without an exhaustive condition set produces a runtime error.

**`policy`** — Global execution guards. Zero or one per module. Attributes set hard limits on step execution counts.

**`permissions`** — Workflow-level tool allowlist. `allow_tools` is a list of glob patterns unioned with any step-level `allow_tools`.

## Expressions

### Namespace bindings

<!-- BEGIN GENERATED:namespaces -->
| Namespace | Available in | Description |
|---|---|---|
| `var.*` | all expressions | Read-only typed input variables declared with `variable` blocks. |
| `steps.<name>.<key>` | post-completion of `<name>` | Captured outputs from a prior step. |
| `each.value` / `each.key` / `each._idx` / `each._total` / `each._first` / `each._last` / `each._prev` | iterating-step expressions only | Per-iteration bindings; see Iteration semantics. |
| `local.*` | all expressions | Compile-time constants declared with `local` blocks. |
| `shared.*` | all expressions; mutable via `shared_writes` | Runtime-mutable shared values declared with `shared_variable` blocks. |
<!-- END GENERATED:namespaces -->

### Operator precedence (HCL)

Standard HCL operator precedence applies (highest to lowest):

1. Unary: `!`, `-`
2. Multiplicative: `*`, `/`, `%`
3. Additive: `+`, `-`
4. Comparison: `==`, `!=`, `<`, `<=`, `>`, `>=`
5. Logical AND: `&&`
6. Logical OR: `||`
7. Conditional: `condition ? true_val : false_val`

### Template interpolation

String attributes support HCL template interpolation: `"prefix ${expression} suffix"`. The `%{if cond}...%{endif}` and `%{for item in list}...%{endfor}` directives are available in template strings.

### Type coercion

HCL performs implicit type coercion in string templates (any value → string) and explicit coercion via built-in HCL functions. Coercion failures are compile-time errors when the expression is a literal; runtime errors otherwise.

## Functions

Expression functions available in all HCL attribute values within a workflow. Functions are registered per-evaluation by [`workflow/eval_functions.go`](../workflow/eval_functions.go).

<!-- BEGIN GENERATED:functions -->
| Function | Signature | Returns | Source | Description |
|---|---|---|---|---|
| `file` | `file(path: string)` | `string` | [workflow/eval_functions.go:109](../workflow/eval_functions.go#L109) | the file(path) → string expression function. |
| `fileexists` | `fileexists(path: string)` | `bool` | [workflow/eval_functions.go:151](../workflow/eval_functions.go#L151) | the fileexists(path) → bool expression function. |
| `trimfrontmatter` | `trimfrontmatter(content: string)` | `string` | [workflow/eval_functions.go:224](../workflow/eval_functions.go#L224) | the trimfrontmatter(content) → string expression function. |
<!-- END GENERATED:functions -->

### Function notes

**`file(path)`** — Path is resolved relative to the workflow directory. Paths outside the workflow directory (and any configured `CRITERIA_WORKFLOW_ALLOWED_PATHS`) are rejected with a security error. Size cap: 1 MiB by default; override with `CRITERIA_FILE_FUNC_MAX_BYTES`. Content must be valid UTF-8.

**`fileexists(path)`** — Same path-confinement rules as `file()`. Returns `false` for directories; propagates non-existence errors as `false`.

**`trimfrontmatter(content)`** — Strips YAML front matter (content between leading `---\n` delimiters) from a string. No-op when no front matter is present. Useful for processing Markdown files read via `file()`.

## Iteration semantics

Steps support two iteration forms, specified via attributes captured in the step's `remain` body:

1. **`for_each`** — Iterates over a list or map expression. One adapter call per element.
2. **`count`** — Iterates a fixed number of times. `count = N` produces iterations `0` through `N-1`.

**`each.*` bindings (available only inside iterating steps):**

| Binding | Type | Description |
|---|---|---|
| `each.value` | any | Current element value (list element or map value). |
| `each.key` | string | Current element key (list index as string, or map key). |
| `each._idx` | number | Zero-based iteration index. |
| `each._first` | bool | True on the first iteration. |
| `each._last` | bool | True on the last iteration. |
| `each._total` | number | Total number of iterations. |
| `each._prev` | any | Output of the previous iteration (nil on first). |

**Parallelism:** Set `parallel = true` (remain attribute) on a step to run all iterations concurrently. Default is sequential.

**`on_failure` semantics:**

- `"continue"` (default for sequential) — record failure, continue remaining iterations.
- `"abort"` (default for parallel) — stop on first failure; remaining iterations are cancelled.
- `"ignore"` — treat all iteration outcomes as success regardless of adapter return.

**Aggregate outcome:** After all iterations complete, a synthetic aggregate outcome is computed:

- `all_succeeded` — all iterations returned a success outcome.
- `any_failed` — at least one iteration returned a failure outcome.
- The step's declared `outcome` blocks must cover both aggregate values (or use `default_outcome`).

**`each._prev`** is populated with the compiled output map from the preceding iteration. On the first iteration it is `null`. This enables sequential pipelines where each step depends on the previous result.

## Outcome model

Each step, wait, and approval node declares one or more `outcome` blocks mapping adapter-returned outcome names to `next` node references.

**Routing rules (in precedence order):**

1. If the adapter returns a named outcome matching a declared `outcome` block, route to that block's `next`.
2. If no match and `default_outcome` is set, route to the `default_outcome` block's `next`.
3. If no match and no `default_outcome`, the run fails with a routing error.

**`output` projection:** An `outcome` block may include an `output = {...}` expression to project a custom output map. If absent, the adapter's full output is passed downstream as `steps.<name>.*`.

**`shared_writes`:** An `outcome` block may include `shared_writes = { key = expr, ... }` to atomically update shared variables on that transition. Write ordering within a single outcome block is deterministic (declaration order).

**Terminal routing:** A `state` block with `terminal = true` terminates the run. `success = true` marks the run as succeeded; `success = false` marks it as failed. A run that reaches no terminal state is a runtime error (infinite loop guard via `policy.max_total_steps`).

**Default outcome:** If a step declares only one `outcome` block and the adapter returns no named outcome, the engine routes to that single outcome automatically (implicit default). With multiple outcomes, `default_outcome` must be explicit.

## Error model

**Compile errors** are detected during `make validate` / `criteria compile`. They include: missing required attributes, unknown block types, type mismatches in literal expressions, unresolved `next` references, missing terminal state, policy constraint violations, and adapter config schema violations.

**Runtime errors** are non-fatal by default unless they propagate to a terminal routing failure. Categories:

- **Adapter crash** — the adapter process exited unexpectedly. Controlled by `on_crash` on the step or adapter block: `abort` (default, fails the run) or `ignore` (routes to the `default_outcome`).
- **Expression evaluation error** — a namespace binding is missing or a function throws. The run fails with a diagnostic including the source location.
- **Routing error** — no matching outcome and no `default_outcome`. Always fatal.
- **Policy violation** — `max_total_steps` exceeded. Always fatal.

**`on_crash` propagation:** If `on_crash` is set on both the step and the adapter, the step-level setting takes precedence.

**Fatal error propagation:** Any fatal error transitions the run to an implicit `_error` terminal state (`success = false`). The `target_state` is not reached.

## Worked examples

### 1. Linear two-step workflow

```hcl
workflow "greet" {
  version = "1"
}

adapter "noop" "default" {}

step "hello" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true  success = true }
```

### 2. Branching switch

```hcl
workflow "branch" { version = "1" }

variable "env" { type = "string" }

adapter "noop" "default" {}

step "check" {
  target = adapter.noop.default
  outcome "ok"   { next = "switch_env" }
  outcome "fail" { next = "failed" }
}

switch "switch_env" {
  condition {
    match = var.env == "prod"
    next  = "deploy_prod"
  }
  default { next = "deploy_dev" }
}

state "deploy_prod" { terminal = true  success = true }
state "deploy_dev"  { terminal = true  success = true }
state "failed"      { terminal = true  success = false }
```

### 3. `for_each` iteration

```hcl
workflow "batch" { version = "1" }

variable "items" { type = "list(string)" }

adapter "noop" "default" {}

step "process" {
  target   = adapter.noop.default
  for_each = var.items
  input    { item = each.value }
  outcome "success" { next = "done" }
}

state "done" { terminal = true  success = true }
```

### 4. Parallel iteration

```hcl
workflow "parallel" { version = "1" }

variable "ids" { type = "list(string)" }

adapter "noop" "default" {}

step "fanout" {
  target   = adapter.noop.default
  for_each = var.ids
  parallel = true
  input    { id = each.value }
  outcome "success" { next = "done" }
}

state "done" { terminal = true  success = true }
```

### 5. Subworkflow call

```hcl
workflow "orchestrate" { version = "1" }

subworkflow "child" {
  source = "./child-workflow"
}

step "run_child" {
  target = subworkflow.child
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done"   { terminal = true  success = true }
state "failed" { terminal = true  success = false }
```

## Versioning

This specification describes language `version = "1"`. Behavior changes and additions are documented per `v0.<minor>.0` release in [CHANGELOG.md](../CHANGELOG.md). A new language version value (`"2"`) will be introduced only for backwards-incompatible grammar changes.
