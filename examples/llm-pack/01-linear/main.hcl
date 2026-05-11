workflow "linear" {
  version       = "1"
  initial_state = "fetch"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "fetch" {
  target = adapter.shell.default
  input {
    command = "echo rawdata"
  }
  outcome "success" { next = "transform" }
  outcome "failure" { next = "failed" }
}

step "transform" {
  target = adapter.shell.default
  input {
    command = "echo processed:${steps.fetch.stdout}"
  }
  outcome "success" { next = "publish" }
  outcome "failure" { next = "failed" }
}

step "publish" {
  target = adapter.shell.default
  input {
    command = "echo done:${steps.transform.stdout}"
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
