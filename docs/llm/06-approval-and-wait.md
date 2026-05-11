# Pattern: Human-in-the-loop

## When to use

Use when workflow execution must pause for an external event or a human
decision before continuing. `wait` blocks on a named signal; `approval`
requires explicit sign-off from named approvers. Both require server mode
(`criteria apply --server ...`); `criteria validate` compiles them fine.

## Minimal example

```hcl
workflow "approval-wait" {
  version       = "1"
  initial_state = "deploy_window"
  target_state  = "done"
}

adapter "noop" "default" {}

wait "deploy_window" {
  signal = "deploy-ready"
  outcome "received" { next = "release_gate" }
  outcome "expired"  { next = "failed" }
}

approval "release_gate" {
  approvers = ["ops-lead", "security-lead"]
  reason    = "Production release requires dual approval."
  outcome "approved" { next = "deploy" }
  outcome "rejected" { next = "failed" }
}

step "deploy" {
  target = adapter.noop.default
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

- **`wait "name" { signal = "..." }`** — pauses until the named signal arrives; use `duration = "5m"` instead of `signal` for a timed pause.
- **`approval "name" { approvers = [...] reason = "..." }`** — requires human approval; the engine notifies each approver.
- **Required outcomes** — `approval` must declare exactly `approved` and `rejected` outcomes; the compiler enforces this.

## Common pitfalls

- **Server mode required** — `criteria apply` without `--server` rejects `wait { signal }` and `approval` at runtime; use `criteria validate` to compile-check without a server.
- **Missing `expired` outcome** — signal-based waits can time out on the server side; declare a path for the non-happy case.

## See also

- [LANGUAGE-SPEC.md § wait](../LANGUAGE-SPEC.md#wait-name--)
- [LANGUAGE-SPEC.md § approval](../LANGUAGE-SPEC.md#approval-name--)
