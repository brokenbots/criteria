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
#   route_status (switch)        → dispatches to merge, deep-review, thread-review, escalate, or backoff
#   specialized_reviews (agent×4)→ deep parallel 4-axis review (security, quality, workstream, api_compat)
#   verdict_aggregate (shell)    → checks whether all 4 axes approved unanimously
#   check_unanimous (switch)     → unanimous → pr_review; not → owner_review
#   owner_review (agent)         → adjudicates specialist reports; approved → pr_review
#                                                                changes_requested → post_pr_findings
#   post_pr_findings (shell)     → posts must-fix list as a PR comment; → count_review_attempt
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
# Approval modes and deep review:
#   When pr_status == "ready" (CI green, no open threads), the workflow runs a
#   deep 4-axis parallel review before the cold pr_review agent. This is the
#   exhaustive multi-axis audit deferred from the develop loop. The develop loop
#   uses a lightweight pair reviewer to keep iteration tight; the deep review
#   happens here as the final gate before human approval and merge.
#   When pr_status == "threads_open", the cold pr_review agent handles thread
#   triage directly (no deep review needed — CI is green, just resolve threads).
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

workflow {

  name = "pr_review"
  version       = "1"
  initial_state = "open_pr"
  target_state  = "returned"
  policy {
    max_total_steps = 300
  }
}

variable "workstream_file" {
  type = string
  default = ""
}

variable "project_dir" {
  type = string
  default = ""
}

variable "max_review_attempts" {
  type = number
  default     = 2
  description = "Number of pr_reviewer escalations before returning `escalated` to the parent."
}

variable "reviewer_model" {
  type        = "string"
  default     = "gpt-5.4"
  description = "Model for the deep specialist reviewers (security, quality, workstream, api_compat)."
}

variable "reviewer_base_url" {
  type        = "string"
  default     = ""
  description = "Optional base URL override for the specialist reviewer model."
}

variable "pr_reviewer_model" {
  type = string
  default     = "gpt-5.5"
  description = "Model for the cold PR reviewer."
}

variable "base_branch" {
  type = string
  default     = "adapter-v2"
  description = "Integration branch that workstream PRs target. Used for PR base, sync, and diff."
}

variable "require_workflow_approval" {
  type = string
  default     = "false"
  description = "Set to 'true' to require explicit workflow-node approval before merge (for main-targeting PRs). Default 'false' uses async GitHub approval polling."
}

shared_variable "review_attempts" {
  type = number
  value = 0
}

shared_variable "terminal_status" {
  type = string
  value = "failed"
}

output "status" {
  type = string
  value = shared.terminal_status
}

subworkflow "review_axis" {
  source = "./review_axis"
}

adapter "shell" "gh" {
  config {}
}

adapter "copilot" "reviewer" {
  config {
    model            = var.reviewer_model
    provider_base_url   = var.reviewer_base_url
    reasoning_effort = "high"
    max_turns        = 10
  }
}

adapter "copilot" "owner" {
  config {
    model            = var.reviewer_model
    #provider_base_url   = var.reviewer_base_url
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("agents/owner.agent.md"))
  }
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
    next  = step.specialized_reviews
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

# ── Deep parallel specialist reviews — 4 axes ──────────────────────────────
# Runs only when pr_status == "ready" (CI green, no open threads). This is the
# exhaustive review deferred from the develop loop. Each axis runs in parallel
# and always emits RESULT: success when complete (verdict is in the report body).
# on_failure = "continue" so one broken axis doesn't cancel the others.

step "specialized_reviews" {
  target       = subworkflow.review_axis
  parallel     = ["security", "quality", "workstream", "api_compat"]
  parallel_max = 4
  on_failure   = "continue"
  max_visits   = 10
  input {
    review_kind     = each.value
    workstream_file = var.workstream_file
    project_dir     = var.project_dir
    reviewer_model  = var.reviewer_model
  }
  outcome "success"       { next = "_continue" }
  outcome "failure"       { next = "_continue" }
  outcome "all_succeeded" { next = "verdict_aggregate" }
  outcome "any_failed"    { next = "failed" }
}

# ── Verdict aggregation: skip owner adjudication on unanimous approval ─────────

