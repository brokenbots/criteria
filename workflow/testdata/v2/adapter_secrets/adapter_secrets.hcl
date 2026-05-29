workflow {
  name = "adapter-secrets"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "default" {
  secrets {
    api_key = "key"
  }
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
