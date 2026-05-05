workflow "phase3_subworkflow_demo" {
  version       = "0.1"
  initial_state = "setup"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

// Declare a subworkflow that will be compiled from the ./subworkflows/inner directory
subworkflow "inner_task" {
  source = "./subworkflows/inner"
  input = {
    work = "demo_task"
  }
}

step "setup" {
  target = adapter.shell.default
  input {
    command = "echo 'Starting subworkflow demo'"
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
