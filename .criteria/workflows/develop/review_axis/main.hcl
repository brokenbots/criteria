# Review Axis Subworkflow
# =======================
# Runs one specialist review axis on the active workstream diff. The parent
# (develop/main.hcl) invokes this in parallel for each kind in
# ["security", "quality", "workstream", "api_compat"], so adapter sessions for
# each axis are isolated.
#
# Outcome convention (works around engine isSuccessOutcome strictness in
# parallel iteration — internal/engine/extensions.go:115): each reviewer emits
# `RESULT: success` once its review is complete, regardless of verdict. The
# verdict (approved vs changes_requested) lives in the agent's stdout body,
# which the output projection captures as `report`. The owner adjudicator
# (develop/main.hcl) parses the verdict line from each report.
#
# Why not separate `approved`/`changes_requested` outcomes? The engine treats
# any parallel-iteration outcome whose name is not the literal "success" as a
# failure for aggregation purposes, which triggers `on_failure="abort"` and
# cancels sibling reviewers mid-review.

workflow "review_axis" {
  version       = "1"
  initial_state = "select_reviewer"
  target_state  = "failed"
}

policy {
  max_total_steps = 60
}

variable "review_kind" {
  type        = "string"
  default     = ""
  description = "Review axis: security, quality, workstream, or api_compat."
}

variable "workstream_file" {
  type        = "string"
  default     = ""
}

variable "project_dir" {
  type        = "string"
  default     = ""
}

variable "reviewer_model" {
  type        = "string"
  default     = "gpt-5.4"
}

adapter "copilot" "security_reviewer" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("agents/security_reviewer.agent.md"))
  }
}

adapter "copilot" "quality_reviewer" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("agents/quality_reviewer.agent.md"))
  }
}

adapter "copilot" "workstream_reviewer" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("agents/workstream_reviewer.agent.md"))
  }
}

adapter "copilot" "api_compat_reviewer" {
  config {
    model            = var.reviewer_model
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("agents/api_compat_reviewer.agent.md"))
  }
}

switch "select_reviewer" {
  condition {
    match = var.review_kind == "security"
    next  = step.security_review
  }
  condition {
    match = var.review_kind == "quality"
    next  = step.quality_review
  }
  condition {
    match = var.review_kind == "workstream"
    next  = step.workstream_review
  }
  condition {
    match = var.review_kind == "api_compat"
    next  = step.api_compat_review
  }
  default { next = state.failed }
}

step "security_review" {
  target      = adapter.copilot.security_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  timeout     = "15m"
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for security issues. Inspect `git diff origin/main...HEAD`, the workstream md, and the relevant code. Do not edit files. Return concrete findings only.\n\nYour review is COMPLETE when you have a verdict, even if that verdict is `changes_requested`. State your verdict on its own line as exactly one of:\nVERDICT: approved\nVERDICT: changes_requested\n\nThen end your final message with exactly:\nRESULT: success\n\nOnly emit `RESULT: failure` if you genuinely cannot perform the review (e.g. tools broken, prerequisite missing). Requesting changes is a successful review, not a failure."
  }
  outcome "success" {
    next   = "return"
    output = { axis = "security", report = steps.security_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "quality_review" {
  target      = adapter.copilot.quality_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  timeout     = "15m"
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for code quality, test sufficiency, complexity additions, and maintainability. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nYour review is COMPLETE when you have a verdict, even if that verdict is `changes_requested`. State your verdict on its own line as exactly one of:\nVERDICT: approved\nVERDICT: changes_requested\n\nThen end your final message with exactly:\nRESULT: success\n\nOnly emit `RESULT: failure` if you genuinely cannot perform the review."
  }
  outcome "success" {
    next   = "return"
    output = { axis = "quality", report = steps.quality_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "workstream_review" {
  target      = adapter.copilot.workstream_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  timeout     = "15m"
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for adherence to the workstream scope: affected files, non-goals, acceptance criteria, required tests, and implementation notes. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nYour review is COMPLETE when you have a verdict, even if that verdict is `changes_requested`. State your verdict on its own line as exactly one of:\nVERDICT: approved\nVERDICT: changes_requested\n\nThen end your final message with exactly:\nRESULT: success\n\nOnly emit `RESULT: failure` if you genuinely cannot perform the review."
  }
  outcome "success" {
    next   = "return"
    output = { axis = "workstream", report = steps.workstream_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "api_compat_review" {
  target      = adapter.copilot.api_compat_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  timeout     = "15m"
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for API and backwards-compatibility risk: HCL DSL changes, plugin gRPC API surface (sdk/pb/*.proto), event-log schema, and semver discipline. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nYour review is COMPLETE when you have a verdict, even if that verdict is `changes_requested`. State your verdict on its own line as exactly one of:\nVERDICT: approved\nVERDICT: changes_requested\n\nThen end your final message with exactly:\nRESULT: success\n\nOnly emit `RESULT: failure` if you genuinely cannot perform the review."
  }
  outcome "success" {
    next   = "return"
    output = { axis = "api_compat", report = steps.api_compat_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

state "failed" {
  terminal = true
  success  = false
}
