# mode: standalone (uses agent adapters; server not required)
#
# Workstream Review Loop
# ======================
# Runs a two-agent review loop against a single workstream file.
# Pass the target file via the workstream_file variable default, or override
# it by editing the default value before running.
#
#   executor  — implements workstream tasks in focused passes
#   reviewer  — reviews executor changes for correctness and completeness
#
# Loop mechanics:
#   • Executor and reviewer iterate until the reviewer is satisfied.
#   • Once approved, reviewer hands back to executor for a final commit pass.
#   • After commit success, the workflow closes both sessions and ends.
#
# Usage (run once per workstream file):
#   CRITERIA_WORKFLOW_ALLOWED_PATHS=.github/agents:workstreams \
#     bin/criteria apply examples/workstream_review_loop.hcl
#
# The allowed-paths env var lets the file() expression function read the agent
# profile markdown files in .github/agents/ (outside this workflow's directory).
# Profiles are loaded into each agent's system_prompt at compile time.
#
# Note: for_each multi-step agent chains are not supported by the engine —
# the do-step must return _continue to advance the loop. Use this single-file
# pattern and invoke once per workstream instead.

workflow "workstream_review_loop" {
  version       = "1"
  initial_state = "checkout_branch"
  target_state  = "done"

  policy {
    max_total_steps = 120  # caps execute/review/pr loops; fails safely if automation cannot converge
  }
}

variable "workstream_file" {
  type        = "string"
  default     = "workstreams/05-shell-adapter-sandbox.md"
  description = "Path to the workstream file to process. Change default or re-run for each file."
}

# ── Adapters ────────────────────────────────────────────────────────────────

# Adapter profile markdowns are loaded into each session via system_prompt at
# compile time (file() + trimfrontmatter()). The profile is established at
# session-open and persists for every subsequent turn — step prompts can be
# short coordination signals.

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

adapter "shell" "default" {
  config { }
}

step "checkout_branch" {
  target = adapter.shell.default
  input {
    command = "branch=$(basename '${var.workstream_file}' .md) && current=$(git branch --show-current) && if [ \"$current\" = \"main\" ]; then git checkout -b \"$branch\"; else echo \"already on branch: $current\"; fi"
  }
  timeout = "10s"
  outcome "success" { next = "execute_init" }
  outcome "failure" { next = "failed" }
}

# ── Init pass: bootstrap agent context ─────────────────────────────────────
# Each agent reads its own profile and the workstream file on its first turn.
# That context persists in the live session for all subsequent loop turns.

step "execute_init" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file} for the full task scope.\n\nExecute the first implementation batch: complete the next unchecked items, write code and tests as needed, keep changes scoped and verifiable. Record your progress and notes in ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
  }
  outcome "needs_review"   { next = "review_init" }
  outcome "needs_approval" { next = "review_init" }
  outcome "failure"        { next = "failed" }
}

step "review_init" {
  target = adapter.copilot.reviewer
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file} for the workstream scope and the executor's latest work.\n\nReview the executor's changes against the acceptance bar. Write all findings and your verdict into the reviewer notes section of ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
  }
  outcome "approved"          { next = "commit_and_prepare_pr" }
  outcome "changes_requested" { next = "execute" }
  outcome "needs_review"      { next = "execute" }
  outcome "needs_approval"    { next = "execute" }
  outcome "failure"           { next = "failed" }
}

# ── Review loop: minimal signal prompts ─────────────────────────────────────
# Agent context is fully established after the init pass.
# These prompts are coordination signals only — not instructions.

step "execute" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Reviewer requested changes. Notes are in ${var.workstream_file}."
  }
  outcome "success"        { next = "verify" }
  outcome "needs_review"   { next = "verify" }
  outcome "needs_approval" { next = "verify" }
  outcome "failure"        { next = "failed" }
}

step "verify" {
  target = adapter.shell.default
  input {
    command = "make ci 2>&1"
  }
  timeout = "120s"
  outcome "success" { next = "review" }
  outcome "failure" { next = "fix_verify" }
}

step "fix_verify" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Build/test verification failed. Fix all failures before this goes to review.\n\n--- verify output ---\n${steps.verify.stdout}\n--- end ---"
  }
  outcome "needs_review"   { next = "verify" }
  outcome "needs_approval" { next = "verify" }
  outcome "failure"        { next = "failed" }
}

step "review" {
  target = adapter.copilot.reviewer
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Ready for review. Latest work is in ${var.workstream_file}."
  }
  outcome "approved"          { next = "commit_and_prepare_pr" }
  outcome "changes_requested" { next = "execute" }
  outcome "needs_review"      { next = "execute" }
  outcome "needs_approval"    { next = "execute" }
  outcome "failure"           { next = "failed" }
}

# ── Finalize: executor commit ──────────────────────────────────────────────

step "commit_and_prepare_pr" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Approved. Commit all workstream changes with message:\nworkstream: complete ${var.workstream_file}\n\nEnd your final line with exactly one of:\nRESULT: success\nRESULT: failure"
  }
  outcome "success" { next = "open_or_update_pr" }
  outcome "failure" { next = "failed" }
}

