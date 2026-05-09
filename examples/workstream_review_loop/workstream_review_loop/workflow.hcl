# mode: standalone (uses copilot adapter plugins; server not required for basic flow,
# but approval nodes require CRITERIA_LOCAL_APPROVAL=stdin for interactive TTY or
# CRITERIA_LOCAL_APPROVAL=auto-approve for unattended CI)
#
# Workstream Reviewer Loop v2
# ==========================
# Processes a single workstream file through an execute-review subworkflow
# and a PR pipeline subworkflow, each with bounded cycles and user-assistance
# escape hatches.
#
# For multi-file processing, invoke this workflow once per file, or create a
# wrapper that runs it sequentially.
#
# Subworkflows:
#   execute_review — executor → verify (make ci) → reviewer loop, bounded to
#     max_execute_cycles (default 5). After max cycles, an approval node asks
#     the operator whether to continue or skip.
#   pr_pipeline — open PR → granular CI/comment/merge checks in a bounded loop
#     (max_pr_cycles default 3). Each check type is a separate shell step with
#     exit-code routing. PR feedback is handled internally with verify/fix steps.
#
# Usage:
#   CRITERIA_WORKFLOW_ALLOWED_PATHS=.github/agents:workstreams \
#     criteria apply examples/workstream_review_loop/workstream_review_loop
#
# For approval nodes (user assistance after max execute cycles):
#   CRITERIA_LOCAL_APPROVAL=stdin criteria apply examples/workstream_review_loop/workstream_review_loop

workflow "workstream_reviewer_loop" {
  version       = "2"
  initial_state = "checkout_branch"
  target_state  = "done"
}

policy {
  max_total_steps = 500
}

# ── Variables ──────────────────────────────────────────────────────────────

variable "workstream_file" {
  type        = "string"
  default     = "workstreams/05-shell-adapter-sandbox.md"
  description = "Path to the workstream file to process."
}

variable "max_execute_cycles" {
  type    = "number"
  default = 5
  description = "Maximum execute-review cycles before requesting user assistance."
}

variable "max_pr_cycles" {
  type    = "number"
  default = 3
  description = "Maximum PR triage cycles before requesting user assistance."
}

# ── Adapter ─────────────────────────────────────────────────────────────────
# Only the shell adapter is needed at the parent level for checkout.
# Subworkflows declare their own copilot adapters with isolated sessions.

adapter "shell" "default" {
  config { }
}

# ── Subworkflow declarations ────────────────────────────────────────────────

subworkflow "execute_review" {
  source = "./subworkflows/execute_review"
  input = {
    workstream_file    = var.workstream_file
    max_execute_cycles = var.max_execute_cycles
  }
}

subworkflow "pr_pipeline" {
  source = "./subworkflows/pr_pipeline"
  input = {
    workstream_file = var.workstream_file
    max_pr_cycles   = var.max_pr_cycles
  }
}

# ── Steps ───────────────────────────────────────────────────────────────────

step "checkout_branch" {
  target = adapter.shell.default
  input {
    command = "branch=$(basename '${var.workstream_file}' .md) && current=$(git branch --show-current) && if [ \"$current\" = \"main\" ]; then git checkout -b \"$branch\"; else echo \"already on branch: $current\"; fi"
  }
  timeout = "10s"
  outcome "success" { next = "run_execute_review" }
  outcome "failure" { next = "failed" }
}

step "run_execute_review" {
  target = subworkflow.execute_review
  outcome "success" { next = "run_pr_pipeline" }
  outcome "failure" { next = "failed" }
}

step "run_pr_pipeline" {
  target = subworkflow.pr_pipeline
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

# ── Terminal states ──────────────────────────────────────────────────────────

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}