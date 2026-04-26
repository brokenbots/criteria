# mode: standalone (uses agent adapters; Castle not required)
#
# Workstream Review Loop
# ======================
# Runs a three-agent review loop against a single workstream file.
# Pass the target file via the workstream_file variable default, or override
# it by editing the default value before running.
#
#   executor  — implements workstream tasks in focused passes
#   reviewer  — reviews executor changes for correctness and completeness
#   cleanup   — commits approved changes and performs final clean-up
#
# Loop mechanics:
#   • Executor and reviewer iterate until the reviewer is satisfied.
#   • The reviewer signals "approved" to hand off to the cleanup agent.
#   • Cleanup runs once; if it finds issues it returns to the executor for a
#     single fix pass. A second cleanup failure fails the workflow.
#
# Usage (run once per workstream file):
#   bin/overseer apply examples/workstream_review_loop.hcl
#
# Note: for_each multi-step agent chains are not supported by the engine —
# the do-step must return _continue to advance the loop. Use this single-file
# pattern and invoke once per workstream instead.

workflow "workstream_review_loop" {
  version       = "1"
  initial_state = "load_executor_agent_file"
  target_state  = "done"

  policy {
    max_total_steps = 2000
  }

  variable "workstream_file" {
    type        = "string"
    default     = "workstreams/01-event-contract-repair.md"
    description = "Path to the workstream file to process. Change default or re-run for each file."
  }

  # ── Agents ─────────────────────────────────────────────────────────────────

  agent "executor" {
    adapter = "copilot"
    config {
      max_turns = 12
    }
  }

  agent "reviewer" {
    adapter = "copilot"
    config {
      max_turns = 10
    }
  }

  agent "cleanup" {
    adapter = "copilot"
    config {
      max_turns = 8
    }
  }

  # ── Load agent instruction files + open agent sessions ──────────────────────

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
    outcome "success" { transition_to = "load_cleanup_agent_file" }
    outcome "failure" { transition_to = "failed" }
  }

  step "load_cleanup_agent_file" {
    adapter = "shell"
    input {
      command = "awk 'NR==1 && $0==\"---\"{f=1;next} f && $0==\"---\"{f=0;next} !f{if(!s){if($0 ~ /^[[:space:]]*$/) next; s=1} print}' .github/agents/workstream-cleanup.agent.md"
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
    outcome "success" { transition_to = "open_cleanup" }
    outcome "failure" { transition_to = "close_executor_abort" }
  }

  step "open_cleanup" {
    agent     = "cleanup"
    lifecycle = "open"
    outcome "success" { transition_to = "execute" }
    outcome "failure" { transition_to = "close_reviewer_abort" }
  }

  # ── Per-workstream: executor / reviewer loop ───────────────────────────────

  step "execute" {
    agent       = "executor"
    allow_tools = [
      "read",
      "write",
      "shell:*",
      "mcp",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-executor.agent.md):\n${steps.load_executor_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nExecute the next batch of implementation tasks for this workstream only. Keep changes focused and verifiable. End your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
    }
    outcome "needs_review"   { transition_to = "review" }
    outcome "needs_approval" { transition_to = "review" }
    outcome "failure"        { transition_to = "close_cleanup_abort" }
  }

  step "review" {
    agent       = "reviewer"
    allow_tools = [
      "read",
      "write",
      "shell:*",
      "mcp",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-reviewer.agent.md):\n${steps.load_reviewer_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nReview the latest executor changes against this workstream. If fully acceptable, approve. If more work is needed, request changes. End your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
    }
    outcome "approved"          { transition_to = "run_cleanup" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_cleanup_abort" }
  }

  # ── Cleanup: commit, verify, and finalize ──────────────────────────────────

  step "run_cleanup" {
    agent       = "cleanup"
    allow_tools = [
      "read",
      "write",
      "shell:*",
      "mcp",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-cleanup.agent.md):\n${steps.load_cleanup_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nReviewer approved the implementation. Perform cleanup and close-out for this workstream. End your final line with exactly one of:\nRESULT: success\nRESULT: needs_fix\nRESULT: failure"
    }
    outcome "success"   { transition_to = "close_cleanup_done" }
    outcome "needs_fix" { transition_to = "execute_fix_once" }
    outcome "failure"   { transition_to = "close_cleanup_abort" }
  }

  # ── Cleanup retry: one executor fix pass ──────────────────────────────────
  #
  # The cleanup agent found issues after approval. The executor gets exactly one
  # pass to address them. A second cleanup failure aborts the workstream.

  step "execute_fix_once" {
    agent       = "executor"
    allow_tools = [
      "read",
      "write",
      "shell:*",
      "mcp",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-executor.agent.md):\n${steps.load_executor_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nThe cleanup agent found issues after approval. Apply a single focused fix pass to resolve those issues only. End your final line with exactly one of:\nRESULT: fixed\nRESULT: failure"
    }
    outcome "fixed"   { transition_to = "run_cleanup_final" }
    outcome "failure" { transition_to = "close_cleanup_abort" }
  }

  step "run_cleanup_final" {
    agent       = "cleanup"
    allow_tools = [
      "read",
      "write",
      "shell:*",
      "mcp",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-cleanup.agent.md):\n${steps.load_cleanup_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nFinal cleanup pass after the one allowed executor fix. If any issue remains, fail the workflow. End your final line with exactly one of:\nRESULT: success\nRESULT: failure"
    }
    outcome "success" { transition_to = "close_cleanup_done" }
    outcome "failure" { transition_to = "close_cleanup_abort" }
  }

  # ── Close agents: success path ──────────────────────────────────────────────

  step "close_cleanup_done" {
    agent     = "cleanup"
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

  step "close_cleanup_abort" {
    agent     = "cleanup"
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
