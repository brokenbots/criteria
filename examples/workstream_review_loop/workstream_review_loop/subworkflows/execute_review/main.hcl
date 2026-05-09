# Execute-Review subworkflow
# =========================
# Runs the execute-review loop for a single workstream file:
#   execute → verify (make ci) → review
# Bounded to max_execute_cycles (default 5). After max cycles, an approval
# node asks the operator whether to continue or fail.
#
# Adapters are isolated from the parent and PR pipeline subworkflow.

workflow "execute_review" {
  version       = "1"
  initial_state = "execute_init"
  target_state  = "approved"
}

variable "workstream_file" {
  type = "string"
}

variable "max_execute_cycles" {
  type    = "number"
  default = 5
  description = "Maximum execute-review cycles before requesting user assistance."
}

shared_variable "execute_cycle_count" {
  type  = "number"
  value = 0
}

adapter "copilot" "executor" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "high"
    max_turns        = 12
    system_prompt    = trimfrontmatter(file("agents/workstream-executor.agent.md"))
  }
}

adapter "copilot" "reviewer" {
  config {
    model            = "gpt-5.4"
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("agents/workstream-reviewer.agent.md"))
  }
}

adapter "shell" "default" {
  config { }
}

# ── Init pass ──────────────────────────────────────────────────────────────
# Bootstrap agent context. Each agent reads the workstream file on its first
# turn. That context persists in the live session for all subsequent loop turns.

step "execute_init" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  input {
    prompt = "Read ${var.workstream_file} for the full task scope.\n\nExecute the first implementation batch: complete the next unchecked items, write code and tests as needed, keep changes scoped and verifiable. Record your progress and notes in ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review"   { next = "review_init" }
  outcome "needs_approval" { next = "review_init" }
  outcome "failure"        { next = "failed" }
}

step "review_init" {
  target = adapter.copilot.reviewer
  allow_tools = ["*"]
  input {
    prompt = "Read ${var.workstream_file} for the workstream scope and the executor's latest work.\n\nReview the executor's changes against the acceptance bar. Write all findings and your verdict into the reviewer notes section of ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved"          { next = "commit_and_prepare_pr" }
  outcome "changes_requested" { next = "count_execute_cycle" }
  outcome "needs_review"      { next = "count_execute_cycle" }
  outcome "needs_approval"    { next = "count_execute_cycle" }
  outcome "failure"           { next = "failed" }
}

# ── Review loop: minimal signal prompts ────────────────────────────────────
# Agent context is fully established after the init pass.
# These prompts are coordination signals only — not instructions.

step "execute" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  max_visits  = 10
  input {
    prompt = "Reviewer requested changes. Notes are in ${var.workstream_file}."
  }
  outcome "success"        { next = "verify" }
  outcome "needs_review"   { next = "verify" }
  outcome "needs_approval" { next = "verify" }
  outcome "failure"        { next = "failed" }
}

step "verify" {
  target = adapter.shell.default
  input {
    command = "make ci 2>&1"
  }
  timeout = "120s"
  outcome "success" { next = "review" }
  outcome "failure" { next = "fix_verify" }
}

step "fix_verify" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  max_visits  = 5
  input {
    prompt = "Build/test verification failed. Fix all failures before this goes to review.\n\n--- verify output ---\n${steps.verify.stdout}\n--- end ---"
  }
  outcome "needs_review"   { next = "verify" }
  outcome "needs_approval" { next = "verify" }
  outcome "failure"        { next = "failed" }
}

step "review" {
  target = adapter.copilot.reviewer
  allow_tools = ["*"]
  max_visits  = 10
  input {
    prompt = "Ready for review. Latest work is in ${var.workstream_file}."
  }
  outcome "approved"          { next = "commit_and_prepare_pr" }
  outcome "changes_requested" { next = "count_execute_cycle" }
  outcome "needs_review"      { next = "count_execute_cycle" }
  outcome "needs_approval"    { next = "count_execute_cycle" }
  outcome "failure"           { next = "failed" }
}

# ── Cycle counting and user assistance ─────────────────────────────────────

step "count_execute_cycle" {
  target = adapter.shell.default
  input {
    command = "echo $(( ${shared.execute_cycle_count} + 1 ))"
  }
  outcome "success" {
    next          = "check_execute_cycles"
    shared_writes = { execute_cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_execute_cycles" {
  condition {
    match = shared.execute_cycle_count >= var.max_execute_cycles
    next  = state.request_user_assist
  }
  default {
    next = state.execute
  }
}

approval "request_user_assist" {
  approvers = ["operator"]
  reason    = "Execute-review loop has cycled without convergence. Continue with another cycle or abort?"
  outcome "approved" { next = "reset_execute_counter" }
  outcome "rejected" { next = "failed" }
}

step "reset_execute_counter" {
  target = adapter.shell.default
  input {
    command = "echo 0"
  }
  outcome "success" {
    next          = "execute"
    shared_writes = { execute_cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

# ── Commit approved work ────────────────────────────────────────────────────

step "commit_and_prepare_pr" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  input {
    prompt = "Approved. Commit all workstream changes with message:\nworkstream: complete ${var.workstream_file}\n\nEnd your final line with exactly one of:\nRESULT: success\nRESULT: failure"
  }
  outcome "success" { next = "approved" }
  outcome "failure" { next = "failed" }
}

# ── Terminal states ─────────────────────────────────────────────────────────

state "approved" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}

output "result" {
  type  = "string"
  value = "approved"
}