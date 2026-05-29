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

adapter "shell" "default" {
  environment = shell.prod
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
