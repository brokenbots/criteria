# Pattern: Sequential iteration (for_each)

## When to use

Use this pattern when you need to run the same step against every item in a known list, one item at a time, in order. The step body runs once per element; the workflow continues only after all iterations complete.

## Minimal example

```hcl
workflow "for_each_iteration" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "process" {
  target   = adapter.shell.default
  for_each = ["a", "b", "c"]
  input {
    command = "echo 'item ${each.value} (index ${each._idx})'"
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
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

- **`for_each = [...]`** — accepts a literal list of strings; the step body executes once per element.
- **`each.value`** — the current element's value; use inside `input` expressions.
- **`each._idx`** — zero-based position of the current element in the list.
- **`outcome "all_succeeded"`** / **`"any_failed"`** — for_each steps use these outcome names instead of `"success"` / `"failure"`.

## Common pitfalls

- **Wrong outcome names** — for_each steps require `"all_succeeded"` and `"any_failed"`, not `"success"` / `"failure"`; using the scalar names is a compile error.
- **`each.value` outside for_each** — `each` is only in scope inside a `for_each` or `parallel` step.

## See also

- [LANGUAGE-SPEC.md § for_each](../LANGUAGE-SPEC.md#for_each)
- [04-iteration-parallel.md](./04-iteration-parallel.md) — run iterations concurrently instead of sequentially.
