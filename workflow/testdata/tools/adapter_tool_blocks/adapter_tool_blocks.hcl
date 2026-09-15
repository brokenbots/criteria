workflow {
  name          = "tools-adapter-tool-blocks"
  version       = "1"
  initial_state = "start"
  target_state  = "done"

  policy {
    max_tool_depth = 12
  }
}

adapter "mcp" "registry" {
  tool "search" {}
  tool "fetch" {}
  tool "reserved" {
    # The tool block body is reserved (CRI-155): unknown attributes and blocks
    # are decoded into Remain and ignored without error.
    unknown_attribute = "ignored"
    unknown_block "x" {
      nested = true
    }
  }
}

step "start" {
  target = adapter.mcp.registry
  outcome "success" { next = state.done }
}

state "done" { terminal = true }