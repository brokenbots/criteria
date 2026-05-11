# Bootstrap Workflow — criteria engine self-development
# =====================================================
# Runs one workstream end-to-end: preflight → develop → pr_review.
#
# `merge_branch` no longer exists as a separate subworkflow; its three shell
# steps (fetch_main, checkout_main, verify) are folded into pr_review/main.hcl
# after `merge_pr`, reducing one moving part.
#
# Subworkflow failure propagation workaround: the engine maps a subworkflow's
# terminal `success=false` state to outcome "success" at the parent
# (internal/engine/node_step.go:477-480). Until that's fixed, each subworkflow
# projects a `status` output ("ok" on the success path, "failed" by default);
# this workflow has a switch *after* each subworkflow call that routes on
# `steps.<sub>.status == "ok"`.
#
# Run with:
#   make self
#
# Or directly:
#   CRITERIA_LOCAL_APPROVAL=stdin \
#   CRITERIA_WORKFLOW_ALLOWED_PATHS=.criteria/workflows \
#     ./bin/criteria apply .criteria/workflows/bootstrap \
#       --var workstream_file=workstreams/td-04-todo-closure.md \
#       --var project_dir=$(pwd)
#
# Approval nodes that pause for the operator (CRITERIA_LOCAL_APPROVAL=stdin):
#   • develop/request_user_assist        — fires at max_retries in the dev loop
#   • pr_review/human_approval_required  — fires before merge; operator must
#     click Approve on the PR in GitHub (branch protection forbids self-
#     approval), then approve the workflow node to continue.

workflow "bootstrap" {
  version       = "1"
  initial_state = "preflight"
  target_state  = "done"
}

policy {
  max_total_steps = 5000
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Path to the workstream markdown file to process, relative to project_dir."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "max_retries" {
  type        = "number"
  default     = 3
  description = "Maximum developer/owner cycles before requesting operator assistance inside develop."
}

variable "developer_model" {
  type        = "string"
  default     = "claude-sonnet-4.6"
}

variable "reviewer_model" {
  type        = "string"
  default     = "gpt-5.4"
}

variable "pr_reviewer_model" {
  type        = "string"
  default     = "gpt-5.5"
}

adapter "shell" "default" {
  config {}
}

subworkflow "develop" {
  source = "../develop"
}

subworkflow "pr_review" {
  source = "../pr_review"
}

# ── Preflight: tooling + repo state ──────────────────────────────────────────

step "preflight" {
  target  = adapter.shell.default
  timeout = "60s"
  input {
    command           = "sh .criteria/workflows/bootstrap/scripts/preflight.sh"
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_preflight" }
  outcome "failure" { next = "failed" }
}

switch "route_preflight" {
  condition {
    match = steps.preflight.stdout == "ok"
    next  = step.develop
  }
  default { next = state.failed }
}

# ── Develop the workstream ───────────────────────────────────────────────────

step "develop" {
  target = subworkflow.develop
  input {
    workstream_file = var.workstream_file
    project_dir     = var.project_dir
    max_retries     = var.max_retries
    developer_model = var.developer_model
    reviewer_model  = var.reviewer_model
  }
  outcome "success" { next = "after_develop" }
  outcome "failure" { next = "failed" }
}

switch "after_develop" {
  condition {
    match = steps.develop.status == "ok"
    next  = step.pr_review
  }
  default { next = state.failed }
}

# ── PR review (opens PR, gates, human-approves, auto-merges, syncs main) ─────

step "pr_review" {
  target = subworkflow.pr_review
  input {
    workstream_file   = var.workstream_file
    project_dir       = var.project_dir
    pr_reviewer_model = var.pr_reviewer_model
  }
  outcome "success" { next = "after_pr_review" }
  outcome "failure" { next = "escalated" }
}

switch "after_pr_review" {
  condition {
    match = steps.pr_review.status == "ok"
    next  = state.done
  }
  default { next = state.escalated }
}

# ── Terminal states ──────────────────────────────────────────────────────────

state "done" {
  terminal = true
  success  = true
}

state "escalated" {
  terminal = true
  success  = false
}

state "failed" {
  terminal = true
  success  = false
}
