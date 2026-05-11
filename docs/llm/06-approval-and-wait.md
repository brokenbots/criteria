# Pattern: Human-in-the-loop (approval and wait)

## When to use

Use this pattern when a workflow must pause for human sign-off before continuing, or wait for an external signal (such as a deployment window opening). Both `approval` and signal-based `wait` nodes require a server-compatible orchestrator (`criteria apply --server ...`).

## Minimal example

```hcl
workflow "approval_and_wait" {
  version       = "0.1"
  initial_state = "prepare"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "prepare" {
  target = adapter.shell.default
  input { command = "echo 'ready for release'" }
  outcome "success" { next = "release_gate" }
  outcome "failure" { next = "failed" }
}

approval "release_gate" {
  approvers = ["@engineering"]
  reason    = "Approve deployment to production"
  outcome "approved" { next = "deploy_window" }
  outcome "rejected" { next = "failed" }
}

wait "deploy_window" {
  signal = "deploy-ready"
  outcome "received" { next = "deploy" }
}

step "deploy" {
  target = adapter.shell.default
  input { command = "echo 'deploying'" }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
```

## Key idioms

- **`approval "<name>" { approvers = [...] reason = "..." }`** — pauses execution and notifies the listed approvers.
- **`outcome "approved"` / `"rejected"`** — these exact names are required on every `approval` block; the compiler rejects any other names.
- **`wait "<name>" { signal = "..." }`** — pauses until the named signal is posted to the orchestrator.

## Common pitfalls

- **Requires `--server`** — `criteria validate` compiles the workflow, but `criteria apply` will fail locally; both `approval` and signal-based `wait` need a running orchestrator.
- **Wrong outcome names on approval** — only `"approved"` and `"rejected"` are accepted; omitting or renaming either is a compile error.

## See also

- [LANGUAGE-SPEC.md § approval](../LANGUAGE-SPEC.md#approval)
- [LANGUAGE-SPEC.md § wait](../LANGUAGE-SPEC.md#wait)
