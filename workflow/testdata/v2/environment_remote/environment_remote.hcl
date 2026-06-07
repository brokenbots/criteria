workflow {
  name = "environment-remote"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "cluster" {
  listen_address = "0.0.0.0:8080"
  mtls           = true
  accept_token   = "token"
}

adapter "exec" "default" {
  environment = remote.cluster
}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
