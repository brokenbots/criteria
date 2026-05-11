# Bootstrap Workflow — criteria engine self-development
# =====================================================
# Runs one workstream end-to-end: develop → pr_review → merge_branch.
#
# This workflow expects `workstream_file` to be provided via --var (the
# Makefile's `make self` target picks the next pending workstream and passes
# it). The workflow itself is a clean linear pipeline; picking lives in the
# `pick-next-workstream.sh` script invoked by the Makefile.
#
# Each subworkflow opens isolated adapter sessions and hands control back via
# its terminal state. The bootstrap routes on each subworkflow's outcome:
#   develop      committed → pr_review;       failed → failed
#   pr_review    approved_and_merged → merge_branch; escalated/failed → escalated
#   merge_branch synced → done;               failed → failed
#
# Run with:
#   make self
#
# Or directly:
#   CRITERIA_LOCAL_APPROVAL=stdin \
#   CRITERIA_WORKFLOW_ALLOWED_PATHS=.criteria/workflows \
#     ./bin/criteria apply .criteria/workflows/bootstrap \
#       --var workstream_file=workstreams/td-01-lint-baseline-ratchet.md \
#       --var project_dir=$(pwd)
#
# Approval nodes that pause for the operator (CRITERIA_LOCAL_APPROVAL=stdin):
#   • develop/request_user_assist     — fires at max_retries in the dev loop
#   • pr_review/human_approval_required — fires before merge; operator must
#     click Approve on the PR in GitHub (branch protection forbids self-
#     approval by the PR author), then approve the workflow node to continue.
#
# CRITERIA_LOCAL_APPROVAL=auto-approve will auto-approve ALL gates including
# the human-PR-approval bridge — DO NOT USE unless you have a CI bot account
# with bypass-actors on the branch protection rule.

workflow "bootstrap" {
  version       = "1"
  initial_state = "develop"
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
  description = "Model for the inner specialist reviewers and the workstream owner."
}

variable "pr_reviewer_model" {
  type        = "string"
  default     = "gpt-5.5"
  description = "Model for the cold PR reviewer. Distinct from reviewer_model so the PR review signal is independent of the inner review."
}

subworkflow "develop" {
  source = "../develop"
}

subworkflow "pr_review" {
  source = "../pr_review"
}

subworkflow "merge_branch" {
  source = "../merge_branch"
}

step "develop" {
  target = subworkflow.develop
  input {
    workstream_file = var.workstream_file
    project_dir     = var.project_dir
    max_retries     = var.max_retries
    developer_model = var.developer_model
    reviewer_model  = var.reviewer_model
  }
  outcome "success" { next = "pr_review" }
  outcome "failure" { next = "failed" }
}

step "pr_review" {
  target = subworkflow.pr_review
  input {
    workstream_file   = var.workstream_file
    project_dir       = var.project_dir
    pr_reviewer_model = var.pr_reviewer_model
  }
  outcome "success" { next = "merge_branch" }
  outcome "failure" { next = "escalated" }
}

step "merge_branch" {
  target = subworkflow.merge_branch
  input {
    branch_name = trimsuffix(basename(var.workstream_file), ".md")
    project_dir = var.project_dir
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

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
