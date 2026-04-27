# mode: standalone (uses agent adapters; Castle not required)
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
    max_total_steps = 1000
  }

  variable "workstream_file" {
    type        = "string"
    default     = "workstreams/02-castle-mode-integration.md"
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
    outcome "success" { transition_to = "execute" }
    outcome "failure" { transition_to = "close_executor_abort" }
  }

  # ── Per-workstream: executor / reviewer loop ───────────────────────────────

  step "execute" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-executor.agent.md):\n${steps.load_executor_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nStart by reading ${var.workstream_file} with read_file and extracting the next unchecked items. Then execute the next focused implementation batch for this workstream only, including code + tests where applicable. Keep changes verifiable and scoped. End your final line with exactly one of:\nRESULT: needs_review\nRESULT: failure"
    }
    outcome "needs_review"   { transition_to = "review" }
    outcome "needs_approval" { transition_to = "review" }
    outcome "failure"        { transition_to = "close_reviewer_abort" }
  }

  step "review" {
    agent       = "reviewer"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-reviewer.agent.md):\n${steps.load_reviewer_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nReview the latest executor changes against this workstream. If more work is needed, request changes and send the executor back for another pass. If fully acceptable, approve and hand off for a final executor commit step. End your final line with exactly one of:\nRESULT: approved\nRESULT: changes_requested\nRESULT: failure"
    }
    outcome "approved"          { transition_to = "commit_and_finish" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "needs_review"      { transition_to = "execute" }
    outcome "needs_approval"    { transition_to = "execute" }
    outcome "failure"           { transition_to = "close_reviewer_abort" }
  }

  # ── Finalize: executor commit and close-out ────────────────────────────────

  step "commit_and_finish" {
    agent       = "executor"
    allow_tools = [
      "*",
    ]
    input {
      prompt = "Agent profile (.github/agents/workstream-executor.agent.md):\n${steps.load_executor_agent_file.stdout}\n\nCurrent workstream file: ${var.workstream_file}\n\nThe reviewer approved the implementation. Commit only the intended workstream changes and use this exact commit message format:\nworkstream: complete ${var.workstream_file}\n\nThen run only minimal final verification needed for those changes. If commit or verification fails, report failure. End your final line with exactly one of:\nRESULT: success\nRESULT: failure"
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
