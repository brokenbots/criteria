# Pattern: Subworkflow call

## When to use

Use this pattern to break a large workflow into reusable units. A `subworkflow` declaration references an external workflow directory; a step targets it like any adapter. The child workflow's `output` blocks become accessible on the calling step's result map.

## Minimal example

```hcl
workflow "subworkflow_call" {
  version       = "0.1"
  initial_state = "run_process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

subworkflow "process_one" {
  source = "./subworkflows/process_one"
}

step "run_process" {
  target = subworkflow.process_one
  input {
    item = "hello"
  }
  outcome "success" { next = "report" }
  outcome "failure" { next = "failed" }
}

step "report" {
  target = adapter.shell.default
  input {
    command = "echo 'result: ${steps.run_process.result}'"
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

The child workflow at `./subworkflows/process_one/main.hcl` declares `variable "item"` and `output "result"`.

## Key idioms

- **`subworkflow "<name>" { source = "..." }`** — declares the reference; `source` is a path relative to the calling workflow directory.
- **`target = subworkflow.<name>`** — routes a step to the child workflow instead of an adapter.
- **`input { key = expr }`** — passes values to the child's declared variables at call time.
- **`steps.<name>.<output>`** — captures values from the child's `output` declarations.

## Common pitfalls

- **Missing variable default** — child variables without a `default` must either be bound in the `subworkflow` declaration's `input = {}` or have a default value; the compiler rejects otherwise.
- **`outcome "success"` maps to terminal state** — a child workflow's `state { success = true }` produces `"success"` on the parent step; `success = false` produces `"failure"`.

## See also

- [LANGUAGE-SPEC.md § subworkflow](../LANGUAGE-SPEC.md#subworkflow)
- [01-linear.md](./01-linear.md) — flat alternative when reuse is not needed.
