# mode: standalone (uses agent adapters; server not required)
#
# Workstream Review Loop
# ======================
# Runs a two-agent review loop against a single workstream file, then opens a
# PR, performs a cold review, and merges to the integration branch once a human
# approves on GitHub.
#
# Pass the target file via the workstream_file variable.
#
#   executor     — implements workstream tasks in focused passes
#   reviewer     — reviews executor changes for correctness and completeness
#   cold_reviewer — post-implementation cold PR review (external perspective)
#
# Loop mechanics:
#   • Executor and reviewer iterate until the reviewer is satisfied.
#   • Once approved, reviewer hands back to executor for a final commit pass.
#   • After commit, a PR is opened, CI warmup runs, then pr_status_check gates.
#   • cold_reviewer performs a proactive review and posts a recommendation.
#   • await_github_approval polls GitHub every 2 minutes until APPROVED.
#   • On APPROVED, the PR is squash-merged and base_branch is synced.
#
# Usage (run once per workstream file):
#   CRITERIA_WORKFLOW_ALLOWED_PATHS=.github/agents:workstreams \
#     bin/criteria apply examples/archived/workstream_review_loop --var workstream_file=workstreams/adapter_v2/WS03-host-v2-wire.md
#
# For post-release workstreams (WS41+) that target main:
#   bin/criteria apply examples/archived/workstream_review_loop \
#     --var workstream_file=workstreams/adapter_v2/WS41-extract-adapter-proto-repo.md \
#     --var base_branch=main \
#     --var require_workflow_approval=true

workflow {

  name = "workstream_review_loop"
  version       = "1"
  initial_state = "checkout_branch"
  target_state  = "done"
  policy {
    max_total_steps = 200
  }
}


variable "workstream_file" {
  type = string
  default     = "workstreams/adapter_v2/WS03-host-v2-wire.md"
  description = "Path to the workstream file to process."
}

variable "base_branch" {
  type = string
  default     = "adapter-v2"
  description = "Integration branch this workstream's PR targets. Use 'main' for post-release workstreams (WS41+)."
}

variable "require_workflow_approval" {
  type = string
  default     = "false"
  description = "Set to 'true' to require explicit workflow-node approval before merge. Default 'false' uses async GitHub approval polling — no babysitting needed."
}

# ── Shared state for reason-passing between loop steps ───────────────────────
# Instead of re-reading the workstream file on every loop iteration (which
# causes context corruption as agents see stale vs. current file content),
# each step writes a concise targeted summary into these shared variables via
# submit_outcome reason. The next step receives only the targeted delta.
data "internal" "last_review_reason" {
  type = string
  value = ""
}
data "internal" "last_execute_reason" {
  type = string
  value = ""
}

# ── Adapters ─────────────────────────────────────────────────────────────────

adapter "copilot" "executor" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "high"
    max_turns        = 12
    system_prompt    = trimfrontmatter(file("../../.github/agents/workstream-executor.agent.md"))
  }
}

adapter "copilot" "reviewer" {
  config {
    model            = "gpt-5.4"
    reasoning_effort = "high"
    max_turns        = 10
    system_prompt    = trimfrontmatter(file("../../.github/agents/workstream-reviewer.agent.md"))
  }
}

adapter "copilot" "pr_manager" {
  config {
    model         = "claude-haiku-4.5"
    max_turns     = 10
    system_prompt = trimfrontmatter(file("../../.github/agents/workstream-pr-manager.agent.md"))
  }
}

adapter "copilot" "cold_reviewer" {
  config {
    model            = "gpt-5.5"
    reasoning_effort = "high"
    max_turns        = 15
    system_prompt    = trimfrontmatter(file("../../.criteria/workflows/pr_review/agents/pr_reviewer.agent.md"))
  }
}

adapter "shell" "default" {
  config { }
}

# ── Branch checkout ───────────────────────────────────────────────────────────

step "checkout_branch" {
  target = adapter.shell.default
  input {
    command = "BASE_BRANCH='${var.base_branch}' sh .criteria/workflows/bootstrap/scripts/prepare-workstream-branch.sh '${var.workstream_file}'"
  }
  timeout = "30s"
  outcome "success" { next = switch.route_branch_state }
  outcome "failure" { next = state.failed }
}

