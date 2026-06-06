workflow {
  name = "adapter-secrets"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "exec" "default" {
  secrets {
    api_key = "key"
  }
}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
