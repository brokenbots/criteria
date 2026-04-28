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
    max_total_steps = 50  # ~15 review cycles plus setup/teardown; increase if workstream is large
  }

  variable "workstream_file" {
    type        = "string"
    default     = "workstreams/02-server-mode-integration.md"
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
    outcome "success" { transition_to = "execute_init" }
    outcome "failure" { transition_to = "close_executor_abort" }
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
    outcome "failure"        { transition_to = "close_reviewer_abort" }
  }

  step "review_init" {
    agent       = "reviewer"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "${steps.load_reviewer_agent_file.stdout}\n\nRead ${var.workstream_file} for the workstream scope and the executor's latest work.\n\nReview the executor's changes against the acceptance bar. Write all findings and your verdict into the reviewer notes section of ${var.workstream_file}.\n\nEnd your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
    }
    outcome "approved"          { transition_to = "commit_and_finish" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_reviewer_abort" }
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
    outcome "failure"        { transition_to = "close_reviewer_abort" }
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
    outcome "failure"        { transition_to = "close_reviewer_abort" }
  }

  step "review" {
    agent       = "reviewer"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Ready for review. Latest work is in ${var.workstream_file}."
    }
    outcome "approved"          { transition_to = "commit_and_finish" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_reviewer_abort" }
  }

  # ── Finalize: executor commit ──────────────────────────────────────────────

  step "commit_and_finish" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Approved. Commit all workstream changes with message:\nworkstream: complete ${var.workstream_file}\n\nEnd your final line with exactly one of:\nRESULT: success\nRESULT: failure"
    }
    outcome "success" { transition_to = "close_reviewer_done" }
    outcome "failure" { transition_to = "close_reviewer_abort" }
  }

  # ── Close agents: success path ──────────────────────────────────────────────

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
