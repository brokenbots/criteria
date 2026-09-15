workflow {
  name          = "tools-adapter-dynamic"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "mcp" "registry" {
  dynamic_tools = true
}

step "start" {
  target = adapter.mcp.registry
  outcome "success" { next = state.done }
}

state "done" { terminal = true }