workflow "agent_hello" {
  version       = "1"
  initial_state = "open_assistant"
  target_state  = "done"

  agent "assistant" {
    adapter = "copilot"
  }

  step "open_assistant" {
    agent     = "assistant"
    lifecycle = "open"

    outcome "success" { transition_to = "ask" }
    outcome "failure" { transition_to = "failed" }
  }

  step "ask" {
    agent       = "assistant"
    allow_tools = ["shell:git status"]
    config = {
      max_turns = "4"
      prompt    = "Run `git status` in the current directory. Summarize the result in one short paragraph. End your final line with exactly one of: RESULT: success | RESULT: needs_review | RESULT: failure. Use RESULT: success only if you successfully ran `git status`."
    }

    outcome "success"      { transition_to = "close_done" }
    outcome "needs_review" { transition_to = "close_needs_review" }
    outcome "failure"      { transition_to = "close_failed" }
  }

  step "close_done" {
    agent     = "assistant"
    lifecycle = "close"

    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_needs_review" {
    agent     = "assistant"
    lifecycle = "close"

    outcome "success" { transition_to = "needs_review" }
    outcome "failure" { transition_to = "failed" }
  }

  step "close_failed" {
    agent     = "assistant"
    lifecycle = "close"

    outcome "success" { transition_to = "failed" }
    outcome "failure" { transition_to = "failed" }
  }

  state "done" {
    terminal = true
    success  = true
  }

  state "needs_review" {
    terminal = true
    success  = false
    requires = "human_input"
  }

  state "failed" {
    terminal = true
    success  = false
  }
}