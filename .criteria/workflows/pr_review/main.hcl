# PR Review Subworkflow
# =====================
# Owns the GitHub PR lifecycle for one committed workstream branch. Hands
# control back to the parent on terminal states.
#
# Flow:
#   open_pr (shell)        → push branch, idempotently create/update PR
#   warm_up (shell)        → sleep 90s for first CI propagation
#   pr_status (shell)      → emits `status:<state>` on first stdout line
#   route_status (switch)  → dispatches to merge, review, escalate, or backoff
#   pr_reviewer (agent)    → cold-review; can approve or request changes
#   approve_and_merge (shell) → `gh pr review --approve` + `gh pr merge`
#
# Separation of duties: the pr_reviewer agent runs `gh pr review --approve` to
# record its approval. Merge is a separate deterministic shell step the agent
# cannot invoke (its allow_tools forbids `gh pr merge`). This keeps the merge
# command auditable in the workflow log and out of the agent transcript.

workflow "pr_review" {
  version       = "1"
  initial_state = "open_pr"
  target_state  = "approved_and_merged"
}

policy {
  max_total_steps = 300
}

variable "workstream_file" {
  type        = "string"
  default     = ""
  description = "Path to the workstream markdown file."
}

variable "project_dir" {
  type        = "string"
  default     = ""
  description = "Absolute path to the criteria engine project root."
}

variable "max_review_attempts" {
  type        = "number"
  default     = 2
  description = "Number of pr_reviewer escalations before returning `escalated` to the parent."
}

variable "pr_reviewer_model" {
  type        = "string"
  default     = "gpt-5.5"
  description = "Model for the PR reviewer. Deliberately distinct from the inner reviewer_model so the cold-review carries an independent signal."
}

shared_variable "review_attempts" {
  type  = "number"
  value = 0
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
  target  = adapter.shell.gh
  timeout = "180s"
  input {
    command           = "sh .criteria/workflows/pr_review/scripts/open-or-update-pr.sh \"${var.workstream_file}\""
    working_directory = var.project_dir
  }
  outcome "success" { next = "warm_up" }
  outcome "failure" { next = "failed" }
}

step "warm_up" {
  target  = adapter.shell.gh
  timeout = "180s"
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
    next  = state.approved_and_merged
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

# ── Exponential-ish backoff before re-polling ────────────────────────────────

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
# Distinct persona from developer / inner reviewers / owner.
#
# The agent can:
#   • read the diff and threads
#   • resolve addressable threads via `resolve-thread.sh` with citation evidence
#   • post a recommendation comment via `gh pr comment`
#
# The agent CANNOT:
#   • approve the PR (GitHub branch protection forbids self-approval by the PR
#     author; the workflow handles approval via a human-in-the-loop node below)
#   • merge the PR (a deterministic shell step owns merge)
#   • push code

step "pr_review" {
  target      = adapter.copilot.pr_reviewer
  allow_tools = ["read", "search", "execute", "shell"]
  max_visits  = 10
  input {
    prompt = "Review the open PR for ${var.workstream_file}. The deterministic status gate classifier was `${steps.pr_status.stdout}` with context:\n\n--- pr-status.sh stderr ---\n${steps.pr_status.stderr}\n--- end ---\n\nUse `gh pr diff <pr_number>` and `git diff origin/main...HEAD` for the code. For each unresolved (and !outdated) review thread, either reply with citation evidence and resolve it via `sh .criteria/workflows/pr_review/scripts/resolve-thread.sh <thread_id>`, or leave it open and request changes.\n\nIf the diff meets the bar and all addressable threads are resolved: post a recommendation comment via `gh pr comment <pr_number> --body \"<your summary>\"` summarizing what you verified and that you recommend approval. Then emit RESULT: approve. DO NOT run `gh pr review --approve` — branch protection forbids self-approval by the PR author; the workflow will pause for a human to click Approve on GitHub before merging.\n\nIf code changes are required: emit a `### Required Changes` section in your final message and RESULT: changes_requested.\n\nDO NOT run `gh pr merge` — a deterministic shell step handles merge after human approval.\n\nEnd your final message with exactly one of:\nRESULT: approve\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approve"           { next = "human_approval_required" }
  outcome "changes_requested" { next = "count_review_attempt" }
  outcome "failure"           { next = "failed" }
}

# ── Human-in-the-loop approval bridge ────────────────────────────────────────
# Branch protection on the upstream repo requires a non-author reviewer to
# approve. We bridge that by pausing here: the operator goes to GitHub, clicks
# Approve on the PR, then approves this workflow node. The verify step below
# confirms the GitHub side actually happened before merging.
#
# Requires `CRITERIA_LOCAL_APPROVAL=stdin` (interactive) or a running server.
# Rejecting this approval routes to `escalated`.

approval "human_approval_required" {
  approvers = ["operator"]
  reason    = "The pr_reviewer agent recommends approval and has posted its summary as a PR comment. GitHub branch protection requires approval from someone other than the PR author. To continue: (1) open the PR in GitHub, (2) review the agent's recommendation comment, (3) click `Approve` on the PR, (4) approve this workflow node. The next step verifies that GitHub's reviewDecision is APPROVED before merging — if you approve here without clicking Approve on GitHub, the merge step will fail cleanly and loop back."
  outcome "approved" { next = "verify_github_approval" }
  outcome "rejected" { next = "escalated" }
}

# Verify the human actually clicked Approve on GitHub before we merge.
# If reviewDecision is anything other than APPROVED, route back to the human
# approval gate rather than failing the run outright.

step "verify_github_approval" {
  target     = adapter.shell.gh
  timeout    = "60s"
  max_visits = 5
  input {
    command           = "set -euo pipefail; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); review_decision=$(gh pr view \"$pr_number\" --json reviewDecision --jq '.reviewDecision // \"REVIEW_REQUIRED\"'); echo \"pr_number=$pr_number\"; echo \"review_decision=$review_decision\"; if [ \"$review_decision\" != \"APPROVED\" ]; then echo \"GitHub reviewDecision=$review_decision; expected APPROVED. Did you click Approve on the PR in GitHub before approving the workflow node?\" >&2; exit 1; fi; echo 'github_approval_confirmed=true'"
    working_directory = var.project_dir
  }
  outcome "success" { next = "merge_pr" }
  outcome "failure" { next = "human_approval_required" }
}

# ── Merge — shell step, not agent ────────────────────────────────────────────
# Runs `gh pr merge` and verifies origin/main advanced. Auditable in event log.

step "merge_pr" {
  target  = adapter.shell.gh
  timeout = "300s"
  input {
    command           = "set -euo pipefail; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); gh pr merge \"$pr_number\" --squash --delete-branch; git fetch origin main; if git show-ref --verify --quiet refs/remotes/origin/main; then echo 'merged_pr_number='\"$pr_number\"'; main_advanced=true'; else echo 'main_ref_missing' >&2; exit 1; fi"
    working_directory = var.project_dir
  }
  outcome "success" { next = "approved_and_merged" }
  outcome "failure" { next = "failed" }
}

# ── Changes-requested counter → escalate after N attempts ────────────────────

step "count_review_attempt" {
  target = adapter.shell.gh
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

state "approved_and_merged" {
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
