# KB-394 canary child — declared fate "fresh" under container hosting.
#
# The body re-declares the parent's container environment and the freshp
# adapter pair with continuation = "fresh" on both sides (compile-enforced).
# Every re-entry of this subworkflow releases the inherited engagement at
# body entry and opens a NEW conversation for this scope — the container
# child itself keeps serving (one child per definition), the conversation
# is what re-entry fate governs.
#
# mode: standalone
workflow {
  name          = "kb-394-canary-fresh-child"
  version       = "0.1"
  initial_state = "body"
  target_state  = "done"
  policy {
    max_total_steps = 20
  }
}

environment "container" "e2e" {}

adapter "shell" "freshp" {
  source       = "localhost:5000/criteria-adapter-shell"
  version      = "v0.5.6"
  environment  = container.e2e
  continuation = "fresh"
}

step "body" {
  target = adapter.shell.freshp
  input {
    command = "echo fresh pair body: opening a fresh conversation at this container re-entry"
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
