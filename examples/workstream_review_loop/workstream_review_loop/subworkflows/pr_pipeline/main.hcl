# PR Pipeline subworkflow
# ======================
# Manages the full PR lifecycle: creation, granular CI/comment/merge checks,
# feedback triage, and merge. Bounded to max_pr_cycles (default 3).
#
# Granular check types (each is a separate shell step with exit-code routing):
#   1. check_ci_status   — CI actions: pending→backoff, failed→check threads, passed→check threads
#   2. check_pr_comments  — review threads: unresolved→triage, clear→check merge
#   3. check_merge_readiness — review decision + merge state: ready→merge, not ready→backoff
#
# Adapters are isolated from the parent and execute-review subworkflow.

workflow "pr_pipeline" {
  version       = "1"
  initial_state = "open_or_update_pr"
  target_state  = "merged"
}

variable "workstream_file" {
  type = "string"
}

variable "max_pr_cycles" {
  type    = "number"
  default = 3
  description = "Maximum PR triage cycles before requesting user assistance."
}

shared_variable "pr_cycle_count" {
  type  = "number"
  value = 0
}

adapter "copilot" "pr_manager" {
  config {
    model         = "auto"
    max_turns     = 10
    system_prompt = trimfrontmatter(file("agents/workstream-pr-manager.agent.md"))
  }
}

adapter "copilot" "executor" {
  config {
    model            = "claude-sonnet-4.6"
    reasoning_effort = "high"
    max_turns        = 12
    system_prompt    = trimfrontmatter(file("agents/workstream-executor.agent.md"))
  }
}

adapter "shell" "default" {
  config { }
}

# ── Open or update PR ────────────────────────────────────────────────────────

step "open_or_update_pr" {
  target = adapter.copilot.pr_manager
  allow_tools = ["*"]
  input {
    prompt = "Read ${var.workstream_file}. Ensure branch is pushed, then create or update the PR from the current branch to main.\n\nInclude a concise summary and test evidence from the workstream notes/reviewer notes.\n\nEnd your final line with exactly one of:\nRESULT: watch_pr\nRESULT: failure"
  }
  outcome "watch_pr"       { next = "warmup_ci" }
  outcome "needs_review"   { next = "warmup_ci" }
  outcome "needs_approval" { next = "warmup_ci" }
  outcome "failure"        { next = "failed" }
}

step "warmup_ci" {
  target = adapter.shell.default
  input {
    command = "set -euo pipefail; branch=$(git branch --show-current | tr '/ ' '__'); mkdir -p .criteria/tmp; echo 0 > .criteria/tmp/pr_watch_backoff_$branch.txt; echo 'warming up CI checks before first poll (90s)'; sleep 90"
  }
  timeout = "3m"
  outcome "success" { next = "check_ci_status" }
  outcome "failure" { next = "check_ci_status" }
}

# ── Granular check: CI actions status ────────────────────────────────────────
#
# Exit codes: 0=pending (backoff and recheck), 1=failed (proceed to check
# threads for full triage), 2=passed or already merged (proceed to check
# threads or merge).

step "check_ci_status" {
  target = adapter.shell.default
  input {
    command = <<-SHELL
      set -euo pipefail; exec 2>&1
      branch=$(git branch --show-current)
      pr_number=$(gh pr view "$branch" --json number --jq '.number')
      echo "pr_number=$pr_number"
      pr_state=$(gh pr view "$pr_number" --json state --jq '.state')
      echo "pr_state=$pr_state"
      if [ "$pr_state" = "MERGED" ]; then echo "already merged"; exit 2; fi
      checks_rc=0
      checks_json=$(gh pr checks "$pr_number" --required --json bucket,name,state,workflow 2>&1) || checks_rc=$?
      if [ "$checks_rc" -eq 8 ]; then
        echo "CI pending"
        printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]'
        exit 0
      fi
      if [ "$checks_rc" -ne 0 ]; then
        echo "CI failed"
        printf '%s\n' "$checks_json"
        exit 1
      fi
      echo "CI passed"
      printf '%s\n' "$checks_json" | jq -r 'group_by(.bucket) | map([.[0].bucket, (length|tostring)] | join("=")) | .[]'
      exit 2
    SHELL
  }
  timeout = "45m"
  outcome "success" { next = "route_ci_status" }
  outcome "failure" { next = "route_ci_status" }
}

