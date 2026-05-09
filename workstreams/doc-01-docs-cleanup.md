# doc-01 — Documentation cleanup: `docs/` directory

**Owner:** Workstream Cleanup agent · **Depends on:** none · **Blocks:** [doc-02](doc-02-meta-cleanup.md) (doc-02 may update cross-links once the roadmap file rename in Step 4 is done)

## Context

Phase 3 introduced a clean-break HCL rename (Phase 3 W11: `agent` → `adapter "<type>" "<name>"`; W12: adapter lifecycle automation; W14: universal `step.target`; W15: `outcome.next` replaces `transition_to`). Several documentation files were not updated to match the new language surface and still contain v0.2.0 syntax that is now **invalid HCL** if a user were to copy-paste it. Bugfix workstream BF-01 also fixed a variable default coercion limitation that is still documented as an open constraint.

This workstream fixes every stale reference in the `docs/` directory. No source code is changed.

## Prerequisites

- `make test` green on `main`.
- `make validate` green on `main` (verifies the `examples/` directory compiles — it is **not** touched by this workstream, but must remain green after the docs edits).

## In scope — allowed files

Exactly these files may be modified or renamed:

- `docs/workflow.md`
- `docs/plugins.md`
- `docs/contributing/your-first-pr.md`
- `docs/roadmap/phase-3.md` — rename only, no content change

No other file may be touched.

---

## Step 1 — `docs/contributing/your-first-pr.md`

### Fix I3 — stale repo link (old brand name)

The file contains a link that uses the legacy `overseer` repo name.

**Find (exact text):**
```
[gfi]: https://github.com/brokenbots/overseer/labels/good%20first%20issue
```

**Replace with:**
```
[gfi]: https://github.com/brokenbots/criteria/labels/good%20first%20issue
```

There are two occurrences — one at the top of the file and one under the Step 1 section. Both must be updated. Verify with:
```bash
grep -n "overseer" docs/contributing/your-first-pr.md
# expected: 0 matches
```

### Fix I4 — stale "Last reviewed" comment

**Find (exact text):**
```
<!-- Last reviewed: Phase 2 (2026-04) -->
```

**Replace with:**
```
<!-- Last reviewed: Phase 3 (2026-05) -->
```

---

## Step 2 — `docs/workflow.md` — Overview section (I5)

### Fix I5 — "Agents" bullet uses old terminology

In the **Overview** section (the `## Overview` bulleted list at the top of the file), one bullet still uses the pre-Phase-3 "Agents" terminology.

**Find (exact text):**
```
- **Agents**: long-lived adapter sessions that maintain state across multiple steps.
```

**Replace with:**
```
- **Adapters**: out-of-process plugin sessions that execute steps. Declared with `adapter "<type>" "<name>" { }` and referenced via `step.target`. Lifecycle is automatic — the engine opens and closes sessions as steps enter and exit scope.
```

---

## Step 3 — `docs/workflow.md` — Variables section (I6, I7, I8)

### Fix I7 — stale internal version reference

**Find (exact text):**
```
Variables are typed, read-only values declared at the workflow level and optionally overridden at runtime (per-run override support is a future enhancement in v1.5; currently defaults are the only source).
```

**Replace with:**
```
Variables are typed, read-only values declared at the workflow level. Per-run override support is a planned future enhancement; currently the `default` attribute is the only value source.
```

### Fix I6 — stale limitation note (BF-01 fixed this)

The note below the "Default values" heading incorrectly states that `list(string)` variables require an exact type match and cannot accept `["a", "b"]` literals. Bugfix workstream BF-01 fixed this in `workflow/compile_variables.go`.

**Find (exact text):**
```
**Note**: In HCL, literal lists like `["a", "b"]` are tuples. For `list(string)` variables, the compiler currently requires an exact type match. Use inline list literals in `for_each` or `input` blocks rather than variable defaults for now, or wait for the tuple-to-list coercion enhancement.
```

**Replace with:**
```
**Note**: In HCL, literal list syntax `["a", "b"]` produces a tuple. The compiler accepts tuple literals where a list type is declared and the element types are compatible — no explicit `tolist()` cast is needed.
```

