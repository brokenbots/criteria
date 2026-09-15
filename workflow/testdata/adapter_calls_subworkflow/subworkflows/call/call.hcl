// call.hcl — body of the adapter_calls_subworkflow parent fixture. The bare
// self-grant exposes the callee's whole tool surface; the named self-grant
// exposes one tool.
workflow {
  name          = "call"
  version       = "1"
  initial_state = "probe"
  target_state  = "ready"
}

adapter "mcp" "registry" {
  tool "search" {}
  tool "fetch" {}
}

step "probe" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools]
  outcome "success" { next = step.fetch }
}

step "fetch" {
  target = adapter.mcp.registry
  tools  = [adapter.mcp.registry.tools.fetch]
  outcome "success" { next = state.ready }
}

state "ready" { terminal = true }