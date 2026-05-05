workflow "phase3-environment" {
  version = "0.3.0"
  initial_state = "print_env"
  target_state = "done"
  environment = "shell.ci"

  environment "shell" "ci" {
    variables = {
      CI = "true"
      LOG_LEVEL = "debug"
      SERVICE_NAME = "criteria-test"
    }
  }

  adapter "shell" "default" {
    config { }
  }

  state "done" {
    terminal = true
    success = true
  }

  step "print_env" {
    target = adapter.shell.default
    input {
      command = "printenv"
    }
    outcome "success" {
      next = "done"
    }
  }
}

