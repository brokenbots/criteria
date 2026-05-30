# Develop Subworkflow
# ===================
# Implements one workstream end-to-end:
#   prepare_branch → develop (LLM) → ci_gate (shell) → cache_diff →
#   pair_review (LLM, single-pass: workstream adherence + security + quality +
#   API conformance) → commit (shell) → finalize_ok (sets status="ok").
#
# Deep multi-axis review (4-axis parallel + owner adjudication) lives in the
# pr_review subworkflow and runs after the branch is committed and a PR is open.
# This keeps the develop loop tight and avoids convergence issues from running
# exhaustive multi-perspective reviews on every iteration.
#


workflow {

  name = "develop"
  version       = "1"
  initial_state = "prepare_branch"
  target_state  = "returned"
  policy {
    max_total_steps = 500
  }
}

variable "workstream_file" {
  type = string
  default     = ""
  description = "Path to the workstream markdown file, relative to project_dir."
}

variable "max_retries" {
  type = number
  default     = 3
  description = "Maximum developer→reviewer cycles before requesting operator assistance."
}

variable "project_dir" {
  type = string
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "developer_model" {
  type = string
  default     = "claude-sonnet-4.6"
}

variable "reviewer_model" {
  type = string
  default     = "gpt-5.4"
}

variable "base_branch" {
  type = string
  default     = "adapter-v2"
  description = "Integration branch to branch from and diff against."
}

shared_variable "cycle_count" {
  type = number
  value = 0
}

shared_variable "terminal_status" {
  type = string
  value = "failed"
}

output "status" {
  type = string
  value = shared.terminal_status
}

adapter "copilot" "developer" {
  config {
    model            = var.developer_model
    reasoning_effort = "high"
    max_turns        = 30
    system_prompt    = trimfrontmatter(file("agents/developer.agent.md"))
  }
}

adapter "copilot" "pair" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/pair.agent.md"))
  }
}

adapter "shell" "ci" {
  config {}
}

# ── Restart-safe branch preparation ──────────────────────────────────────────

step "prepare_branch" {
  target     = adapter.shell.ci
  timeout    = "180s"
  max_visits = 10
  input {
    command           = "BASE_BRANCH='${var.base_branch}' sh .criteria/workflows/bootstrap/scripts/prepare-workstream-branch.sh \"${var.workstream_file}\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_branch_state" }
  outcome "failure" { next = "failed" }
}

switch "route_branch_state" {
  condition {
    match = steps.prepare_branch.stdout == "already_merged"
    next  = step.finalize_ok
  }
  condition {
    match = steps.prepare_branch.stdout == "existing_local"
    next  = step.ci_gate
  }
  condition {
    match = steps.prepare_branch.stdout == "existing_remote"
    next  = step.ci_gate
  }
  default { next = step.develop_init }
}

# ── Initial implementation pass ──────────────────────────────────────────────

step "develop_init" {
  target      = adapter.copilot.developer
  allow_tools = ["*"]
  timeout     = "180m"
  input {
    prompt = <<-PROMPT
      Read ${var.workstream_file} for the full task scope. 
      Branch state classifier: `${steps.prepare_branch.stdout}` (one of: created, existing_local, existing_remote, existing_dirty; the branch name is `basename ${var.workstream_file}`). 
      If `created`, implement every acceptance-criterion item from a clean slate. If `existing_*`, inspect the current state, preserve useful work, and complete only missing items. 
      Write tests.
      Run `make build` to verify the code compiles clean before declaring ready — do not run the full test suite, the CI gate step handles that. 
      Update ${var.workstream_file} with implementation notes and check off completed items.
      
      ## Output Contract
      Once you have completed your task, you **must** call the `submit_outcome` tool to finalize the step. You may only select from the following allowed outcomes: `needs_review`, `failure`.

      The `reason` parameter in `submit_outcome` should contain a concise summary of your progress or a description of why you are failing.

      Do not attempt to signal the outcome via text alone; the step will only progress if the tool is called.
    PROMPT
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Deterministic CI gate with single auto-retry on flake ────────────────────
# If `make ci` fails, retry ONCE before invoking the LLM repair agent. CI
# flakes (network blips, race conditions in tests) are the most common
# transient failure and don't warrant a token-expensive repair session.

step "ci_gate" {
  target     = adapter.shell.ci
  timeout    = "1200s"
  max_visits = 30
  input {
    command           = "make ci"
    working_directory = var.project_dir
  }
  outcome "success" { next = "cache_diff" }
  outcome "failure" { next = "develop" }
}


# ── Cache the diff for reviewers ─────────────────────────────────────────────
# Writes .criteria/tmp/diff.patch + diff.stat once so all 4 reviewers can read
# the same file instead of each invoking `git diff origin/$base_branch...HEAD`.

step "cache_diff" {
  target     = adapter.shell.ci
  timeout    = "60s"
  max_visits = 10
  input {
    command           = "BASE_BRANCH='${var.base_branch}' sh .criteria/workflows/develop/scripts/cache-diff.sh"
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_diff" }
  outcome "failure" { next = "failed" }
}

switch "route_diff" {
  condition {
    match = steps.cache_diff.stdout == "no_changes"
    next  = step.commit
  }
  condition {
    match = steps.cache_diff.stdout == "ok"
    next  = step.pair_review
  }
  default { next = state.failed }
}

# ── Pair review: single-pass workstream + security + quality + API check ─────
# A focused pair reviewer checks the diff before commit. Unlike the deep
# multi-axis review in pr_review, this is a tight single-pass review designed
# to keep the developer on track without over-constraining the loop. The pair
# approves if things look broadly correct; it only blocks on concrete,
# actionable must-fix issues.

step "pair_review" {
  target      = adapter.copilot.pair
  allow_tools = ["read", "search", "execute"]
  timeout     = "180m"
  max_visits  = 20
  input {
    prompt = <<-PROMPT
      You are the pair reviewer for ${var.workstream_file}.
      Read the workstream md and `.criteria/tmp/diff.patch` (pre-cached; do not run git diff).

      Evaluate the diff across all four axes in a single focused pass:
      1. **Workstream adherence** — does the diff implement the acceptance criteria and stay within the stated scope?
      2. **Security** — any shell injection, path traversal, secret leakage, plugin trust-boundary issues, or allow-tools bypass?
      3. **Code quality** — obvious structural problems, missing test coverage for new paths, or complexity spikes that create future risk?
      4. **API conformance** — any unintended HCL DSL changes, proto field mutations, event-log schema breaks, or missing semver discipline?

      Approve if things are broadly correct. Request changes only for concrete, blocking issues you can cite with file:line evidence.
      Do not block on stylistic preferences, speculative concerns, or items outside the workstream scope.
      Do not edit files.

      In the submit_outcome reason, include a concise must-fix list (file:line + issue) if requesting changes, or a brief approval note if complete.
      This reason is passed directly to the developer — keep it tight and actionable.

      ## Output Contract
      End your final message with exactly one of:
      RESULT: approved
      RESULT: changes_requested
      RESULT: failure
    PROMPT
  }
  outcome "approved"          { next = "commit" }
  outcome "changes_requested" { next = "count_cycle" }
  outcome "failure"           { next = "failed" }
}

# ── Cycle counter + max-retries operator gate ────────────────────────────────

step "count_cycle" {
  target     = adapter.shell.ci
  max_visits = 30
  input {
    command           = "echo $(( ${shared.cycle_count} + 1 ))"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "check_limit"
    shared_writes = { cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_limit" {
  condition {
    match = shared.cycle_count >= var.max_retries
    next  = approval.request_user_assist
  }
  default { next = step.develop }
}

approval "request_user_assist" {
  approvers = ["operator"]
  reason    = "The developer/reviewer loop has reached max_retries cycles without convergence. Inspect the workstream md for reviewer notes. Approve to continue with a fresh cycle, or reject to fail the workstream."
  outcome "approved" { next = "reset_counter" }
  outcome "rejected" { next = "failed" }
}

step "reset_counter" {
  target     = adapter.shell.ci
  max_visits = 10
  input {
    command           = "echo 0"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "develop"
    shared_writes = { cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

# ── Iteration loop: developer addresses reviewer must-fix list ──────────────────

step "develop" {
  target      = adapter.copilot.developer
  allow_tools = ["*"]
  timeout     = "180m"
  max_visits  = 20
  input {
    prompt = <<-PROMPT
      The workstream reviewer has requested changes for ${var.workstream_file}.
      Pair reviewer must-fix list:
      
      ${steps.pair_review.reason}
      
      Address every item above completely. 
      Do not chase raw specialist reviewer suggestions the reviewer rejected. 
      Run `make build` to verify compilation before declaring ready — the CI gate step handles the full test suite.
      In the submit_outcome reason, briefly summarize the specific changes you made (file:line and what changed).

      ## Output Contract
      Once you have completed your task, you **must** call the `submit_outcome` tool to finalize the step. You may only select from the following allowed outcomes: `needs_review`, `failure`.

      The `reason` parameter in `submit_outcome` should contain a concise summary of your progress or a description of why you are failing.

      Do not attempt to signal the outcome via text alone; the step will only progress if the tool is called.
    PROMPT
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Commit + push (deterministic shell, no LLM) ──────────────────────────────
# reviewer approved (or unanimous specialist approval); the work is done. A
# deterministic shell step commits and pushes — no LLM judgment required.

step "commit" {
  target     = adapter.shell.ci
  timeout    = "120s"
  max_visits = 5
  input {
    command           = "set -eu; branch=$(git branch --show-current); if [ -z \"$branch\" ] || [ \"$branch\" = \"main\" ] || [ \"$branch\" = \"adapter-v2\" ]; then echo \"refusing to commit on protected branch: $${branch:-detached}\" >&2; exit 1; fi; git add -A; if git diff --cached --quiet; then echo 'no changes to commit; ensuring branch is pushed'; else git commit -m \"feat: complete ${var.workstream_file}\"; fi; git push --set-upstream origin \"$branch\" 2>/dev/null || git push origin \"$branch\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "finalize_ok" }
  outcome "failure" { next = "failed" }
}

# ── Set status output to "ok" on the success path ───────────────────────────
# This is the only place that flips terminal_status away from its default
# "failed" value. The bootstrap parent reads this via the projected output.

step "finalize_ok" {
  target     = adapter.shell.ci
  timeout    = "10s"
  max_visits = 5
  input {
    command           = "printf '%s' 'ok'"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "returned"
    shared_writes = { terminal_status = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

state "returned" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
