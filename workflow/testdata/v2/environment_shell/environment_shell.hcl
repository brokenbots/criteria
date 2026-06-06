workflow {
  name = "environment-shell"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "shell" "default" {
  variables = {
    CI = "true"
  }
}

adapter "exec" "default" {
  environment = shell.default
}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
