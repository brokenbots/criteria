workflow {
  name = "compose-remote-demo"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "default" {
  listen_address = "0.0.0.0:7778"
  accept_token   = "smoke-token"
}

adapter "greeter" "default" {
  environment = remote.default
}

step "run" {
  target = adapter.greeter.default
  input {
    name = "world"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