switch "route_ci_status" {
  condition {
    match = steps.check_ci_status.exit_code == "0"
    next  = state.backoff_ci
  }
  condition {
    match = steps.check_ci_status.exit_code == "1"
    next  = state.check_pr_comments
  }
  default {
    next = state.check_pr_comments
  }
}

step "backoff_ci" {
  target = adapter.shell.default
  input {
    command = <<-SHELL
      set -euo pipefail
      branch=$(git branch --show-current | tr '/ ' '__')
      mkdir -p .criteria/tmp
      state=.criteria/tmp/pr_watch_backoff_$branch.txt
      attempt=0
      if [ -f "$state" ]; then attempt=$(cat "$state" 2>/dev/null || echo 0); fi
      attempt=$((attempt + 1))
      echo "$attempt" > "$state"
      if [ "$attempt" -le 1 ]; then delay=20
      elif [ "$attempt" -le 2 ]; then delay=40
      elif [ "$attempt" -le 3 ]; then delay=80
      elif [ "$attempt" -le 4 ]; then delay=120
      else delay=180
      fi
      echo "backoff_attempt=$attempt"
      echo "sleep_seconds=$delay"
      sleep "$delay"
    SHELL
  }
  timeout = "5m"
  outcome "success" { next = "check_ci_status" }
  outcome "failure" { next = "check_ci_status" }
}

# ── Granular check: PR review threads ────────────────────────────────────────
#
# Exit codes: 0=unresolved threads exist (triage needed), 1=clear (no
# unresolved threads, proceed to merge readiness check).

step "check_pr_comments" {
  target = adapter.shell.default
  input {
    command = <<-SHELL
      set -euo pipefail; exec 2>&1
      branch=$(git branch --show-current)
      pr_number=$(gh pr view "$branch" --json number --jq '.number')
      echo "pr_number=$pr_number"
      owner=$(gh repo view --json owner --jq '.owner.login')
      repo=$(gh repo view --json name --jq '.name')
      review_threads_json=$(gh api graphql -f query='query($owner:String!, $repo:String!, $number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){totalCount pageInfo{hasNextPage endCursor} nodes{isResolved isOutdated comments(first:1){nodes{author{login}}}}}}}' -f owner="$owner" -f repo="$repo" -F number="$pr_number")
      unresolved_threads=$(printf '%s' "$review_threads_json" | jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isOutdated|not) and (.isResolved|not))] | length')
      echo "unresolved_count=$unresolved_threads"
      if [ "$unresolved_threads" -eq 0 ]; then
        echo "thread_status=clear"
        exit 1
      fi
      echo "thread_status=unresolved"
      exit 0
    SHELL
  }
  timeout = "30s"
  outcome "success" { next = "route_pr_comments" }
  outcome "failure" { next = "route_pr_comments" }
}

switch "route_pr_comments" {
  condition {
    match = steps.check_pr_comments.exit_code == "0"
    next  = state.count_pr_cycle
  }
  default {
    next = state.check_merge_readiness
  }
}

# ── Granular check: merge readiness ───────────────────────────────────────────
#
# Exit codes: 0=ready to merge, 1=not ready (backoff and recheck),
# 2=already merged (proceed to merge step).

