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
#   bin/criteria apply examples/workstream_review_loop.hcl
#
# Note: for_each multi-step agent chains are not supported by the engine —
# the do-step must return _continue to advance the loop. Use this single-file
# pattern and invoke once per workstream instead.

workflow "workstream_review_loop" {
  version       = "1"
  initial_state = "load_executor_agent_file"
  target_state  = "done"

  policy {
    max_total_steps = 120  # caps execute/review/pr loops; fails safely if automation cannot converge
  }

  variable "workstream_file" {
    type        = "string"
    default     = "workstreams/05-shell-adapter-sandbox.md"
    description = "Path to the workstream file to process. Change default or re-run for each file."
  }

  # ── Agents ─────────────────────────────────────────────────────────────────

  agent "executor" {
    adapter = "copilot"
    config {
      model            = "claude-sonnet-4.6"
      reasoning_effort = "medium"
      max_turns        = 12
    }
  }

  agent "reviewer" {
    adapter = "copilot"
    config {
      model            = "claude-sonnet-4.6"
      reasoning_effort = "high"
      max_turns        = 10
    }
  }

  agent "pr_manager" {
    adapter = "copilot"
    config {
      model            = "claude-sonnet-4.6"
      reasoning_effort = "high"
      max_turns        = 10
    }
  }

  # ── Load agent profiles (once) ─────────────────────────────────────────────
  # Profile text is injected into the first user turn of each agent's session.
  # It is never re-sent; all subsequent loop turns are short coordination signals.

  step "load_executor_agent_file" {
    adapter = "shell"
    input {
      command = "awk 'NR==1 && $0==\"---\"{f=1;next} f && $0==\"---\"{f=0;next} !f{if(!s){if($0 ~ /^[[:space:]]*$/) next; s=1} print}' .github/agents/workstream-executor.agent.md"
    }
    timeout = "10s"
    outcome "success" { transition_to = "load_reviewer_agent_file" }
    outcome "failure" { transition_to = "failed" }
  }

  step "load_reviewer_agent_file" {
    adapter = "shell"
    input {
      command = "awk 'NR==1 && $0==\"---\"{f=1;next} f && $0==\"---\"{f=0;next} !f{if(!s){if($0 ~ /^[[:space:]]*$/) next; s=1} print}' .github/agents/workstream-reviewer.agent.md"
    }
    timeout = "10s"
    outcome "success" { transition_to = "load_pr_manager_agent_file" }
    outcome "failure" { transition_to = "failed" }
  }

  step "load_pr_manager_agent_file" {
    adapter = "shell"
    input {
      command = "awk 'NR==1 && $0==\"---\"{f=1;next} f && $0==\"---\"{f=0;next} !f{if(!s){if($0 ~ /^[[:space:]]*$/) next; s=1} print}' .github/agents/workstream-pr-manager.agent.md"
    }
    timeout = "10s"
    outcome "success" { transition_to = "checkout_branch" }
    outcome "failure" { transition_to = "failed" }
  }

  step "checkout_branch" {
    adapter = "shell"
    input {
      command = "branch=$(basename '${var.workstream_file}' .md) && current=$(git branch --show-current) && if [ \"$current\" = \"main\" ]; then git checkout -b \"$branch\"; else echo \"already on branch: $current\"; fi"
    }
    timeout = "10s"
    outcome "success" { transition_to = "open_executor" }
    outcome "failure" { transition_to = "failed" }
  }

  # ── Open agent sessions ───────────────────────────────────────────────────
  # Failure paths close only the agents already opened before the fault.

  step "open_executor" {
    agent     = "executor"
    lifecycle = "open"
    outcome "success" { transition_to = "open_reviewer" }
    outcome "failure" { transition_to = "failed" }
  }

  step "open_reviewer" {
    agent     = "reviewer"
    lifecycle = "open"
    outcome "success" { transition_to = "open_pr_manager" }
    outcome "failure" { transition_to = "close_executor_abort" }
  }

  step "open_pr_manager" {
    agent     = "pr_manager"
    lifecycle = "open"
    outcome "success" { transition_to = "execute_init" }
    outcome "failure" { transition_to = "close_reviewer_abort" }
  }

  # ── Init pass: bootstrap agent context ─────────────────────────────────────
  # Each agent reads its own profile and the workstream file on its first turn.
  # That context persists in the live session for all subsequent loop turns.

  step "execute_init" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "${steps.load_executor_agent_file.stdout}\n\nRead ${var.workstream_file} for the full task scope.\n\nExecute the first implementation batch: complete the next unchecked items, write code and tests as needed, keep changes scoped and verifiable. Record your progress and notes in ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
    }
    outcome "needs_review"   { transition_to = "review_init" }
    outcome "needs_approval" { transition_to = "review_init" }
    outcome "failure"        { transition_to = "close_pr_manager_abort" }
  }

  step "review_init" {
    agent       = "reviewer"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "${steps.load_reviewer_agent_file.stdout}\n\nRead ${var.workstream_file} for the workstream scope and the executor's latest work.\n\nReview the executor's changes against the acceptance bar. Write all findings and your verdict into the reviewer notes section of ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
    }
    outcome "approved"          { transition_to = "commit_and_prepare_pr" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_pr_manager_abort" }
  }

  # ── Review loop: minimal signal prompts ─────────────────────────────────────
  # Agent context is fully established after the init pass.
  # These prompts are coordination signals only — not instructions.

  step "execute" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Reviewer requested changes. Notes are in ${var.workstream_file}."
    }
    outcome "needs_review"   { transition_to = "verify" }
    outcome "needs_approval" { transition_to = "verify" }
    outcome "failure"        { transition_to = "close_pr_manager_abort" }
  }

  step "verify" {
    adapter = "shell"
    input {
      command = "make ci 2>&1"
    }
    timeout = "120s"
    outcome "success" { transition_to = "review" }
    outcome "failure" { transition_to = "fix_verify" }
  }

  step "fix_verify" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Build/test verification failed. Fix all failures before this goes to review.\n\n--- verify output ---\n${steps.verify.stdout}\n--- end ---"
    }
    outcome "needs_review"   { transition_to = "verify" }
    outcome "needs_approval" { transition_to = "verify" }
    outcome "failure"        { transition_to = "close_pr_manager_abort" }
  }

  step "review" {
    agent       = "reviewer"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Ready for review. Latest work is in ${var.workstream_file}."
    }
    outcome "approved"          { transition_to = "commit_and_prepare_pr" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_pr_manager_abort" }
  }

  # ── Finalize: executor commit ──────────────────────────────────────────────

  step "commit_and_prepare_pr" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Approved. Commit all workstream changes with message:\nworkstream: complete ${var.workstream_file}\n\nEnd your final line with exactly one of:\nRESULT: success\nRESULT: failure"
    }
    outcome "success" { transition_to = "open_or_update_pr" }
    outcome "failure" { transition_to = "close_pr_manager_abort" }
  }

  # ── PR automation loop ────────────────────────────────────────────────────
  # PR manager owns creation/updates and comment replies.
  # Shell step blocks on required checks and returns gate status.

  step "open_or_update_pr" {
    agent       = "pr_manager"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "${steps.load_pr_manager_agent_file.stdout}\n\nRead ${var.workstream_file}. Ensure branch is pushed, then create or update the PR from the current branch to main.\n\nInclude a concise summary and test evidence from the workstream notes/reviewer notes.\n\nEnd your final line with exactly one of:\nRESULT: watch_pr\nRESULT: failure"
    }
    outcome "watch_pr"      { transition_to = "watch_pr_gate" }
    outcome "needs_review"  { transition_to = "watch_pr_gate" }
    outcome "needs_approval" { transition_to = "watch_pr_gate" }
    outcome "failure"       { transition_to = "close_pr_manager_abort" }
  }

  step "watch_pr_gate" {
    adapter = "shell"
    input {
      command = "set -euo pipefail; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); echo \"pr_number=$pr_number\"; if ! gh pr checks \"$pr_number\" --required --watch; then echo \"checks=failed\"; gh pr view \"$pr_number\" --json url,reviewDecision --template '{{.url}} review={{.reviewDecision}}\\n'; exit 1; fi; owner=$(gh repo view --json owner --jq '.owner.login'); repo=$(gh repo view --json name --jq '.name'); review_decision=$(gh pr view \"$pr_number\" --json reviewDecision --jq '.reviewDecision // \"REVIEW_REQUIRED\"'); unresolved_threads=$(gh api graphql -f query='query($owner:String!, $repo:String!, $number:Int!){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved isOutdated}}}}}' -f owner=\"$owner\" -f repo=\"$repo\" -F number=\"$pr_number\" --jq '[.data.repository.pullRequest.reviewThreads.nodes[] | select((.isOutdated|not) and (.isResolved|not))] | length'); echo \"review_decision=$review_decision\"; echo \"unresolved_threads=$unresolved_threads\"; if [ \"$review_decision\" = \"APPROVED\" ] && [ \"$unresolved_threads\" -eq 0 ]; then echo \"ready_to_merge=true\"; exit 0; fi; echo \"ready_to_merge=false\"; exit 1"
    }
    timeout = "45m"
    outcome "success" { transition_to = "merge_pr_and_sync_main" }
    outcome "failure" { transition_to = "triage_pr_feedback" }
  }

  step "triage_pr_feedback" {
    agent       = "pr_manager"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "PR watch gate reported unresolved feedback or failed checks.\n\nUse this gate output as context:\n--- watch_pr_gate output ---\n${steps.watch_pr_gate.stdout}\n--- end ---\n\nInspect the PR reviews/comments/threads. If a comment is already addressed by code or reviewer notes, reply with evidence and resolve where possible.\n\nReturn RESULT: needs_executor only when new code changes are required. Return RESULT: recheck when you handled comments and we should re-run gate checks.\n\nEnd your final line with exactly one of:\nRESULT: needs_executor\nRESULT: recheck\nRESULT: failure"
    }
    outcome "needs_executor" { transition_to = "execute_pr_feedback" }
    outcome "recheck"        { transition_to = "watch_pr_gate" }
    outcome "needs_review"   { transition_to = "watch_pr_gate" }
    outcome "needs_approval" { transition_to = "watch_pr_gate" }
    outcome "failure"        { transition_to = "close_pr_manager_abort" }
  }

  step "execute_pr_feedback" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "PR manager determined code changes are required from review comments or check failures.\n\nUse this gate output as context:\n--- watch_pr_gate output ---\n${steps.watch_pr_gate.stdout}\n--- end ---\n\nInspect the PR feedback directly, implement all required fixes, update ${var.workstream_file} notes, and prepare for verification/re-review."
    }
    outcome "needs_review"   { transition_to = "verify" }
    outcome "needs_approval" { transition_to = "verify" }
    outcome "failure"        { transition_to = "close_pr_manager_abort" }
  }

  step "merge_pr_and_sync_main" {
    adapter = "shell"
    input {
      command = "set -euo pipefail; branch=$(git branch --show-current); pr_number=$(gh pr view \"$branch\" --json number --jq '.number'); gh pr merge \"$pr_number\" --squash --delete-branch; git fetch origin main; git checkout main; git pull --ff-only origin main; echo \"merged_pr=$pr_number\""
    }
    timeout = "5m"
    outcome "success" { transition_to = "close_pr_manager_done" }
    outcome "failure" { transition_to = "triage_pr_feedback" }
  }

  # ── Close agents: success path ──────────────────────────────────────────────

  step "close_pr_manager_done" {
    agent     = "pr_manager"
    lifecycle = "close"
    outcome "success" { transition_to = "close_reviewer_done" }
    outcome "failure" { transition_to = "close_reviewer_done" }
  }

  step "close_reviewer_done" {
    agent     = "reviewer"
    lifecycle = "close"
    outcome "success" { transition_to = "close_executor_done" }
    outcome "failure" { transition_to = "close_executor_done" }
  }

  step "close_executor_done" {
    agent     = "executor"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }

  # ── Close agents: abort path ─────────────────────────────────────────────────
  # Each step chains to the next so all open sessions are closed before failing.

  step "close_pr_manager_abort" {
    agent     = "pr_manager"
    lifecycle = "close"
    outcome "success" { transition_to = "close_reviewer_abort" }
    outcome "failure" { transition_to = "close_reviewer_abort" }
  }

  step "close_reviewer_abort" {
    agent     = "reviewer"
    lifecycle = "close"
    outcome "success" { transition_to = "close_executor_abort" }
    outcome "failure" { transition_to = "close_executor_abort" }
  }

  step "close_executor_abort" {
    agent     = "executor"
    lifecycle = "close"
    outcome "success" { transition_to = "failed" }
    outcome "failure" { transition_to = "failed" }
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
}
