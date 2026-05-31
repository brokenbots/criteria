workflow {
  name = "environment-sandbox"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "sandbox" "secure" {
  os = "linux"
  filesystem = {
    read_only = true
  }
  network = {
    allow_egress = false
  }
  resources = {
    max_memory = "512M"
  }
}

adapter "shell" "default" {
  environment = sandbox.secure
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = state.done }
}

state "done" { terminal = true }