step "check_merge_readiness" {
  target = adapter.shell.default
  input {
    command = <<-SHELL
      set -euo pipefail; exec 2>&1
      branch=$(git branch --show-current)
      pr_number=$(gh pr view "$branch" --json number --jq '.number')
      pr_state=$(gh pr view "$pr_number" --json state --jq '.state')
      echo "pr_state=$pr_state"
      if [ "$pr_state" = "MERGED" ]; then
        echo "already_merged=true"
        exit 2
      fi
      review_decision=$(gh pr view "$pr_number" --json reviewDecision --jq '.reviewDecision // "REVIEW_REQUIRED"')
      echo "review_decision=$review_decision"
      if [ "$review_decision" = "APPROVED" ]; then
        echo "ready_to_merge=true"
        exit 0
      fi
      echo "ready_to_merge=false"
      exit 1
    SHELL
  }
  timeout = "30s"
  outcome "success" { next = "route_merge_readiness" }
  outcome "failure" { next = "route_merge_readiness" }
}

switch "route_merge_readiness" {
  condition {
    match = steps.check_merge_readiness.exit_code == "2"
    next  = state.merge_pr_and_sync_main
  }
  condition {
    match = steps.check_merge_readiness.exit_code == "0"
    next  = state.merge_pr_and_sync_main
  }
  default {
    next = state.backoff_ci
  }
}

# ── PR triage cycle counting ─────────────────────────────────────────────────

