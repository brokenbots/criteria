# Pattern: Adapter tool calls

## When to use

Use when a tool-capable adapter needs data or actions from another adapter mid-step. The caller step declares a `tools` list granting the callee tools; the callee executes inline and returns the result — calls are not steps and never route through outcomes.

## Minimal example

```hcl
workflow {
  name = "review-with-tools"
  version       = "1"
  initial_state = "review"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}

adapter "git" "repo" {
  tool "git_status" {}
  tool "git_diff" {}
}

adapter "copilot" "assistant" {}

step "review" {
  target = adapter.copilot.assistant
  tools  = [adapter.git.repo.tools.git_status,
            adapter.git.repo.tools.git_diff]
  input {
    prompt = "Review the working tree."
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
```

## Key idioms

- **`tools = [adapter.<type>.<name>.tools.<tool>]`** — grant list of bare traversals; entries are literals, not globs.
- **Bare `…tools` entry** — grants the callee's entire tool surface; `…tools.<tool>` grants exactly one.
- **`tool "<name>" {}` / `dynamic_tools = true`** — declares the callee tool surface (static or runtime).
- **`allow_tools`** — glob gate on the target string (`filepath.Match`, first match wins, empty denies all); lists union.
- **`policy { max_tool_depth = N }`** — bounds nested call depth; integer ≥ 1, default `8`.
- **`policy { max_tool_calls = N }`** — bounds the total tool calls per caller session; integer ≥ 1, default `100`.

## Common pitfalls

- **The callee never enters the FSM** — a call creates no nodes, fires no outcome blocks, and produces no `steps.<name>` outputs; results return to the caller only.
- **Pointless entries are flagged, not fatal** — `tools` on a non-adapter_tools target warns; named entries on a tool-less callee error.
- **Depth caps** — beyond `policy.max_tool_depth` calls fail typed `depth_exceeded`; beyond `policy.max_tool_calls`, `budget_exhausted`; cycles draw a compile-time warning.
- **Grants are not globs** — entries name exact instances; runtime selection is `allow_tools`.

## See also

- [LANGUAGE-SPEC.md § Adapter tools](../LANGUAGE-SPEC.md#adapter-tools)
- [ADR-0004](../adrs/ADR-0004-adapter-tools.md) — wire contract.
- [05-subworkflow.md](05-subworkflow.md) — FSM-level delegation.