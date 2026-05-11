# Pattern: Linear pipeline

## When to use

Use this pattern when every step runs in a fixed sequence and each step feeds its output directly into the next. Ideal for fetch → transform → publish pipelines where no branching, looping, or approval is needed.

## Minimal example

```hcl
workflow "linear_pipeline" {
  version       = "0.1"
  initial_state = "fetch"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "fetch" {
  target = adapter.shell.default
  input {
    command = "echo 'data-123'"
  }
  outcome "success" { next = "transform" }
  outcome "failure" { next = "failed" }
}

step "transform" {
  target = adapter.shell.default
  input {
    command = "echo 'transformed: ${steps.fetch.stdout}'"
  }
  outcome "success" { next = "publish" }
  outcome "failure" { next = "failed" }
}

step "publish" {
  target = adapter.shell.default
  input {
    command = "echo 'result: ${steps.transform.stdout}'"
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

- **`steps.<name>.stdout`** — references the adapter's captured stdout from an already-completed step; use in the next step's `input` block.
- **`outcome "success"` / `"failure"`** — every step must declare both; omitting either is a compile error.
- **`initial_state` / `target_state`** — marks entry point and the terminal success state the engine aims for.

## Common pitfalls

- **Missing outcome arm** — both `"success"` and `"failure"` are required on every step; the compiler rejects workflows that omit one.
- **Forward reference** — `steps.<name>` only resolves outputs from steps that have already completed; referencing a step that hasn't run yet causes a runtime error.

## See also

- [LANGUAGE-SPEC.md § steps](../LANGUAGE-SPEC.md#steps)
- [02-branching-switch.md](./02-branching-switch.md) — add one-of-N routing to a linear workflow.