step "count_pr_cycle" {
  target = adapter.shell.default
  input {
    command = "echo $(( ${shared.pr_cycle_count} + 1 ))"
  }
  outcome "success" {
    next          = "check_pr_cycles"
    shared_writes = { pr_cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

switch "check_pr_cycles" {
  condition {
    match = shared.pr_cycle_count >= var.max_pr_cycles
    next  = state.request_pr_assist
  }
  default {
    next = state.triage_pr_feedback
  }
}

approval "request_pr_assist" {
  approvers = ["operator"]
  reason    = "PR triage has cycled without convergence. Continue with another cycle or abort?"
  outcome "approved" { next = "reset_pr_counter" }
  outcome "rejected" { next = "failed" }
}

step "reset_pr_counter" {
  target = adapter.shell.default
  input {
    command = "echo 0"
  }
  outcome "success" {
    next          = "triage_pr_feedback"
    shared_writes = { pr_cycle_count = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

# ── PR triage: agent handles feedback ────────────────────────────────────────

step "triage_pr_feedback" {
  target = adapter.copilot.pr_manager
  allow_tools = ["*"]
  max_visits  = 3
  input {
    prompt = <<-EOT
      PR checks reported unresolved feedback or failed checks.

      Use this context:
      --- CI status ---
      ${steps.check_ci_status.stdout}
      --- end ---

      --- Review threads ---
      ${steps.check_pr_comments.stdout}
      --- end ---

      HARD RULES:
      1. DO NOT run `gh pr merge` — the workflow's merge_pr_and_sync_main step owns merging.
      2. The repository requires every review thread to be resolved before merge. You MUST drive every unresolved (and not-outdated) thread to a resolved state.

      First: `gh pr view <num> --json state` — if state is MERGED, return RESULT: merged immediately.

      Otherwise enumerate every review thread via the GraphQL API and process each one where isResolved=false AND isOutdated=false:
        • If the comment is already addressed by code on the branch or by reviewer notes in the workstream file: reply on the thread with concrete evidence and resolve the thread.
        • If the comment requires NEW code changes you cannot resolve by citation: leave the thread unresolved, return RESULT: needs_executor so the executor can fix it.
        • If a check (CI) failed: investigate via `gh pr checks` / `gh run view`. If a code fix is needed, return RESULT: needs_executor.

      Return values:
        RESULT: merged          — PR is already MERGED on GitHub.
        RESULT: needs_executor  — code changes are required.
        RESULT: recheck         — you replied to and resolved every addressable thread; gate should re-poll.
        RESULT: watch_pr        — checks still running, no review action available yet.
        RESULT: failure         — unrecoverable error.

      End your final line with exactly one of:
      RESULT: merged
      RESULT: needs_executor
      RESULT: recheck
      RESULT: watch_pr
      RESULT: failure
    EOT
  }
  outcome "merged"         { next = "merge_pr_and_sync_main" }
  outcome "needs_executor" { next = "execute_pr_feedback" }
  outcome "recheck"        { next = "backoff_ci" }
  outcome "watch_pr"       { next = "backoff_ci" }
  outcome "needs_review"   { next = "backoff_ci" }
  outcome "needs_approval" { next = "backoff_ci" }
  outcome "failure"        { next = "failed" }
}

# ── PR feedback: executor makes code changes ────────────────────────────────
# After changes, verify_pr runs local CI. If CI passes, re-enter the remote
# check loop via backoff_ci. If CI fails, fix_verify_pr loops.

step "execute_pr_feedback" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  input {
    prompt = <<-EOT
      PR manager determined code changes are required from review comments or check failures.

      Use this context:
      --- CI status ---
      ${steps.check_ci_status.stdout}
      --- end ---

      --- Review threads ---
      ${steps.check_pr_comments.stdout}
      --- end ---

      For every unresolved (and not-outdated) review thread that requires a code change:
        1. Implement the fix.
        2. Update ${var.workstream_file} notes with the remediation.
        3. Commit and push.
        4. Reply on the thread citing the fix (commit SHA + file:line) and resolve the thread via the GraphQL resolveReviewThread mutation.

      The repository requires zero unresolved threads before merge. Do not leave any addressed thread unresolved. Do not resolve threads you have not actually addressed.
    EOT
  }
  outcome "success"        { next = "verify_pr" }
  outcome "needs_review"   { next = "verify_pr" }
  outcome "needs_approval" { next = "verify_pr" }
  outcome "failure"        { next = "failed" }
}

step "verify_pr" {
  target = adapter.shell.default
  input {
    command = "make ci 2>&1"
  }
  timeout = "120s"
  outcome "success" { next = "backoff_ci" }
  outcome "failure" { next = "fix_verify_pr" }
}

step "fix_verify_pr" {
  target = adapter.copilot.executor
  allow_tools = ["*"]
  max_visits  = 3
  input {
    prompt = "CI verification failed after PR feedback changes. Fix all failures, then commit and push.\n\n--- verify output ---\n${steps.verify_pr.stdout}\n--- end ---"
  }
  outcome "success"        { next = "verify_pr" }
  outcome "needs_review"   { next = "verify_pr" }
  outcome "needs_approval" { next = "verify_pr" }
  outcome "failure"        { next = "failed" }
}

# ── Merge and sync ───────────────────────────────────────────────────────────

step "merge_pr_and_sync_main" {
  target = adapter.shell.default
  input {
    command = <<-SHELL
      set -uo pipefail; exec 2>&1
      branch=$(git branch --show-current)
      pr_state=""
      pr_number=""
      if [ -n "$branch" ] && [ "$branch" != "main" ]; then
        pr_view=$(gh pr view "$branch" --json number,state 2>/dev/null || true)
        if [ -n "$pr_view" ]; then
          pr_number=$(printf '%s' "$pr_view" | jq -r '.number // empty')
          pr_state=$(printf '%s' "$pr_view" | jq -r '.state // empty')
        fi
      fi
      echo "branch=$branch pr_number=$${pr_number:-unknown} pr_state=$${pr_state:-unknown}"
      if [ -n "$pr_number" ] && [ "$pr_state" != "MERGED" ] && [ "$pr_state" != "CLOSED" ]; then
        gh pr merge "$pr_number" --squash --delete-branch || { echo 'merge command failed'; exit 1; }
      else
        echo 'skip_merge=true'
      fi
      git fetch origin main || exit 1
      git checkout main || exit 1
      git pull --ff-only origin main || exit 1
      echo "synced_main=true merged_pr=$${pr_number:-unknown}"
      exit 0
    SHELL
  }
  timeout = "5m"
  outcome "success" { next = "merged" }
  outcome "failure" { next = "merged" }
}

# ── Terminal states ──────────────────────────────────────────────────────────

state "merged" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}

output "result" {
  type  = "string"
  value = "merged"
}