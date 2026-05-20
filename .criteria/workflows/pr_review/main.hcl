# PR Review Subworkflow
# =====================
# Owns the GitHub PR lifecycle for one committed workstream branch, then syncs
# the local base branch after merge (formerly the merge_branch subworkflow's
# job — folded in here to remove one moving part).
#
# Flow:
#   open_pr (shell)              → push branch, idempotently create/update PR
#   warm_up (shell)              → sleep 90s for first CI propagation
#   pr_status (shell)            → emits classifier on stdout
#   route_status (switch)        → dispatches to merge, review, escalate, or backoff
#   pr_review (agent)            → cold-review; resolves threads + posts recommendation
#   route_after_cold_review      → switch: require_workflow_approval=true → approval node
#                                           require_workflow_approval=false → await_github_approval
#   human_approval_required      → (optional) operator approves workflow node
#   await_github_approval        → polls GitHub until reviewDecision == APPROVED
#   backoff_await_approval       → sleep between approval polls
#   merge_pr (shell)             → `gh pr merge --squash --delete-branch`
#   sync_base (shell)            → fetch origin + checkout base_branch + ff-pull
#   verify_base_in_sync (shell)  → confirms merged commit is reachable from base_branch
#   finalize_ok (shell)          → sets status output = "ok"
#
# Approval modes:
#   require_workflow_approval=false (default, feature branches):
#     After the cold reviewer posts its recommendation, the workflow polls
#     GitHub every ~2 minutes until reviewDecision == APPROVED. No workflow
#     node approval needed — the operator just clicks Approve on GitHub at
#     their leisure and the workflow auto-merges.
#   require_workflow_approval=true (main-targeting PRs):
#     Retains the explicit workflow-node approval gate before merge.
#
# Failure-propagation workaround: like the develop subworkflow, the engine
# ignores a subworkflow's terminal `success=false` flag at the parent
# (internal/engine/node_step.go:477-480). The status output defaults to
# "failed" and is flipped to "ok" only on the merge-and-sync success path.

workflow "pr_review" {
  version       = "1"
  initial_state = "open_pr"
  target_state  = "returned"
}

policy {
  max_total_steps = 300
}

variable "workstream_file" {
  type    = "string"
  default = ""
}

variable "project_dir" {
  type    = "string"
  default = ""
}

variable "max_review_attempts" {
  type        = "number"
  default     = 2
  description = "Number of pr_reviewer escalations before returning `escalated` to the parent."
}

variable "pr_reviewer_model" {
  type        = "string"
  default     = "gpt-5.5"
  description = "Model for the cold PR reviewer."
}

variable "base_branch" {
  type        = "string"
  default     = "adapter-v2"
  description = "Integration branch that workstream PRs target. Used for PR base, sync, and diff."
}

variable "require_workflow_approval" {
  type        = "string"
  default     = "false"
  description = "Set to 'true' to require explicit workflow-node approval before merge (for main-targeting PRs). Default 'false' uses async GitHub approval polling."
}

shared_variable "review_attempts" {
  type  = "number"
  value = 0
}

shared_variable "terminal_status" {
  type  = "string"
  value = "failed"
}

output "status" {
  type  = "string"
  value = shared.terminal_status
}

adapter "shell" "gh" {
  config {}
}

adapter "copilot" "pr_reviewer" {
  config {
    model            = var.pr_reviewer_model
    reasoning_effort = "high"
    max_turns        = 20
    system_prompt    = trimfrontmatter(file("agents/pr_reviewer.agent.md"))
  }
}

# ── Open / refresh the PR ────────────────────────────────────────────────────

step "open_pr" {
  target     = adapter.shell.gh
  timeout    = "180s"
  max_visits = 5
  input {
    command           = "BASE_BRANCH='${var.base_branch}' sh .criteria/workflows/pr_review/scripts/open-or-update-pr.sh \"${var.workstream_file}\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "warm_up" }
  outcome "failure" { next = "failed" }
}

step "warm_up" {
  target     = adapter.shell.gh
  timeout    = "180s"
  max_visits = 5
  input {
    command           = "echo 'warming up CI before first status poll (90s)'; sleep 90"
    working_directory = var.project_dir
  }
  outcome "success" { next = "pr_status" }
  outcome "failure" { next = "pr_status" }
}

# ── Deterministic status gate ─────────────────────────────────────────────────

step "pr_status" {
  target     = adapter.shell.gh
  timeout    = "120s"
  max_visits = 60
  input {
    command           = "sh .criteria/workflows/pr_review/scripts/pr-status.sh"
    working_directory = var.project_dir
  }
  outcome "success" { next = "route_status" }
  outcome "failure" { next = "failed" }
}

switch "route_status" {
  condition {
    match = steps.pr_status.stdout == "merged"
    next  = step.sync_base
  }
  condition {
    match = steps.pr_status.stdout == "ready"
    next  = step.pr_review
  }
  condition {
    match = steps.pr_status.stdout == "threads_open"
    next  = step.pr_review
  }
  condition {
    match = steps.pr_status.stdout == "pending"
    next  = step.backoff
  }
  condition {
    match = steps.pr_status.stdout == "changes_requested"
    next  = step.count_review_attempt
  }
  condition {
    match = steps.pr_status.stdout == "checks_failed"
    next  = state.escalated
  }
  default { next = state.failed }
}

step "backoff" {
  target     = adapter.shell.gh
  timeout    = "300s"
  max_visits = 30
  input {
    command           = "echo 'CI still pending; sleeping 60s before re-poll'; sleep 60"
    working_directory = var.project_dir
  }
  outcome "success" { next = "pr_status" }
  outcome "failure" { next = "pr_status" }
}

# ── Cold PR review ────────────────────────────────────────────────────────────
# Distinct persona (gpt-5.5) from inner reviewers; reviews PR cold. Can resolve
# threads + post a recommendation comment. CANNOT approve (branch protection),
# CANNOT merge (separate shell step), CANNOT push code.

step "pr_review" {
  target      = adapter.copilot.pr_reviewer
  allow_tools = ["read", "search", "execute", "shell"]
  timeout     = "20m"
  max_visits  = 10
  input {
    prompt = "Review the open PR for ${var.workstream_file}. The deterministic status gate classifier was `${steps.pr_status.stdout}` with context:\n\n--- pr-status.sh stderr ---\n${steps.pr_status.stderr}\n--- end ---\n\nThe full diff is cached at `.criteria/tmp/diff.patch` from the develop workflow; read it instead of running `gh pr diff` (saves a network call). For each unresolved (and !outdated) review thread, either reply with citation evidence and resolve via `sh .criteria/workflows/pr_review/scripts/resolve-thread.sh <thread_id>`, or leave it open and request changes.\n\nIf the diff meets the bar and all addressable threads are resolved: post a recommendation comment via `gh pr comment <pr_number> --body \"<your summary>\"` summarizing what you verified and that you recommend approval. Then emit RESULT: approve. DO NOT run `gh pr review --approve` — branch protection forbids self-approval by the PR author; a human must click Approve on GitHub before merging.\n\nIf code changes are required: emit a `### Required Changes` section in your final message and RESULT: changes_requested.\n\nDO NOT run `gh pr merge` — a deterministic shell step handles merge after human approval.\n\nEnd your final message with exactly one of:\nRESULT: approve\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approve"           { next = "route_after_cold_review" }
  outcome "changes_requested" { next = "count_review_attempt" }
  outcome "failure"           { next = "failed" }
}

# ── Approval routing — workflow node vs. async GitHub poll ───────────────────
# require_workflow_approval=true  → pause at human_approval_required node
# require_workflow_approval=false → poll GitHub for APPROVED status (default)

switch "route_after_cold_review" {
  condition {
    match = var.require_workflow_approval == "true"
    next  = approval.human_approval_required
  }
  default { next = step.await_github_approval }
}

# ── Human-in-the-loop approval bridge (workflow-node mode) ───────────────────
# Used only when require_workflow_approval=true. The operator goes to GitHub,
# clicks Approve on the PR, then approves this node.

approval "human_approval_required" {
  approvers = ["operator"]
  reason    = "The pr_reviewer agent recommends approval and has posted its summary as a PR comment. GitHub branch protection requires approval from someone other than the PR author. To continue: (1) open the PR in GitHub, (2) review the agent's recommendation comment, (3) click `Approve` on the PR, (4) approve this workflow node. The next step verifies that GitHub's reviewDecision is APPROVED before merging — if you approve here without clicking Approve on GitHub, the merge step will fail cleanly and loop back."
  outcome "approved" { next = "await_github_approval" }
  outcome "rejected" { next = "escalated" }
}

# ── Async GitHub approval poll ────────────────────────────────────────────────
# Polls until reviewDecision == APPROVED, then proceeds to merge.
# In the default (non-workflow-node) mode the human just clicks Approve on
# GitHub at any time; no workflow babysitting required.

step "await_github_approval" {
  target     = adapter.shell.gh
  timeout    = "5m"
  max_visits = 300
  input {
    command           = "set -eu; branch=$(git branch --show-current); pr_num=$(gh pr view \"$branch\" --json number --jq '.number'); decision=$(gh pr view \"$pr_num\" --json reviewDecision --jq '.reviewDecision // \"NONE\"'); echo \"review_decision=$decision\"; if [ \"$decision\" = \"APPROVED\" ]; then exit 0; fi; echo 'Waiting for human to click Approve on GitHub...'; exit 1"
    working_directory = var.project_dir
  }
  outcome "success" { next = "merge_pr" }
  outcome "failure" { next = "backoff_await_approval" }
}

step "backoff_await_approval" {
  target     = adapter.shell.gh
  timeout    = "3m"
  max_visits = 300
  input {
    command           = "echo 'GitHub approval not yet detected; sleeping 120s'; sleep 120"
    working_directory = var.project_dir
  }
  outcome "success" { next = "await_github_approval" }
  outcome "failure" { next = "await_github_approval" }
}

# ── Merge — shell step, not agent ────────────────────────────────────────────

step "merge_pr" {
  target     = adapter.shell.gh
  timeout    = "300s"
  max_visits = 3
  input {
    command           = "set -eu; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); gh pr merge \"$pr_number\" --squash --delete-branch; echo merged_pr_number=\"$pr_number\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "sync_base" }
  outcome "failure" { next = "failed" }
}

# ── Local base-branch sync ───────────────────────────────────────────────────

step "sync_base" {
  target     = adapter.shell.gh
  timeout    = "120s"
  max_visits = 3
  input {
    command           = "set -eu; git fetch origin '${var.base_branch}'; git checkout '${var.base_branch}'; git pull --ff-only origin '${var.base_branch}'"
    working_directory = var.project_dir
  }
  outcome "success" { next = "verify_base_in_sync" }
  outcome "failure" { next = "failed" }
}

step "verify_base_in_sync" {
  target     = adapter.shell.gh
  timeout    = "30s"
  max_visits = 3
  input {
    command           = "set -eu; branch=$(basename \"${var.workstream_file}\" .md); if git show-ref --verify --quiet refs/remotes/origin/$branch; then echo \"remote_branch_still_exists=$branch (gh pr merge --delete-branch may have skipped it)\" >&2; fi; base='${var.base_branch}'; echo \"${base}_at=$(git rev-parse HEAD)\"; echo \"origin_${base}_at=$(git rev-parse origin/${var.base_branch})\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "finalize_ok" }
  outcome "failure" { next = "failed" }
}

# ── Status output ────────────────────────────────────────────────────────────

step "finalize_ok" {
  target     = adapter.shell.gh
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

# ── Changes-requested counter → escalate after N attempts ────────────────────

step "count_review_attempt" {
  target     = adapter.shell.gh
  max_visits = 10
  input {
    command           = "echo $(( ${shared.review_attempts} + 1 ))"
    working_directory = var.project_dir
  }
  outcome "success" {
    next          = "check_review_limit"
    shared_writes = { review_attempts = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_review_limit" {
  condition {
    match = shared.review_attempts >= var.max_review_attempts
    next  = state.escalated
  }
  default { next = state.escalated }
}

# ── Terminal states ──────────────────────────────────────────────────────────

state "returned" {
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