### Fix I8 — Variables usage example uses v0.2.0 `adapter = "shell"` syntax

The code snippet after "Reference variables with `var.<name>`:" uses the v0.2.0 `adapter = "shell"` step attribute instead of the v0.3.0 `target = adapter.<type>.<name>` form.

**Find (exact text, including the surrounding comment and closing):**
```
<!-- validator: fragment -->
```hcl
step "deploy" {
  adapter = "shell"
  input {
    command = "deploy --env ${var.env}"
  }
  outcome "success" { next = "done" }
}
```
```

**Replace with:**
```
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
  outcome "success" { next = "done" }
}
```
```

> **Why `validator: skip`?** The fragment deliberately omits the `workflow` header and state declarations. The `fragment` directive relies on the validator merging the snippet with a minimal skeleton, but the adapter block reference (`adapter.shell.default`) requires the declaration to be present. Using `skip` with an explanatory comment is consistent with other illustrative-only excerpts in this file.

---

## Step 4 — `docs/workflow.md` — Agents section (I9, I10)

This is the most significant fix. The entire `## Agents` section (approximately lines 241–297) describes the v0.2.0 `agent "name" { adapter = "..." }` block with explicit `lifecycle = "open"` / `lifecycle = "close"` steps. Phase 3 workstreams W11 and W12 eliminated this pattern entirely:

- **W11** renamed `agent "<name>" { ... }` → `adapter "<type>" "<name>" { ... }` (two-label form, type is first).
- **W12** removed explicit open/close lifecycle management; adapters auto-open on scope entry and auto-close on exit (LIFO).

The section heading, the HCL example block, all attribute descriptions, and the "Lifecycle steps" subsection must be replaced.

### Fix I9 — Rewrite `## Agents` section to `## Adapters`

**Find (exact text — from the `## Agents` heading through the end of the "Lifecycle steps" subsection, ending just before `### Plugin discovery`):**

```
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
  outcome "success" { next = "ask_question" }
  outcome "failure" { next = "failed" }
}

step "ask_question" {
  agent       = "assistant"
  allow_tools = ["shell:ls*", "shell:cat*"]
  input {
    prompt = "List files in the current directory and summarize their purpose."
  }
  outcome "success" { next = "close_assistant" }
  outcome "failure" { next = "failed" }
}

step "close_assistant" {
  agent     = "assistant"
  lifecycle = "close"
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
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
```

**Replace with:**

```
## Adapters

Adapters are out-of-process plugin sessions declared at the workflow level and referenced from steps via `step.target`. The engine opens a session automatically when the first step that uses the adapter is entered and closes it automatically when the last step exits scope (LIFO order). No explicit open or close steps are needed.

<!-- validator: skip: illustrative excerpt; workflow header and state blocks omitted -->
```hcl
adapter "copilot" "assistant" {
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
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}
```

### Adapter block attributes

- **`<type>`** (first label, required): Plugin type. Determines which `criteria-adapter-<type>` binary is loaded.
- **`<name>`** (second label, required): Logical instance name. Multiple adapters of the same type may be declared with different names.
- **`on_crash`** (optional): Crash recovery policy: `"fail"` (default), `"respawn"`, `"abort_run"`.
- **`config`** (optional): Session-open configuration block. Attributes are adapter-specific. See [plugins.md](plugins.md) for per-adapter config schemas.

### Automatic lifecycle

The engine manages the full adapter session lifecycle without any explicit workflow steps:

- **Open**: the session is opened before the first step targeting this adapter executes.
- **Close**: the session is closed after the last step targeting this adapter in the current scope exits (including error paths).
- **LIFO order**: when multiple adapters are declared, they close in reverse declaration order.

Explicit `lifecycle = "open"` and `lifecycle = "close"` steps from v0.2.0 are no longer accepted and produce a compile error (`lifecycle attribute removed in v0.3.0`).
```

### Fix I10 — Remove `lifecycle` attribute from Step attributes list

Still within `## Steps` → `### Step attributes`, there is a bullet documenting `lifecycle` as a valid step attribute. This attribute was removed in Phase 3 W12.

**Find (exact text):**
```
- **`lifecycle`** (optional, agent-backed adapter steps only): `"open"` or `"close"`. See [Agents](#agents).
```

**Replace with:** *(delete the line entirely — no replacement)*

After deletion, the surrounding list must remain coherent:
```
- **`target`** (required): ...
- **`timeout`** (optional): ...
```
(No blank line or extra marker is needed between the two adjacent bullets.)

---

## Step 5 — `docs/plugins.md` — v0.2.0 syntax sweep (I11, I12)

`docs/plugins.md` has two categories of stale content:

1. **All `transition_to` occurrences** — 14 instances across the file. Phase 3 W15 renamed this attribute to `next`.
2. **The "HCL Surface — Agent-backed Workflows" section** — references `agent "name" { adapter = "..." }` blocks, explicit `lifecycle = "open"/"close"` steps, `agent = "name"` step attribute, and `adapter = "copilot"` (bare adapter name) syntax. All of these are v0.2.0 and invalid in v0.3.0.
3. **Dead workstream link** — references `[W15](../workstreams/15-copilot-submit-outcome-adapter.md)` (archived).

### Fix I11 — Replace all `transition_to` with `next`

Run a targeted in-file substitution. Every `transition_to` in `docs/plugins.md` must become `next`. There are 14 occurrences; do not leave any behind.

Verify:
```bash
grep -c "transition_to" docs/plugins.md
# expected: 0
```

The `branch` block example near the bottom of the file also needs to be updated from `branch` to `switch` syntax (the `transition_to` fields inside it are part of the same stale block):

**Find (exact text — the complete stale `branch` example block):**
```
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

**Replace with:**
```
switch "check_version" {
  condition {
    match = startswith(steps.get_version.stdout, "v1.")
    next  = state.deploy_v1
  }
  default {
    next = state.deploy_next
  }
}
```

### Fix I12 — Rewrite the "HCL Surface — Agent-backed Workflows" section

The entire section starting with `## HCL Surface — Agent-backed Workflows` through (but not including) `## Copilot Adapter Reference` uses v0.2.0 `agent` block syntax and must be replaced with v0.3.0 `adapter` block syntax.

**Find (exact text — the full old section):**
```
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
      prompt    = "Run `git status` in the current directory. Summarize the result in one short paragraph. Call submit_outcome with 'success' if you successfully ran `git status`, otherwise 'failure'."
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
- `ask` is the only execute step. For the Copilot plugin, `input.prompt` is required (Phase 1.5: step-level input moved from `config` to `input` block). `max_turns` is optional and limits the number of assistant turns; see "Outcome finalization" below for how the step outcome is determined.
- Separate close steps let the workflow clean up the session and still terminate in the right state for `success`, `needs_review`, or `failure`.
```

**Replace with:**
```
## HCL Surface — Adapter-backed Workflows

Adapter-backed workflows declare one or more `adapter "<type>" "<name>" { }` blocks at the top level and reference them from steps via `step.target`. The engine manages the full session lifecycle automatically — no explicit open or close steps are needed.

A minimal Copilot-backed workflow:

<!-- validator: skip: illustrative excerpt only -->
```hcl
workflow "agent_hello" {
  version       = "1"
  initial_state = "ask"
  target_state  = "done"
}

adapter "copilot" "assistant" {
  config {
    max_turns = 4
  }
}

step "ask" {
  target      = adapter.copilot.assistant
  allow_tools = ["shell:git status"]
  input {
    prompt = "Run `git status` in the current directory. Summarize the result in one short paragraph. Call submit_outcome with 'success' if you successfully ran `git status`, otherwise 'failure'."
  }

  outcome "success"      { next = "done" }
  outcome "needs_review" { next = "done" }
  outcome "failure"      { next = "failed" }
}