switch "route_branch_state" {
  condition {
    match = steps.checkout_branch.stdout == "already_merged"
    next = state.done
  }
  default { next = step.execute_init }
}

# ── Init pass: bootstrap agent context ───────────────────────────────────────
# Each agent reads the workstream file ONCE here to establish context. That
# context persists in the live session for all subsequent loop turns.
# Loop steps pass targeted feedback via submit_outcome reason (stored in
# shared variables) instead of asking agents to re-read the workstream file,
# which causes context corruption when agents see stale vs. current content.

step "execute_init" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file} for the full task scope.\n\nExecute the first implementation batch: complete the next unchecked items, write code and tests as needed, keep changes scoped and verifiable. Record your progress in ${var.workstream_file}.\n\nIn the submit_outcome reason, include a brief summary of what you implemented (specific file paths and what was added/changed). This summary is passed directly to the reviewer — keep it targeted.\n\nOutcomes: needs_review, failure"
  }
  outcome "needs_review" {
    next = step.review_init
      write {
    target = data.internal.last_execute_reason.value
    value  = output.reason
  }
  }
  outcome "needs_approval" {
    next = step.review_init
      write {
    target = data.internal.last_execute_reason.value
    value  = output.reason
  }
  }
  outcome "failure" { next = state.failed }
}

step "review_init" {
  target = adapter.copilot.reviewer
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file} for the workstream scope. The executor's first pass summary:\n\n${data.internal.last_execute_reason.value}\n\nReview the executor's changes against the acceptance bar. Write full findings into the reviewer notes section of ${var.workstream_file}.\n\nIn the submit_outcome reason, include a concise actionable list of must-fix items (if requesting changes), or a brief approval confirmation. This is passed directly to the executor — keep it targeted and specific (file:line where relevant).\n\nOutcomes: approved, changes_requested, failure"
  }
  outcome "approved" { next = step.commit_and_prepare_pr }
  outcome "changes_requested" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "needs_review" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "needs_approval" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "failure" { next = state.failed }
}

# ── Review loop: reason-passing prompts ──────────────────────────────────────
# Agent context is established from the init pass. These steps pass targeted
# feedback between agents via data.internal.last_review_reason.value / last_execute_reason
# rather than directing agents to re-read the workstream file.

step "execute" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Reviewer requested changes:\n\n${data.internal.last_review_reason.value}\n\nAddress each finding. In the submit_outcome reason, briefly summarize the specific changes you made (file:line and what changed). This is passed directly to the reviewer.\n\nOutcomes: needs_review, failure"
  }
  outcome "success" {
    next = step.verify
      write {
    target = data.internal.last_execute_reason.value
    value  = output.reason
  }
  }
  outcome "needs_review" {
    next = step.verify
      write {
    target = data.internal.last_execute_reason.value
    value  = output.reason
  }
  }
  outcome "needs_approval" {
    next = step.verify
      write {
    target = data.internal.last_execute_reason.value
    value  = output.reason
  }
  }
  outcome "failure" { next = state.failed }
}

step "verify" {
  target = adapter.shell.default
  input {
    command = "make ci 2>&1"
  }
  timeout = "120s"
  outcome "success" { next = step.review }
  outcome "failure" { next = step.fix_verify }
}

step "fix_verify" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Build/test verification failed. Fix all failures before this goes to review.\n\n--- verify output ---\n${steps.verify.stdout}\n--- end ---"
  }
  outcome "needs_review"   { next = step.verify }
  outcome "needs_approval" { next = step.verify }
  outcome "failure"        { next = state.failed }
}

step "review" {
  target = adapter.copilot.reviewer
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Executor addressed your findings. Changes made:\n\n${data.internal.last_execute_reason.value}\n\nVerify these changes are correct and complete. In the submit_outcome reason, include a concise list of remaining must-fix items (if requesting changes) or a brief approval confirmation.\n\nOutcomes: approved, changes_requested, failure"
  }
  outcome "approved" { next = step.commit_and_prepare_pr }
  outcome "changes_requested" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "needs_review" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "needs_approval" {
    next = step.execute
      write {
    target = data.internal.last_review_reason.value
    value  = output.reason
  }
  }
  outcome "failure" { next = state.failed }
}

# ── Finalize: executor commit ─────────────────────────────────────────────────

