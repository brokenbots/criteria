workflow {
  name = "serve_adapter_toy"
  version = "0.1"
  initial_state = "warmup"
  target_state  = "done"
}

variable "label" {
  type    = string
  default = "hello"
}

adapter "noop" "demo" {}

output "final" {
  value = var.label
}

step "warmup" {
  target = adapter.noop.demo
  input {
    prompt = "warmup"
  }
  outcome "success" { next = step.slow }
}

step "slow" {
  target = adapter.noop.demo
  input {
    prompt = "slow"
    delay_ms = 4000
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
