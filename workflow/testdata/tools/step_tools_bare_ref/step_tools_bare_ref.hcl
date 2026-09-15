workflow {
  name          = "tools-step-bare-ref"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "mcp" "registry" {
  tool "search" {}
}

step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }