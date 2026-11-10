# KB-394 canary child — declared fate "reuse" under container hosting.
#
# The body re-declares the parent's container environment and the reusep
# adapter pair with continuation = "reuse" on both sides (compile-enforced).
# Every re-entry of this subworkflow detaches and re-adopts the inherited
# engagement, so the same container conversation keeps serving the pair.
#
# mode: standalone
workflow {
  name          = "kb-394-canary-reuse-child"
  version       = "0.1"
  initial_state = "body"
  target_state  = "done"
  policy {
    max_total_steps = 20
  }
}

environment "container" "e2e" {}

adapter "shell" "reusep" {
  source       = "localhost:5000/criteria-adapter-shell"
  version      = "v0.5.6"
  environment  = container.e2e
  continuation = "reuse"
}

step "body" {
  target = adapter.shell.reusep
  input {
    command = "echo reuse pair body: re-adopting the inherited container conversation"
  }

  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
