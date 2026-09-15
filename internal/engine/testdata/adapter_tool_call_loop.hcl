# CRI-166: M5.3 adapter-to-adapter tool call loop (ADR-0004).
#
# The caller step carries a step-level tools grant and tool-calls the callee
# adapter mid-step. The callee runs in its own session — its own environment
# and its own allow_tools — and never enters the FSM: its outputs are typed on
# the caller side under callee.* keys, and the caller's own outcome routing
# finishes the run. This is the demoable M5 loop and the template for the M6
# matrix (CRI-167..170).
workflow {
  name          = "adapter_tool_call_loop"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"
}

environment "shell" "prod" {
  os = "linux"
}

adapter "callee" "default" {
  environment = shell.prod

  tool "helper_task" {}
}

adapter "caller" "default" {}

step "call" {
  target = adapter.caller.default

  # Step-level tools grant (ADR-0004 §4): the caller may invoke the callee's
  # helper_task without any allow_tools on its own side.
  tools = [adapter.callee.default.tools.helper_task]

  outcome "success" { next = step.done }
}

state "done" { terminal = true }

# Workflow-level allow_tools: becomes the callee synthetic step's AllowTools —
# the callee's own policy, distinct from the caller's step-level tools grant.
permissions {
  allow_tools = ["callee.helpers.*"]
}

# The callee's outputs land on the caller side typed, under callee.* keys.
# These run outputs project them out of the run; the meta.id projection keeps
# nested attribute access on the callee's object output, which only works
# while the values stay typed (a flattened string would fail to eval).
output "callee_report" {
  value = steps.call["callee.report"]
}
output "callee_count" {
  value = steps.call["callee.count"]
}
output "callee_meta_id" {
  value = steps.call["callee.meta"].id
}