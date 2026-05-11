# Develop Subworkflow
# ===================
# Implements one workstream: branch prep → developer → CI gate → parallel
# specialist reviews → owner adjudication → repair/cycle → commit-and-push.
#
# Deterministic gates short-circuit LLM calls wherever possible:
#   prepare-workstream-branch.sh → if already_merged, skip the whole workflow.
#   make ci (shell) → only invoke `repair` agent if it fails.
# This keeps token spend tied to irreducible-judgment steps (review, adjudicate).

workflow "develop" {
  version       = "1"
  initial_state = "prepare_branch"
  target_state  = "committed"
}

policy {
  max_total_steps = 500
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Path to the workstream markdown file, relative to project_dir."
}

variable "max_retries" {
  type        = "number"
  default     = 3
  description = "Maximum developer→owner cycles before requesting operator assistance."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "developer_model" {
  type        = "string"
  default     = "claude-sonnet-4.6"
}

variable "reviewer_model" {
  type        = "string"
  default     = "gpt-5.4"
}

shared_variable "cycle_count" {
  type  = "number"
  value = 0
}

adapter "copilot" "developer" {
  config {
    model            = var.developer_model
    reasoning_effort = "high"
    max_turns        = 30
    system_prompt    = trimfrontmatter(file("agents/developer.agent.md"))
  }
}

adapter "copilot" "owner" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/owner.agent.md"))
  }
}

adapter "copilot" "repair" {
  config {
    model            = var.developer_model
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/repair.agent.md"))
  }
}

adapter "shell" "ci" {
  config {}
}

subworkflow "review_axis" {
  source = "./review_axis"
}

# ── Restart-safe branch preparation ──────────────────────────────────────────

step "prepare_branch" {
  target     = adapter.shell.ci
  timeout    = "180s"
  max_visits = 10
  input {
    command           = "sh .criteria/workflows/bootstrap/scripts/prepare-workstream-branch.sh \"${var.workstream_file}\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_branch_state" }
  outcome "failure" { next = "failed" }
}

switch "route_branch_state" {
  condition {
    match = steps.prepare_branch.stdout == "already_merged"
    next  = state.committed
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
  input {
    prompt = "Read ${var.workstream_file} for the full task scope. Branch state classifier: `${steps.prepare_branch.stdout}` (one of: created, existing_local, existing_remote, existing_dirty; the branch name is `basename '${var.workstream_file}' .md`). If `created`, implement every acceptance-criterion item from a clean slate. If `existing_*`, inspect the current state, preserve useful work, and complete only missing items. Write tests. Run `make ci` to confirm; if it fails, fix before declaring ready. Update ${var.workstream_file} with implementation notes and check off completed items.\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Deterministic CI gate — `make ci` is the source of truth ────────────────
# Only invoke the LLM repair agent on failure. Green = jump straight to reviews.

step "ci_gate" {
  target     = adapter.shell.ci
  timeout    = "1200s"
  max_visits = 30
  input {
    command           = "make ci"
    working_directory = var.project_dir
  }
  outcome "success" { next = "specialized_reviews" }
  outcome "failure" { next = "repair_ci" }
}

step "repair_ci" {
  target      = adapter.copilot.repair
  allow_tools = ["read", "edit", "execute", "shell"]
  max_visits  = 10
  input {
    prompt = "`make ci` failed. Fix all failures with the smallest correct changes; do not refactor or expand scope. Do not raise the lint baseline cap or add to .golangci.baseline.yml — fix the finding instead.\n\n--- ci stdout ---\n${steps.ci_gate.stdout}\n--- ci stderr ---\n${steps.ci_gate.stderr}\n--- end ---\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Parallel specialist reviews — 4 axes ─────────────────────────────────────
# Reviewers always emit RESULT: success when their review is complete (regardless
# of whether the verdict is approved or changes_requested) — see the comment in
# review_axis/main.hcl explaining the engine's isSuccessOutcome strictness.
# on_failure = "continue" so a real reviewer failure (broken tooling) doesn't
# cancel the other in-flight reviewers; any_failed only fires if at least one
# reviewer truly errors out.

step "specialized_reviews" {
  target       = subworkflow.review_axis
  parallel     = ["security", "quality", "workstream", "api_compat"]
  parallel_max = 4
  on_failure   = "continue"
  max_visits   = 20
  input {
    review_kind     = each.value
    workstream_file = var.workstream_file
    project_dir     = var.project_dir
    reviewer_model  = var.reviewer_model
  }
  outcome "success"       { next = "_continue" }
  outcome "failure"       { next = "_continue" }
  outcome "all_succeeded" { next = "owner_review" }
  outcome "any_failed"    { next = "failed" }
}

# ── Owner adjudication ───────────────────────────────────────────────────────

step "owner_review" {
  target      = adapter.copilot.owner
  allow_tools = ["read", "search", "edit", "execute"]
  max_visits  = 20
  input {
    prompt = "You are the workstream owner for ${var.workstream_file}. Read the workstream, current diff (`git diff origin/main...HEAD`), and the four specialist reviewer reports below. Each report contains a `VERDICT: approved` or `VERDICT: changes_requested` line followed by findings. Decide which requests are legitimate, in scope, and mandatory. Reject overreach, duplicates, speculative rewrites, or anything contradicting the workstream non-goals.\n\nRecord your verdict under `## Owner Review Notes` in ${var.workstream_file}. If changes are needed, write only must-fix items. If complete, record owner approval.\n\n--- security ---\n${steps.specialized_reviews[0].report}\n--- quality ---\n${steps.specialized_reviews[1].report}\n--- workstream ---\n${steps.specialized_reviews[2].report}\n--- api_compat ---\n${steps.specialized_reviews[3].report}\n--- end ---\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
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
  reason    = "The developer/owner loop has reached max_retries cycles without convergence. Inspect the workstream md for owner notes. Approve to continue with a fresh cycle, or reject to fail the workstream."
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

# ── Iteration loop: developer addresses owner must-fix list ──────────────────

step "develop" {
  target      = adapter.copilot.developer
  allow_tools = ["*"]
  max_visits  = 20
  input {
    prompt = "The workstream owner has requested changes for ${var.workstream_file}. Read only the owner-approved must-fix list under `## Owner Review Notes`; do not chase raw specialist reviewer suggestions the owner rejected. Address every must-fix item completely, then run `make ci`.\n\nEnd your final message with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review" { next = "ci_gate" }
  outcome "failure"      { next = "failed" }
}

# ── Commit and push approved work ────────────────────────────────────────────

step "commit" {
  target      = adapter.copilot.developer
  allow_tools = ["execute", "shell", "read"]
  max_visits  = 5
  input {
    prompt = "Owner has approved ${var.workstream_file}. Confirm you are on the workstream branch. Stage all modified and new files for this workstream. If there are staged changes, commit with message `feat: complete ${var.workstream_file}`. If the tree is clean (branch was already complete), do NOT create an empty commit; just verify the branch is pushed. Push the branch to origin.\n\nDo not merge into main — the pr_review subworkflow handles PR creation and merge.\n\nEnd your final message with exactly one of:\nRESULT: success\nRESULT: failure"
  }
  outcome "success" { next = "committed" }
  outcome "failure" { next = "failed" }
}

state "committed" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