step "commit_and_prepare_pr" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Approved. Commit all workstream changes with message:\nworkstream: complete ${var.workstream_file}\n\nEnd your final line with exactly one of:\nRESULT: success\nRESULT: failure"
  }
  outcome "success" { next = step.open_or_update_pr }
  outcome "failure" { next = state.failed }
}

# ── PR automation ─────────────────────────────────────────────────────────────

step "open_or_update_pr" {
  target = adapter.copilot.pr_manager
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file}. Ensure branch is pushed (BASE_BRANCH=${var.base_branch}), then create or update the PR from the current branch to ${var.base_branch}.\n\nInclude a concise summary and test evidence from the workstream notes/reviewer notes. Use: BASE_BRANCH='${var.base_branch}' sh .criteria/workflows/pr_review/scripts/open-or-update-pr.sh '${var.workstream_file}'\n\nEnd your final line with exactly one of:\nRESULT: watch_pr\nRESULT: failure"
  }
  outcome "watch_pr"       { next = step.watch_pr_warmup }
  outcome "needs_review"   { next = step.watch_pr_warmup }
  outcome "needs_approval" { next = step.watch_pr_warmup }
  outcome "failure"        { next = state.failed }
}

step "watch_pr_warmup" {
  target = adapter.shell.default
  input {
    command = "echo 'warming up CI before first status poll (90s)'; sleep 90"
  }
  timeout = "3m"
  outcome "success" { next = step.pr_status_check }
  outcome "failure" { next = step.pr_status_check }
}

# ── Deterministic PR status gate ──────────────────────────────────────────────

step "pr_status_check" {
  target = adapter.shell.default
  input {
    command = "sh .criteria/workflows/pr_review/scripts/pr-status.sh"
  }
  timeout = "120s"
  outcome "success" { next = switch.route_pr_status }
  outcome "failure" { next = state.failed }
}

switch "route_pr_status" {
  condition {
    match = steps.pr_status_check.stdout == "merged"
    next = step.sync_base
  }
  condition {
    match = steps.pr_status_check.stdout == "ready"
    next = step.cold_review
  }
  condition {
    match = steps.pr_status_check.stdout == "threads_open"
    next = step.cold_review
  }
  condition {
    match = steps.pr_status_check.stdout == "pending"
    next = step.pr_backoff
  }
  condition {
    match = steps.pr_status_check.stdout == "changes_requested"
    next = step.execute_pr_feedback
  }
  condition {
    match = steps.pr_status_check.stdout == "checks_failed"
    next = state.failed
  }
  default { next = state.failed }
}

step "pr_backoff" {
  target = adapter.shell.default
  input {
    command = "echo 'CI still pending; sleeping 60s before re-poll'; sleep 60"
  }
  timeout = "3m"
  outcome "success" { next = step.pr_status_check }
  outcome "failure" { next = step.pr_status_check }
}

# ── Cold PR review ────────────────────────────────────────────────────────────
# External-perspective review before requesting human GitHub approval.
# Posts a recommendation comment; cannot approve or merge directly.

step "cold_review" {
  target = adapter.copilot.cold_reviewer
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Review the open PR for ${var.workstream_file}. PR status gate emitted: `${steps.pr_status_check.stdout}`\n\nContext from pr-status.sh:\n--- stderr ---\n${steps.pr_status_check.stderr}\n--- end ---\n\nFor each unresolved (and !outdated) review thread, either reply with citation evidence and resolve via `sh .criteria/workflows/pr_review/scripts/resolve-thread.sh <thread_id>`, or leave it open and request changes.\n\nIf the diff meets the bar and all addressable threads are resolved: post a recommendation comment via `gh pr comment <pr_number> --body \"<your summary>\"` summarizing what you verified and that you recommend approval. Then emit RESULT: approve.\n\nDO NOT run `gh pr review --approve` — branch protection forbids self-approval.\nDO NOT run `gh pr merge` — the workflow handles merge after human approval.\n\nEnd your final message with exactly one of:\nRESULT: approve\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approve"           { next = switch.route_after_cold_review }
  outcome "changes_requested" { next = step.execute_pr_feedback }
  outcome "failure"           { next = state.failed }
}

# ── Approval routing ──────────────────────────────────────────────────────────

