# Review Axis Subworkflow
# =======================
# Runs one specialist review axis on the active workstream diff. The parent
# (develop/main.hcl) invokes this in parallel for each kind in
# ["security", "quality", "workstream", "api_compat"], so adapter sessions for
# each axis are isolated.
#
# Each axis emits structured output { axis, verdict, report } that the parent
# passes to the owner for adjudication.

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
  description = "Review axis to run: security, quality, workstream, or api_compat."
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Path to the workstream markdown file, relative to project_dir."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
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
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for security issues. Inspect `git diff origin/main...HEAD`, the workstream md, and the relevant code. Do not edit files. Return concrete findings only.\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved" {
    next   = "return"
    output = { axis = "security", verdict = "approved", report = steps.security_review.stdout }
  }
  outcome "changes_requested" {
    next   = "return"
    output = { axis = "security", verdict = "changes_requested", report = steps.security_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "quality_review" {
  target      = adapter.copilot.quality_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for code quality, test sufficiency, complexity additions, and maintainability. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved" {
    next   = "return"
    output = { axis = "quality", verdict = "approved", report = steps.quality_review.stdout }
  }
  outcome "changes_requested" {
    next   = "return"
    output = { axis = "quality", verdict = "changes_requested", report = steps.quality_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "workstream_review" {
  target      = adapter.copilot.workstream_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for adherence to the workstream scope: affected files, non-goals, acceptance criteria, required tests, and implementation notes. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved" {
    next   = "return"
    output = { axis = "workstream", verdict = "approved", report = steps.workstream_review.stdout }
  }
  outcome "changes_requested" {
    next   = "return"
    output = { axis = "workstream", verdict = "changes_requested", report = steps.workstream_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

step "api_compat_review" {
  target      = adapter.copilot.api_compat_reviewer
  allow_tools = ["read", "search", "shell", "execute"]
  input {
    prompt = "Review the active diff for ${var.workstream_file} in ${var.project_dir} for API and backwards-compatibility risk: HCL DSL changes, plugin gRPC API surface (sdk/pb/*.proto), event-log schema, and semver discipline. Inspect `git diff origin/main...HEAD` and the workstream md. Do not edit files. Return concrete findings only.\n\nEnd your final message with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved" {
    next   = "return"
    output = { axis = "api_compat", verdict = "approved", report = steps.api_compat_review.stdout }
  }
  outcome "changes_requested" {
    next   = "return"
    output = { axis = "api_compat", verdict = "changes_requested", report = steps.api_compat_review.stdout }
  }
  outcome "failure" { next = "failed" }
}

state "failed" {
  terminal = true
  success  = false
}