# ── PR automation loop ────────────────────────────────────────────────────
# PR manager owns creation/updates and comment replies.
# Shell step blocks on required checks and returns gate status.

step "open_or_update_pr" {
  target = adapter.copilot.pr_manager
  allow_tools = [
    "*",
  ]
  input {
    prompt = "Read ${var.workstream_file}. Ensure branch is pushed, then create or update the PR from the current branch to main.\n\nInclude a concise summary and test evidence from the workstream notes/reviewer notes.\n\nEnd your final line with exactly one of:\nRESULT: watch_pr\nRESULT: failure"
  }
  outcome "watch_pr"       { next = "watch_pr_warmup" }
  outcome "needs_review"   { next = "watch_pr_warmup" }
  outcome "needs_approval" { next = "watch_pr_warmup" }
  outcome "failure"        { next = "failed" }
}

step "watch_pr_warmup" {
  target = adapter.shell.default
  input {
    command = "set -euo pipefail; branch=$(git branch --show-current | tr '/ ' '__'); mkdir -p .criteria/tmp; echo 0 > .criteria/tmp/pr_watch_backoff_$branch.txt; echo 'warming up CI checks before first poll (90s)'; sleep 90"
  }
  timeout = "3m"
  outcome "success" { next = "watch_pr_gate" }
  outcome "failure" { next = "triage_pr_feedback" }
}

step "watch_pr_backoff" {
  target = adapter.shell.default
  input {
    command = "set -euo pipefail; branch=$(git branch --show-current | tr '/ ' '__'); mkdir -p .criteria/tmp; state=.criteria/tmp/pr_watch_backoff_$branch.txt; attempt=0; if [ -f \"$state\" ]; then attempt=$(cat \"$state\" 2>/dev/null || echo 0); fi; attempt=$((attempt + 1)); echo \"$attempt\" > \"$state\"; if [ \"$attempt\" -le 1 ]; then delay=20; elif [ \"$attempt\" -le 2 ]; then delay=40; elif [ \"$attempt\" -le 3 ]; then delay=80; elif [ \"$attempt\" -le 4 ]; then delay=120; else delay=180; fi; echo \"backoff_attempt=$attempt\"; echo \"sleep_seconds=$delay\"; sleep \"$delay\""
  }
  timeout = "5m"
  outcome "success" { next = "watch_pr_gate" }
  outcome "failure" { next = "triage_pr_feedback" }
}

step "watch_pr_gate" {
  target = adapter.shell.default
  input {
    command = "set -euo pipefail; exec 2>&1; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); echo \"pr_number=$pr_number\"; pr_state=$(gh pr view \"$pr_number\" --json state --jq '.state'); echo \"pr_state=$pr_state\"; if [ \"$pr_state\" = \"MERGED\" ]; then echo \"checks=already_merged\"; echo \"ready_to_merge=true\"; exit 0; fi; checks_rc=0; checks_json=$(gh pr checks \"$pr_number\" --required --json bucket,name,state,workflow 2>&1) || checks_rc=$?; if [ \"$checks_rc\" -eq 8 ]; then echo \"checks=pending\"; printf '%s\n' \"$checks_json\" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join(\"=\")) | .[]'; exit 1; fi; if [ \"$checks_rc\" -ne 0 ]; then echo \"checks=failed\"; printf '%s\n' \"$checks_json\"; exit 1; fi; echo \"checks=passed\"; printf '%s\n' \"$checks_json\" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join(\"=\")) | .[]'; owner=$(gh repo view --json owner --jq '.owner.login'); repo=$(gh repo view --json name --jq '.name'); review_decision=$(gh pr view \"$pr_number\" --json reviewDecision --jq '.reviewDecision // \"REVIEW_REQUIRED\"'); review_threads_json=$(gh api graphql -f query='query($owner:String!, $repo:String!, $number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){totalCount pageInfo{hasNextPage endCursor} nodes{isResolved isOutdated}}}}}' -f owner=\"$owner\" -f repo=\"$repo\" -F number=\"$pr_number\"); review_threads_total=$(printf '%s' \"$review_threads_json\" | jq -r '.data.repository.pullRequest.reviewThreads.totalCount'); review_threads_has_next_page=$(printf '%s' \"$review_threads_json\" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage'); unresolved_threads=$(printf '%s' \"$review_threads_json\" | jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isOutdated|not) and (.isResolved|not))] | length'); echo \"review_decision=$review_decision\"; echo \"review_threads_total=$review_threads_total\"; echo \"review_threads_has_next_page=$review_threads_has_next_page\"; echo \"unresolved_threads=$unresolved_threads\"; if [ \"$review_decision\" = \"APPROVED\" ] && [ \"$review_threads_has_next_page\" = \"false\" ] && [ \"$unresolved_threads\" -eq 0 ]; then echo \"ready_to_merge=true\"; exit 0; fi; if [ \"$review_threads_has_next_page\" = \"true\" ]; then echo \"review_threads_complete=false\"; fi; echo \"ready_to_merge=false\"; exit 1"
  }
  timeout = "45m"
  outcome "success" { next = "merge_pr_and_sync_main" }
  outcome "failure" { next = "triage_pr_feedback" }
}

