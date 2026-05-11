workflow "subworkflow_call" {
  version       = "0.1"
  initial_state = "run_process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

subworkflow "process_one" {
  source = "./subworkflows/process_one"
}

step "run_process" {
  target = subworkflow.process_one
  input {
    item = "hello"
  }
  outcome "success" { next = "report" }
  outcome "failure" { next = "failed" }
}

step "report" {
  target = adapter.shell.default
  input {
    command = "echo 'result: ${steps.run_process.result}'"
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
