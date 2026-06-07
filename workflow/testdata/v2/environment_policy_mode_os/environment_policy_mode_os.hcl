workflow {
  name = "environment-policy-mode-os"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "shell" "prod" {
  policy_mode = "strict"
  os          = "linux"
  variables = {
    CI = "true"
  }
}

adapter "exec" "default" {
  environment = shell.prod
}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
