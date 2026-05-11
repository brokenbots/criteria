# Merge Branch Subworkflow
# ========================
# Post-PR-merge local sync. By the time we arrive here, pr_review has already
# run `gh pr merge --squash --delete-branch`, so the PR commit is on origin/main.
# Our job: leave the local repo on a clean main that includes the merged commit.
#
# Deterministic shell handles the happy path. A narrow git_safety agent repairs
# the rare cases where the sync fails (dirty working tree, divergent main).

workflow "merge_branch" {
  version       = "1"
  initial_state = "fetch_main"
  target_state  = "synced"
}

policy {
  max_total_steps = 100
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Workstream markdown path. The branch name is derived as `basename(workstream_file, .md)` inside the verify step (since the engine does not yet provide a basename function in HCL eval)."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "max_retries" {
  type        = "number"
  default     = 2
  description = "Maximum git_safety repair cycles before failing."
}

shared_variable "sync_cycle" {
  type  = "number"
  value = 0
}

adapter "shell" "git" {
  config {}
}

adapter "copilot" "git_safety" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/git_safety.agent.md"))
  }
}

step "fetch_main" {
  target  = adapter.shell.git
  timeout = "120s"
  input {
    command           = "set -euo pipefail; git fetch origin main"
    working_directory = var.project_dir
  }
  outcome "success" { next = "checkout_main" }
  outcome "failure" { next = "count_sync_cycle" }
}

step "checkout_main" {
  target  = adapter.shell.git
  timeout = "60s"
  input {
    command           = "set -euo pipefail; git checkout main && git pull --ff-only origin main"
    working_directory = var.project_dir
  }
  outcome "success" { next = "verify_branch_merged" }
  outcome "failure" { next = "count_sync_cycle" }
}

step "verify_branch_merged" {
  target  = adapter.shell.git
  timeout = "60s"
  input {
    command           = "set -euo pipefail; branch=$(basename \"${var.workstream_file}\" .md); if git show-ref --verify --quiet refs/remotes/origin/$branch; then git merge-base --is-ancestor origin/$branch HEAD && echo \"branch_in_main=true branch=$branch\"; else echo \"branch_deleted=true branch=$branch\"; fi"
    working_directory = var.project_dir
  }
  outcome "success" { next = "synced" }
  outcome "failure" { next = "count_sync_cycle" }
}

step "count_sync_cycle" {
  target = adapter.shell.git
  input {
    command           = "echo $(( ${shared.sync_cycle} + 1 ))"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "check_sync_limit"
    shared_writes = { sync_cycle = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_sync_limit" {
  condition {
    match = shared.sync_cycle >= var.max_retries
    next  = state.failed
  }
  default { next = step.repair_sync }
}

step "repair_sync" {
  target      = adapter.copilot.git_safety
  allow_tools = ["read", "edit", "shell", "execute"]
  max_visits  = 5
  input {
    prompt = "The post-PR local main sync failed for workstream `${var.workstream_file}` in ${var.project_dir}. The PR has already been merged on GitHub via `gh pr merge --squash --delete-branch`. Inspect the repository state, resolve any dirty working tree or divergent-main issue without destructive git operations, and leave the repo on main with a clean working tree containing the merged commit. The branch name is `basename '${var.workstream_file}' .md`.\n\nDo not push. Do not force any branch operation. If you cannot resolve cleanly, fail and let the operator step in.\n\nEnd your final message with exactly one of:\nRESULT: success\nRESULT: failure"
  }
  outcome "success" { next = "fetch_main" }
  outcome "failure" { next = "failed" }
}

state "synced" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
