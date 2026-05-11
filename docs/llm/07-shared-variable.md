# Pattern: Mutable shared state

## When to use

Use this pattern when two or more steps must read and update a shared value — for example, accumulating a status string, a counter, or a running log. `shared_variable` is mutable across step boundaries unlike `steps.<name>.<key>` which is immutable once written.

## Minimal example

```hcl
workflow "shared_state" {
  version       = "0.1"
  initial_state = "init"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

shared_variable "status" {
  type  = "string"
  value = "pending"
}

step "init" {
  target = adapter.shell.default
  input {
    command = "echo 'processing'"
  }
  outcome "success" {
    next          = "finish"
    shared_writes = { status = "stdout" }
  }
  outcome "failure" { next = "failed" }
}

step "finish" {
  target = adapter.shell.default
  input {
    command = "echo 'done: ${shared.status}'"
  }
  outcome "success" {
    next          = "done"
    shared_writes = { status = "stdout" }
  }
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

- **`shared_variable "<name>" { type = "..." value = "..." }`** — declares a mutable variable with an initial value.
- **`shared_writes = { <var> = "<adapter-output-key>" }`** — on outcome, writes the named adapter output (e.g. `"stdout"`) into the shared variable.
- **`shared.<name>`** — reads the current value of a shared variable inside any `input` expression.

## Common pitfalls

- **`shared_writes` value is a key name, not a literal** — `{ status = "stdout" }` copies the adapter's `stdout` field into `status`; it does not assign the string `"stdout"`.
- **Race on parallel writes** — avoid writing the same shared variable from concurrent `parallel` iterations; the last write wins non-deterministically.

## See also

- [LANGUAGE-SPEC.md § shared_variable](../LANGUAGE-SPEC.md#shared_variable)
- [01-linear.md](./01-linear.md) — immutable step-output chaining, no shared state needed.