step "verdict_aggregate" {
  target     = adapter.shell.gh
  timeout    = "30s"
  max_visits = 10
  input {
    command           = <<-CMD
      mkdir -p .criteria/tmp
      cat > .criteria/tmp/verdict_agg_input.txt <<'CRITERIA_VERDICT_REPORTS_EOF'
      ${steps.specialized_reviews[0].report}
      ${steps.specialized_reviews[1].report}
      ${steps.specialized_reviews[2].report}
      ${steps.specialized_reviews[3].report}
      CRITERIA_VERDICT_REPORTS_EOF
      sh .criteria/workflows/develop/scripts/aggregate-verdicts.sh < .criteria/tmp/verdict_agg_input.txt
    CMD
    working_directory = var.project_dir
  }
  outcome "success" { next = "check_unanimous" }
  outcome "failure" { next = "owner_review" }
}

switch "check_unanimous" {
  condition {
    match = steps.verdict_aggregate.stdout == "unanimous"
    next  = step.pr_review
  }
  default { next = step.owner_review }
}

# ── Owner adjudication (only when specialists disagree) ─────────────────────
# Reads all four specialist reports and the workstream md, then decides what
# is genuinely blocking. Approved work proceeds to the cold pr_review.
# Changes requested are posted as a PR comment and the workflow backs off to
# wait for the developer to push fixes.

step "owner_review" {
  target      = adapter.copilot.owner
  allow_tools = ["read", "search", "execute"]
  timeout     = "20m"
  max_visits  = 10
  input {
    prompt = <<-PROMPT
      You are the workstream owner adjudicating the four specialist reviewer reports for ${var.workstream_file}.
      Read the workstream md and `.criteria/tmp/diff.patch` (pre-cached; do not run git diff).
      The four specialist reviewer reports are below — each contains a `VERDICT:` line and findings.
      Decide which requests are legitimate, in scope, and mandatory.
      Reject overreach, duplicates, speculative rewrites, or anything contradicting the workstream non-goals.

      Record your verdict under `## Owner Review Notes` in ${var.workstream_file}.
      If changes are needed, write only must-fix items there.

      In the submit_outcome reason, include a concise must-fix list (file:line + issue) if requesting changes,
      or a brief approval note if complete. This reason is posted as a PR comment if changes are requested.

      --- security ---
      ${steps.specialized_reviews[0].report}
      --- quality ---
      ${steps.specialized_reviews[1].report}
      --- workstream ---
      ${steps.specialized_reviews[2].report}
      --- api_compat ---
      ${steps.specialized_reviews[3].report}
      --- end ---

      End your final message with exactly one of:
      RESULT: approved
      RESULT: changes_requested
      RESULT: failure
    PROMPT
  }
  outcome "approved"          { next = "pr_review" }
  outcome "changes_requested" { next = "post_pr_findings" }
  outcome "failure"           { next = "failed" }
}

# ── Post findings as PR comment, then back off ─────────────────────────────
# Posts the owner must-fix list as a PR comment so the developer can see what
# needs to be fixed, then counts the attempt and enters the backoff-poll loop.

step "post_pr_findings" {
  target     = adapter.shell.gh
  timeout    = "60s"
  max_visits = 5
  input {
    command           = <<-CMD
      set -eu
      branch=$(git branch --show-current)
      pr_num=$(gh pr view "$branch" --json number --jq '.number')
      gh pr comment "$pr_num" --body "### Deep Review: Changes Requested\n\n${steps.owner_review.reason}\n\n_Posted by the automated review workflow. Please push fixes to the branch and CI will re-run._"
    CMD
    working_directory = var.project_dir
  }
  outcome "success" { next = "count_review_attempt" }
  outcome "failure" { next = "count_review_attempt" }
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
    command           = "set -eu; branch=$(basename \"${var.workstream_file}\" .md); if git show-ref --verify --quiet refs/remotes/origin/$branch; then echo \"remote_branch_still_exists=$branch (gh pr merge --delete-branch may have skipped it)\" >&2; fi; echo \"${var.base_branch}_at=$(git rev-parse HEAD)\"; echo \"origin_${var.base_branch}_at=$(git rev-parse origin/${var.base_branch})\""
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
  default { next = step.pr_status }
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
