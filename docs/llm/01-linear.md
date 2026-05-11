# Pattern: Linear pipeline

## When to use

Use when your workflow is a fixed sequence of steps with no branching or
looping. Each step can consume outputs from any earlier step via
`steps.<name>.<key>`.

## Minimal example

```hcl
workflow "linear" {
  version       = "1"
  initial_state = "fetch"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "fetch" {
  target = adapter.shell.default
  input {
    command = "echo rawdata"
  }
  outcome "success" { next = "transform" }
  outcome "failure" { next = "failed" }
}

step "transform" {
  target = adapter.shell.default
  input {
    command = "echo processed:${steps.fetch.stdout}"
  }
  outcome "success" { next = "publish" }
  outcome "failure" { next = "failed" }
}

step "publish" {
  target = adapter.shell.default
  input {
    command = "echo done:${steps.transform.stdout}"
  }
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

- **`adapter "shell" "default" {}`** — declares the shell adapter; steps reference it via `target = adapter.shell.default`.
- **`input { command = "..." }`** — the shell adapter requires one `command` key; other adapters use their own keys.
- **`steps.<name>.stdout`** — captured stdout from a completed step; available to any downstream step input.
- **`outcome "success" { next = "..." }`** — maps the adapter result to the next node; every possible outcome must be declared.

## Common pitfalls

- **Missing terminal state** — every code path must reach a `state { terminal = true }` node; the compiler rejects dangling paths.
- **`config` on the step body** — legacy `config = {...}` is rejected; use the nested `input { ... }` block instead.

## See also

- [LANGUAGE-SPEC.md § step](../LANGUAGE-SPEC.md#step-name--)
- [LANGUAGE-SPEC.md § Outcome model](../LANGUAGE-SPEC.md#outcome-model)
- [02-branching-switch.md](02-branching-switch.md) for conditional routing after a step.
