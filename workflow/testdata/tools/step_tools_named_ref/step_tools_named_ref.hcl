workflow {
  name          = "tools-step-named-ref"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "mcp" "registry" {
  tool "search" {}
}

step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools.search]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }