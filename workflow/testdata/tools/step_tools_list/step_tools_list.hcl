workflow {
  name          = "tools-step-list"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}

adapter "mcp" "registry" {
  tool "search" {}
  tool "fetch" {}
}

adapter "shell" "worker" {
  tool "git_status" {}
}

step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools, adapter.mcp.registry.tools.search, adapter.shell.worker.tools.git_status]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }