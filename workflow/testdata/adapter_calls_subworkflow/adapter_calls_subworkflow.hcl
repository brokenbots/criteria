// adapter_calls_subworkflow.hcl — adapter tool-call rendering in subworkflow
// clusters (CRI-158). The parent targets a subworkflow (its body renders as a
// dot cluster) and keeps one direct adapter step, so the root call-edge path
// and the namespaced cluster path are both exercised in one fixture. The
// subworkflow body carries a bare self-grant and a named self-grant.
workflow {
  name          = "adapter-calls-subworkflow"
  version       = "1"
  initial_state = "begin"
  target_state  = "done"
}

subworkflow "call" {
  source = "./subworkflows/call"
}

adapter "shell" "host" {
  tool "status" {}
}

step "begin" {
  target = subworkflow.call
  outcome "success" { next = step.finish }
}

step "finish" {
  target = adapter.shell.host
  tools  = [adapter.shell.host.tools.status]
  outcome "success" { next = state.done }
}

state "done" { terminal = true }