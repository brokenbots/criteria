# WS08 — `workflow.md` and `README.md` alignment

**Phase:** Language Cleanup · **Track:** Documentation · **Owner:** Workstream executor · **Depends on:** WS07 (spec aligned first so the two PRs don't conflict on adjacent docs). · **Unblocks:** Adapter documentation refresh; external contributors. · **Base branch:** `main`

## Context

`docs/workflow.md` is the human-readable language reference — longer, more narrative than the spec, aimed at workflow authors. It accumulated the same syntax drift as `LANGUAGE-SPEC.md` as WS01–WS06 landed: old `workflow "name" {}` label form, quoted type strings, inverted switch attribute names, an outdated subworkflow example in the fully-nested old format, missing `.chcl` extension mentions, and a stale "variable overrides are future" note that predates the WS06 `--var-file` flag.

`README.md` has one small inconsistency: the quickstart uses `version = "0.1"` while the spec says to use `"1"`.

## Prerequisites

- WS07 merged (keeps doc PRs non-conflicting).

## In scope

### Step 1 — Workflow header examples

Two places in `workflow.md` show `workflow "name" { ... }` (old label form). Update both to the current body-attribute form.

**Workflow Header section** (around line 34):
```hcl
# BEFORE:
workflow "deploy_pipeline" {
  version       = "1"
  initial_state = "validate"
  target_state  = "deployed"
}

# AFTER:
workflow "deploy_pipeline" {
  name          = "deploy_pipeline"
  version       = "1"
  initial_state = "validate"
  target_state  = "deployed"
}
```

**Default Environment section** (around line 208):
```hcl
# BEFORE:
workflow "multi_env_workflow" {
  version       = "1"
  initial_state = "start"
  target_state  = "done"
  environment   = "shell.production"
  ...
}

# AFTER:
workflow "multi_env_workflow" {
  name          = "multi_env_workflow"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
  environment   = shell.production
}
```
Note: `environment` also changes from a quoted string to a bare traversal.

### Step 2 — Variable type examples

The `## Variables` section shows `type = "string"`, `type = "number"`, `type = "bool"` (quoted strings). Replace with bare type expressions throughout the section:

```hcl
# BEFORE:
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

# AFTER:
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

Also update the **Supported types** bullet list prose to use unquoted code spans: `` `string` ``, `` `number` ``, `` `bool` ``, `` `list(string)` ``, `` `map(string)` `` (these are likely already unquoted in the prose — just verify).

Update the **Variables section intro** (around line 101): remove "Per-run override support is a planned future enhancement; currently the `default` attribute is the only value source." Replace with: "The `default` attribute is the value source for most workflows. For per-run overrides, use `--var-file` (see [CLI reference](#standalone-cli))."

### Step 3 — Switch attributes section (critical fix)

The **Switch** section's attribute documentation inverts the WS04-renamed block and attribute names. The code example in that section is already correct (`match { condition = ... }`). Only the prose attribute table is wrong.

Find the attributes bullet list that currently reads:
```
- **`condition`** (zero or more): Conditional arms evaluated in order. First match wins.
  - **`match`**: Boolean expression. See [Expressions](#expressions).
  - **`next`**: Target node in traversal form (…)
  - **`output`** (optional): …
```

Rewrite to:
```
- **`match`** (zero or more): Conditional arms evaluated in order. First match wins.
  - **`condition`**: Boolean HCL expression. See [Expressions](#expressions).
  - **`next`**: Target node in traversal form (`step.name`, `state.name`, `wait.name`,
    `approval.name`, `switch.name`) or bare keyword `return`.
  - **`output`** (optional): Object expression whose key/value pairs are stored under
    `steps.<switch_name>.*` before the target is entered.
```

### Step 4 — data block type examples

The `## Data Values` section declares data blocks with quoted types. Update to bare type expressions:

```hcl
# BEFORE:
data "internal" "counter" {
  type  = "number"
  value = 0
}
data "internal" "status_msg" {
  type  = "string"
  value = "pending"
}

# AFTER:
data "internal" "counter" {
  type  = number
  value = 0
}
data "internal" "status_msg" {
  type  = string
  value = "pending"
}
```

Also update the prose in that section: "`type` accepts the same type surface as `variable` declarations: `"string"`, `"number"`, `"bool"`, `"list(string)"`, ...`" → remove the quotes from the type values: `string`, `number`, `bool`, `list(string)`, `list(number)`, `list(bool)`, `map(string)`.

### Step 5 — Permissions example

The **Permissions** section shows `permissions { }` nested inside a `workflow { }` block. `permissions` is a top-level block. Update the example:

```hcl
# BEFORE:
workflow "secure_build" {
  permissions {
    allow_tools = ["shell:git*", "shell:make*"]
  }
  # ...
}

# AFTER:
workflow "secure_build" {
  name          = "secure_build"
  version       = "1"
  initial_state = "build"
  target_state  = "done"
}

permissions {
  allow_tools = ["shell:git*", "shell:make*"]
}
```
Add a `<!-- validator: fragment -->` directive before this example (or `<!-- validator: skip: ... -->` if the referenced steps/states are not in scope).

### Step 6 — Subworkflow example rewrite

The **Subworkflows** section's declaring example (around line 1699) uses the fully-nested old format: adapter, subworkflow, variable, step, and state blocks all inside `workflow { }`. This is the format removed in WS01. Rewrite as flat top-level declarations.

```hcl
# BEFORE (nested, old):
<!-- validator: skip: ... -->
workflow "deploy_pipeline" {
  version       = "1"
  initial_state = "lint"
  target_state  = "done"

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

  variable "env" {
    type    = "string"
    default = "staging"
  }

  step "lint" {
    target = adapter.shell.default
    input { command = "run-lint" }
    outcome "success" { next = state.done }
    outcome "failure" { next = state.done }
  }

  state "done" { terminal = true  success = true }
}

# AFTER (flat, current):
<!-- validator: skip: subworkflow source path ./subworkflows/smoke is illustrative; not present in this repo -->
workflow "deploy_pipeline" {
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
  input { command = "run-lint" }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" { terminal = true  success = true }
```

Also update the **Sub-workflow directory layout prose** (around line 1742) that says "its own `workflow \"<name>\" { ... }` wrapper" — rewrite as "its own `workflow { name = \"...\" }` header block."

### Step 7 — Directory mode: add `.chcl`

The **Directory mode** section references `.hcl` files throughout. Add `.chcl` at each occurrence:

- "A workflow can be split across multiple `.hcl` files" → "`.hcl` or `.chcl` files"
- Directory tree example labels (`workflow.hcl`, `adapters.hcl`, etc.) — add a note that `.chcl` extension is equally valid
- "Passing a `.hcl` file path" → "Passing a `.hcl` or `.chcl` file path"
- "Only `.hcl` files are accepted as file-path entry points" → "Only `.hcl` and `.chcl` files are accepted…"

### Step 8 — CLI section: `--var-file` flag

In `## Standalone CLI`, the `criteria apply` flags list is missing `--var-file`. Add it:

```
- **`--var-file <path>`** (repeatable): Load variable overrides from a `.chcl`, `.hcl`, or `.json`
  file. Multiple `--var-file` flags are merged left-to-right; later files overwrite earlier
  entries. `--var` individual overrides always take precedence over `--var-file` entries.
```

Add a similar entry under `criteria plan` if that command also exposes the flag.

### Step 9 — Variable overrides appendix

The **Variable overrides at runtime** appendix section (around line 1803) says both `--var-file` and `--var` are "planned post-1.5". Split into two:

> **`--var-file <path>`** is available now (see [CLI reference](#standalone-cli)). Load overrides from a file for multi-variable configurations.
>
> **`--var key=value`** individual flag overrides are still planned for a future release.

### Step 10 — README.md quickstart

In `README.md`, the quickstart example uses `version = "0.1"`. Change to `version = "1"` to match the spec recommendation. Also update the workflow header to use the current no-label form if the quickstart uses `workflow "name" {}`.

## Out of scope

- Changes to `docs/LANGUAGE-SPEC.md` — WS07.
- Changes to `docs/llm/*.md` — those files are current.
- Changes to `docs/adapters.md` — adapter documentation refresh is a separate effort.

## Reuse pointers

- `make validate-docs` — validates all non-skipped HCL fenced blocks in `docs/*.md`.
- `grep -n 'workflow "' docs/workflow.md` — find remaining label-form instances after edits.
- `grep -n 'type = "' docs/workflow.md` — find remaining quoted type strings.
- `examples/phase3-fold/fold-demo.hcl` — canonical example of flat top-level workflow syntax.

## Behavior change

Documentation only. No code change. No user-visible runtime behavior change.

## Tests required

- `make validate-docs` passes (no new failures; fixed examples now validate where they previously skipped).
- `grep -n 'workflow "' docs/workflow.md` returns zero results (no label-form instances remaining).
- `grep -n 'type = "' docs/workflow.md` returns zero results (no quoted type expressions remaining, outside of string-value attributes like `version = "1"`).

## Implementation Notes

### Checklist

- [ ] Step 1 — Workflow header examples updated (label → body attribute; environment traversal)
- [ ] Step 2 — Variable type examples updated (quoted strings → bare types); variables intro updated
- [ ] Step 3 — Switch attributes prose fixed (condition ↔ match inversion corrected)
- [ ] Step 4 — data block type examples updated
- [ ] Step 5 — Permissions example moved to top-level
- [ ] Step 6 — Subworkflow example rewritten (flat format, current syntax)
- [ ] Step 7 — Directory mode `.chcl` mentions added throughout
- [ ] Step 8 — `--var-file` flag documented in CLI section
- [ ] Step 9 — Variable overrides appendix split (`--var-file` live, `--var` still future)
- [ ] Step 10 — README.md quickstart version updated

### Reviewer Notes

_To be filled in during review._
