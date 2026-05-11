# Pattern: Subworkflow call

## When to use

Use when a reusable unit of work can be encapsulated in its own workflow
module. The parent passes typed inputs; the child exposes named outputs that
the parent can read via `steps.<name>.<output_key>`.

## Minimal example

```hcl
workflow "subwf-parent" {
  version       = "1"
  initial_state = "prepare"
  target_state  = "done"
}

adapter "noop" "default" {}

subworkflow "process_one" {
  source = "./child"
  input = {
    item = "example"
  }
}

step "prepare" {
  target = adapter.noop.default
  outcome "success" { next = "invoke" }
  outcome "failure" { next = "failed" }
}

step "invoke" {
  target = subworkflow.process_one
  outcome "success" { next = "finish" }
  outcome "failure" { next = "failed" }
}

step "finish" {
  target = adapter.noop.default
  input {
    processed = steps.invoke.result
  }
  outcome "success" { next = "done" }
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

- **`subworkflow "name" { source = "./path" }`** — declares a child module; `source` is a relative path to a directory containing `.hcl` files.
- **`input = { key = expr }`** — static inputs to the child; the child receives them as `variable` bindings.
- **`target = subworkflow.<name>`** — routes a step to execute the declared subworkflow.
- **`steps.<invoke_step>.<output_key>`** — reads a named `output` value exported by the child after it completes.

## Common pitfalls

- **Relative path resolution** — `source` is resolved from the parent's directory; absolute paths are allowed but fragile across environments.
- **Variable type mismatch** — inputs passed via `input = {...}` must match the child's declared `variable` types or compilation fails.

## See also

- [LANGUAGE-SPEC.md § subworkflow](../LANGUAGE-SPEC.md#subworkflow-name--)
- [LANGUAGE-SPEC.md § step](../LANGUAGE-SPEC.md#step-name--)
