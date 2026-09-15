// adapter_calls.hcl — adapter tool-call rendering fixture (CRI-158).
// Exercises the three call-edge grant shapes so the dot/json/plan renderers
// have one deterministic example: a bare self-grant (whole tool surface), a
// named cross-adapter grant, and a named self-grant.
workflow {
  name          = "adapter-calls"
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

// Caller grants its own whole tool surface (bare) plus one named tool on
# another adapter.
step "start" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools, adapter.shell.worker.tools.git_status]
  outcome "success" { next = step.audit }
}

// Caller grants one of its own named tools.
step "audit" {
  target = adapter.shell.worker
  tools  = [adapter.shell.worker.tools.git_status]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }