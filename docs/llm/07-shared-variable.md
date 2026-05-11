# Pattern: Mutable shared state

## When to use

Use when two or more steps need to read and update a common value during the
same run. `shared_variable` provides engine-managed, workflow-scoped mutable
state with deterministic write ordering.

## Minimal example

```hcl
workflow "shared-var" {
  version       = "1"
  initial_state = "increment"
  target_state  = "done"
}

adapter "noop" "default" {}

shared_variable "counter" {
  type  = "string"
  value = "0"
}

step "increment" {
  target = adapter.noop.default
  outcome "success" {
    next          = "double"
    shared_writes = { counter = "next_value" }
  }
}

step "double" {
  target = adapter.noop.default
  input {
    current = shared.counter
  }
  outcome "success" {
    next          = "done"
    shared_writes = { counter = "next_value" }
  }
}

state "done" {
  terminal = true
  success  = true
}
```

## Key idioms

- **`shared_variable "name" { type = "string" value = "..." }`** — declares a workflow-scoped variable with an optional initial value.
- **`shared.<name>`** — reads the current value of the variable in any expression including step inputs.
- **`shared_writes = { var_name = "adapter_output_key" }`** — in an outcome block, maps a shared variable name to an adapter output key whose value is written atomically on that transition.

## Common pitfalls

- **Parallel write races** — writing the same `shared_variable` from concurrent parallel iterations produces non-deterministic values; prefer sequential `for_each` when order matters.
- **Write semantics** — `shared_writes` values are adapter output key names (strings), not the new values themselves; the engine resolves the key from the adapter result.

## See also

- [LANGUAGE-SPEC.md § shared_variable](../LANGUAGE-SPEC.md#shared_variable-name--)
- [LANGUAGE-SPEC.md § Outcome model](../LANGUAGE-SPEC.md#outcome-model)
- [04-iteration-parallel.md](04-iteration-parallel.md) for cautions about shared state in parallel steps.
