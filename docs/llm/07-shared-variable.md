# Pattern: Mutable shared state

## When to use

Use when two or more steps need to read and update a common value during the
same run. `data "internal"` provides engine-managed, workflow-scoped mutable
state with deterministic write ordering.

## Minimal example

```hcl
workflow {
  name = "shared-var"
  version       = "1"
  initial_state = "increment"
  target_state  = "done"
}

adapter "noop" "default" {}
data "internal" "counter" {
  type = string
  value = "0"
}

step "increment" {
  target = adapter.noop.default
  outcome "success" {
    next = step.double
    write {
      target = data.internal.counter.value
      value  = "1"
    }
  }
}

step "double" {
  target = adapter.noop.default
  input {
    current = data.internal.counter.value
  }
  outcome "success" {
    next = state.done
    write {
      target = data.internal.counter.value
      value  = "2"
    }
  }
}

state "done" {
  terminal = true
  success  = true
}
```

## Key idioms

- **`data "internal" "name" { type = string value = "..." }`** — declares a workflow-scoped variable with an optional initial value.
- **`data.internal.<name>.value`** — reads the current value of the variable in any expression including step inputs.
- **`write { target = data.internal.<name>.value, value = output.<key> }`** — in an outcome block, evaluates `value` against the step output scope and atomically writes the result into the targeted data block.

## Common pitfalls

- **Parallel write races** — writing the same `data.internal` value from concurrent parallel iterations produces non-deterministic values; prefer sequential `for_each` when order matters.
- **Write semantics** — `write.value` is an expression evaluated against the step output scope (or any literal value), not a raw adapter output key name.

## See also

- [LANGUAGE-SPEC.md § data](../LANGUAGE-SPEC.md#data-kind-name--)
- [LANGUAGE-SPEC.md § Outcome model](../LANGUAGE-SPEC.md#outcome-model)
- [04-iteration-parallel.md](04-iteration-parallel.md) for cautions about shared state in parallel steps.
