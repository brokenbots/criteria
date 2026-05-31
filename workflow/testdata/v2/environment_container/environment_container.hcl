workflow {
  name = "environment-container"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "container" "docker" {
  os      = "linux"
  runtime = "docker"
  image   = "alpine:latest"
  network = {
    allow_egress = true
  }
}

adapter "shell" "default" {
  environment = container.docker
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
