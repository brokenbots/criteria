workflow "linear_pipeline" {
  version       = "0.1"
  initial_state = "fetch"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "fetch" {
  target = adapter.shell.default
  input {
    command = "echo 'data-123'"
  }
  outcome "success" { next = "transform" }
  outcome "failure" { next = "failed" }
}

step "transform" {
  target = adapter.shell.default
  input {
    command = "echo 'transformed: ${steps.fetch.stdout}'"
  }
  outcome "success" { next = "publish" }
  outcome "failure" { next = "failed" }
}

step "publish" {
  target = adapter.shell.default
  input {
    command = "echo 'result: ${steps.transform.stdout}'"
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
