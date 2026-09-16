workflow {
  name = "review-with-tools"
  version       = "1"
  initial_state = "review"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}

adapter "git" "repo" {
  tool "git_status" {}
  tool "git_diff" {}
}

adapter "copilot" "assistant" {}

step "review" {
  target = adapter.copilot.assistant
  tools  = [adapter.git.repo.tools.git_status,
            adapter.git.repo.tools.git_diff]
  input {
    prompt = "Review the working tree."
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