step "triage_pr_feedback" {
  target = adapter.copilot.pr_manager
  allow_tools = [
    "*",
  ]
  input {
    prompt = "PR watch gate reported unresolved feedback or failed checks.\n\nUse this gate output as context:\n--- watch_pr_gate output ---\n${steps.watch_pr_gate.stdout}\n--- end ---\n\nHARD RULES:\n1. DO NOT run `gh pr merge` — the workflow's merge_pr_and_sync_main step owns merging. Self-merging breaks the workflow and bypasses required-resolution policy.\n2. The repository requires every review thread to be resolved before merge. You MUST drive every unresolved (and not-outdated) thread to a resolved state.\n\nFirst: `gh pr view <num> --json state` — if state is MERGED, return RESULT: merged immediately.\n\nOtherwise enumerate every review thread via the GraphQL API (reviewThreads.nodes) and process each one where isResolved=false AND isOutdated=false:\n  • If the comment is already addressed by code on the branch or by reviewer notes in the workstream file: reply on the thread with concrete evidence (commit SHA, file:line, or quoted reviewer note) and resolve the thread (resolveReviewThread mutation, or `gh api graphql` with resolveReviewThread).\n  • If the comment requires NEW code changes you cannot resolve by citation: leave the thread unresolved, return RESULT: needs_executor so the executor can fix it. Do not resolve threads you have not addressed.\n  • If a check (CI) failed: investigate via `gh pr checks` / `gh run view`. Reply on related threads with the diagnosis. If a code fix is needed, return RESULT: needs_executor.\n\nAfter processing, re-query reviewThreads to confirm zero unresolved+not-outdated threads remain before returning recheck.\n\nReturn values:\n  RESULT: merged          — PR is already MERGED on GitHub.\n  RESULT: needs_executor  — code changes are required (unresolved threads remain that need fixes, or checks failed needing a fix).\n  RESULT: recheck         — you replied to and resolved every addressable thread; gate should re-poll after backoff.\n  RESULT: watch_pr        — checks still running, no review action available yet.\n  RESULT: failure         — unrecoverable error.\n\nEnd your final line with exactly one of:\nRESULT: merged\nRESULT: needs_executor\nRESULT: recheck\nRESULT: watch_pr\nRESULT: failure"
  }
  outcome "merged"         { next = "merge_pr_and_sync_main" }
  outcome "needs_executor" { next = "execute_pr_feedback" }
  outcome "recheck"        { next = "watch_pr_backoff" }
  outcome "watch_pr"       { next = "watch_pr_backoff" }
  outcome "needs_review"   { next = "watch_pr_backoff" }
  outcome "needs_approval" { next = "watch_pr_backoff" }
  outcome "failure"        { next = "failed" }
}

step "execute_pr_feedback" {
  target = adapter.copilot.executor
  allow_tools = [
    "*",
  ]
  input {
    prompt = "PR manager determined code changes are required from review comments or check failures.\n\nUse this gate output as context:\n--- watch_pr_gate output ---\n${steps.watch_pr_gate.stdout}\n--- end ---\n\nFor every unresolved (and not-outdated) review thread that requires a code change:\n  1. Implement the fix.\n  2. Update ${var.workstream_file} notes with the remediation.\n  3. Commit and push.\n  4. Reply on the thread citing the fix (commit SHA + file:line) and resolve the thread via the GraphQL resolveReviewThread mutation (`gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -f id=<thread_id>`).\n\nThe repository requires zero unresolved threads before merge. Do not leave any addressed thread unresolved. Do not resolve threads you have not actually addressed."
  }
  outcome "success"        { next = "verify" }
  outcome "needs_review"   { next = "verify" }
  outcome "needs_approval" { next = "verify" }
  outcome "failure"        { next = "failed" }
}

step "merge_pr_and_sync_main" {
  target = adapter.shell.default
  input {
    command = "set -uo pipefail; exec 2>&1; branch=$(git branch --show-current); pr_state=\"\"; pr_number=\"\"; if [ -n \"$branch\" ] && [ \"$branch\" != \"main\" ]; then pr_view=$(gh pr view \"$branch\" --json number,state 2>/dev/null || true); if [ -n \"$pr_view\" ]; then pr_number=$(printf '%s' \"$pr_view\" | jq -r '.number // empty'); pr_state=$(printf '%s' \"$pr_view\" | jq -r '.state // empty'); fi; fi; echo \"branch=$branch pr_number=$${pr_number:-unknown} pr_state=$${pr_state:-unknown}\"; if [ -n \"$pr_number\" ] && [ \"$pr_state\" != \"MERGED\" ] && [ \"$pr_state\" != \"CLOSED\" ]; then gh pr merge \"$pr_number\" --squash --delete-branch || { echo 'merge command failed'; exit 1; }; else echo 'skip_merge=true'; fi; git fetch origin main || exit 1; git checkout main || exit 1; git pull --ff-only origin main || exit 1; echo \"synced_main=true merged_pr=$${pr_number:-unknown}\"; exit 0"
  }
  timeout = "5m"
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}

# ── Terminal states ────────────────────────────────────────────────────────

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