state "done"   { terminal = true }
state "failed" { terminal = true; success = false }
```

Key points:

- `adapter "copilot" "assistant"` declares a named adapter session. The first label is the plugin type (`copilot`); the second is the instance name (`assistant`). The engine resolves this to the `criteria-adapter-copilot` binary.
- `step.target = adapter.copilot.assistant` binds the step to the declared adapter instance. This is a traversal expression, not a string.
- The session is opened automatically before `ask` runs and closed automatically after it completes (success or failure). No explicit `lifecycle = "open"` or `lifecycle = "close"` steps exist in v0.3.0.
- For the Copilot plugin, `input.prompt` is the required step-level input. `max_turns` in the `config` block limits conversation turns; see "Outcome finalization" below for how the step outcome is determined.

See [docs/workflow.md — Adapters](workflow.md#adapters) for the full adapter block reference.
```

### Fix I12b — Dead workstream link in `allowed_outcomes` paragraph

**Find (exact text):**
```
The host validation guard in `internal/engine/node_step.go` is unchanged: adapters that ignore `allowed_outcomes` continue to function exactly as before. [W15](../workstreams/15-copilot-submit-outcome-adapter.md) is the first adapter consumer, adding a `submit_outcome` tool call to the Copilot adapter that uses this field to expose the declared outcome set to the model as a structured schema.
```

**Replace with:**
```
The host validation guard in `internal/engine/node_step.go` is unchanged: adapters that ignore `allowed_outcomes` continue to function exactly as before. The Copilot adapter is the first consumer: it exposes `allowed_outcomes` to the model as a `submit_outcome` tool schema, constraining the model to declared outcomes only.
```

### Fix I12c — `adapter = "shell"` bare-name syntax in `get_version` step example

Within the adapter outputs section there is a `get_version` step using the old bare `adapter = "shell"` attribute. This step is part of the same example block as the `branch` node fixed in I11.

**Find (exact text — the complete stale step):**
```
step "get_version" {
  adapter = "shell"
  input {
    command = "git describe --tags --always"
  }
  outcome "success" { transition_to = "check_version" }
}
```

**Replace with:**
```
step "get_version" {
  target = adapter.shell.default
  input {
    command = "git describe --tags --always"
  }
  outcome "success" { next = "check_version" }
}
```

### Fix I12d — `adapter = "shell"` bare-name syntax in shell adapter example

In the "HCL Surface — Shell Adapter" section, the closing outcome lines use v0.2.0 `transition_to`:

**Find (exact text):**
```
  outcome "success" { transition_to = "test" }
  outcome "failure" { transition_to = "failed" }
```
*(these appear immediately after the `working_directory` attribute in the shell adapter table example)*

**Replace with:**
```
  outcome "success" { next = "test" }
  outcome "failure" { next = "failed" }
```

After fixing I12d, re-run the grep check for `transition_to` to confirm zero matches.

### Fix I12e — Update Copilot Adapter Reference prose that still references `agent` blocks

The `## Copilot Adapter Reference` section's prose and examples still use `agent "planner" { adapter = "copilot" ... }` syntax in two places.

**First occurrence — Find (exact text):**
```
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
```

**Replace with:**
```
<!-- validator: skip: illustrative excerpt only -->
```hcl
adapter "copilot" "planner" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "medium"
    system_prompt    = "You are a senior software engineer. Think carefully before writing code."
    max_turns        = 8
  }
}
```
```

**Second occurrence — Find (exact text, the two-agent-block example with per-step override):**
```
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
```

**Replace with:**
```
<!-- validator: skip: illustrative excerpt only -->
```hcl
adapter "copilot" "planner" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "medium"  # default for all steps
  }
}

# Planning step uses higher reasoning effort.
step "plan" {
  target = adapter.copilot.planner
  input {
    prompt           = "Draft a step-by-step implementation plan."
    reasoning_effort = "high"   # overrides "medium" for this step only
  }
  outcome "success" { next = "execute" }
  outcome "failure" { next = "failed" }
}