switch "route_after_cold_review" {
  condition {
    match = var.require_workflow_approval == "true"
    next = approval.human_approval_required
  }
  default { next = step.await_github_approval }
}

approval "human_approval_required" {
  approvers = ["operator"]
  reason    = "The cold reviewer recommends approval and has posted a summary comment on the PR. Go to GitHub, review the comment, click Approve on the PR, then approve this node."
  outcome "approved" { next = step.await_github_approval }
  outcome "rejected" { next = state.failed }
}

# ── Async GitHub approval poll ────────────────────────────────────────────────
# The cold reviewer has posted its recommendation. Just click Approve on GitHub
# whenever you're ready — no workflow babysitting needed.

step "await_github_approval" {
  target = adapter.shell.default
  input {
    command = "set -eu; branch=$(git branch --show-current); pr_num=$(gh pr view \"$branch\" --json number --jq '.number'); decision=$(gh pr view \"$pr_num\" --json reviewDecision --jq '.reviewDecision // \"NONE\"'); echo \"review_decision=$decision\"; if [ \"$decision\" = \"APPROVED\" ]; then exit 0; fi; echo 'Waiting for human to click Approve on GitHub...'; exit 1"
  }
  timeout = "5m"
  outcome "success" { next = step.merge_pr_and_sync_base }
  outcome "failure" { next = step.backoff_await_approval }
}

step "backoff_await_approval" {
  target = adapter.shell.default
  input {
    command = "echo 'not yet approved; sleeping 120s'; sleep 120"
  }
  timeout = "3m"
  outcome "success" { next = step.await_github_approval }
  outcome "failure" { next = step.await_github_approval }
}

# ── PR feedback from human reviewers ─────────────────────────────────────────

step "execute_pr_feedback" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "PR requires code changes from review comments or failed checks.\n\nPR status context:\n--- pr_status_check stderr ---\n${steps.pr_status_check.stderr}\n--- end ---\n\nFor every unresolved (and !outdated) review thread that requires a code change:\n  1. Implement the fix.\n  2. Update ${var.workstream_file} notes with the remediation.\n  3. Commit and push.\n  4. Reply on the thread citing the fix (commit SHA + file:line) and resolve via: gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -f id=<thread_id>\n\nEnd your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "success"        { next = step.verify }
  outcome "needs_review"   { next = step.verify }
  outcome "needs_approval" { next = step.verify }
  outcome "failure"        { next = state.failed }
}

# ── Merge and sync ────────────────────────────────────────────────────────────

step "merge_pr_and_sync_base" {
  target = adapter.shell.default
  input {
    command = "set -uo pipefail; exec 2>&1; branch=$(git branch --show-current); pr_state=''; pr_number=''; if [ -n \"$branch\" ] && [ \"$branch\" != '${var.base_branch}' ]; then pr_view=$(gh pr view \"$branch\" --json number,state 2>/dev/null || true); if [ -n \"$pr_view\" ]; then pr_number=$(printf '%s' \"$pr_view\" | jq -r '.number // empty'); pr_state=$(printf '%s' \"$pr_view\" | jq -r '.state // empty'); fi; fi; echo \"branch=$branch pr_number=${pr_number:-unknown} pr_state=${pr_state:-unknown}\"; if [ -n \"$pr_number\" ] && [ \"$pr_state\" != 'MERGED' ] && [ \"$pr_state\" != 'CLOSED' ]; then gh pr merge \"$pr_number\" --squash --delete-branch || { echo 'merge command failed'; exit 1; }; else echo 'skip_merge=true'; fi; git fetch origin '${var.base_branch}' || exit 1; git checkout '${var.base_branch}' || exit 1; git pull --ff-only origin '${var.base_branch}' || exit 1; echo \"synced_base=${var.base_branch} merged_pr=${pr_number:-unknown}\"; exit 0"
  }
  timeout = "5m"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

step "sync_base" {
  target = adapter.shell.default
  input {
    command = "set -eu; git fetch origin '${var.base_branch}'; git checkout '${var.base_branch}'; git pull --ff-only origin '${var.base_branch}'; echo synced_base='${var.base_branch}'"
  }
  timeout = "2m"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

# ── Terminal states ───────────────────────────────────────────────────────────

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