# Execution steps inherit the adapter default ("medium").
step "execute" {
  target = adapter.copilot.planner
  input {
    prompt = "Implement the plan from the previous step."
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}
```
```

Also update the explanatory prose that follows these examples — the sentence about `agent { config { ... } }` placement in the "Common mistake" error message:

**Find (exact text):**
```
  agent "<name>" {
    adapter = "copilot"
    config {
      system_prompt = ...
    }
  }
```

**Replace with:**
```
  adapter "copilot" "<name>" {
    config {
      system_prompt = ...
    }
  }
```

---

## Step 6 — `docs/roadmap/phase-3.md` → rename to `phase-3-summary.md`

Rename the file using `git mv` for clean history. No content changes.

```bash
git mv docs/roadmap/phase-3.md docs/roadmap/phase-3-summary.md
```

After renaming, verify that no other file in `docs/` hard-links to the old path (use `grep -r "phase-3\.md" docs/`). If any are found, update them to `phase-3-summary.md`. The `workstreams/README.md` link will be updated as part of [doc-02](doc-02-meta-cleanup.md).

---

## Verification checklist

After all steps are complete, run these checks before marking the workstream done:

```bash
# No transition_to left in docs/plugins.md
grep -c "transition_to" docs/plugins.md   # must be 0

# No agent block syntax left in the two key docs files
grep -n "agent \"" docs/workflow.md docs/plugins.md   # must be 0 matches

# No lifecycle attribute in docs/workflow.md
grep -n "lifecycle" docs/workflow.md   # must be 0 matches (the word appears nowhere)

# No overseer references in your-first-pr.md
grep -n "overseer" docs/contributing/your-first-pr.md   # must be 0

# Rename completed
ls docs/roadmap/   # must contain phase-3-summary.md, not phase-3.md

# Examples still compile
make validate
```

---

## Exit criteria — reviewer checklist

The reviewer must verify each item independently. "Pass" means the criterion is fully met; any partial or ambiguous fix is a "Fail" requiring remediation.

| # | File | Check | Pass / Fail |
|---|------|-------|-------------|
| I3 | `docs/contributing/your-first-pr.md` | Both `[gfi]` link definitions point to `brokenbots/criteria`, not `brokenbots/overseer`. | |
| I4 | `docs/contributing/your-first-pr.md` | Header comment reads `Phase 3 (2026-05)`. | |
| I5 | `docs/workflow.md` | Overview bullet list contains `**Adapters**` (not `**Agents**`). | |
| I6 | `docs/workflow.md` | "Default values" subsection contains no reference to "exact type match" or "tuple-to-list coercion enhancement". | |
| I7 | `docs/workflow.md` | Variables intro sentence contains no `v1.5` version string. | |
| I8 | `docs/workflow.md` | Variables usage example uses `target = adapter.shell.default`; no `adapter = "shell"` attribute. | |
| I9 | `docs/workflow.md` | `## Agents` heading no longer exists; `## Adapters` heading is present. The section contains a v0.3.0 `adapter "copilot" "assistant"` block and no `agent "..."` blocks, no `lifecycle = "open"/"close"` steps. | |
| I10 | `docs/workflow.md` | Step attributes list does not contain a `lifecycle` bullet. | |
| I11a | `docs/plugins.md` | Zero occurrences of `transition_to`. | |
| I11b | `docs/plugins.md` | `branch` example block replaced with `switch`/`condition`/`default` syntax. | |
| I12a | `docs/plugins.md` | Section heading reads `## HCL Surface — Adapter-backed Workflows` (not "Agent-backed"). | |
| I12b | `docs/plugins.md` | `allowed_outcomes` paragraph contains no link to `workstreams/15-copilot-submit-outcome-adapter.md`. | |
| I12c | `docs/plugins.md` | `get_version` step uses `target = adapter.shell.default` and `next = "check_version"`. | |
| I12d | `docs/plugins.md` | Shell adapter closing example uses `next = "test"` and `next = "failed"`. | |
| I12e | `docs/plugins.md` | Both `agent "planner"` blocks replaced with `adapter "copilot" "planner"` blocks; per-step examples use `target = adapter.copilot.planner`; "Common mistake" error message shows `adapter "copilot" "<name>"`. | |
| R1 | `docs/roadmap/` | File `phase-3-summary.md` exists; `phase-3.md` does not. | |
| V1 | repo | `make validate` passes (examples unchanged). | |

All 17 checks must pass before reviewer approval.

---

## Executor notes

*(To be filled in by the executor agent during implementation.)*

## Reviewer notes

*(To be filled in by the reviewer agent.)*
