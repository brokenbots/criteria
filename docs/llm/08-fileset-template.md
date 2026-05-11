# Pattern: File-driven prompts

## When to use

Use this pattern when a workflow must process a set of files — for example, iterating over a directory of prompt templates and running one step per file. The file list is provided as a literal array until `fileset()` lands in a future release.

## Minimal example

```hcl
workflow "file_driven_prompts" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config { }
}

step "process" {
  target   = adapter.shell.default
  for_each = ["prompts/hello.md"]
  input {
    command = "cat '${each.value}'"
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

- **`for_each = ["path/to/file.md"]`** — pass file paths as strings; `each.value` holds the path inside `input`.
- **`cat '${each.value}'`** — reads the file content via the shell adapter; combine with a copilot adapter to feed file content into an LLM prompt.
- **`each._idx`** — zero-based index useful for ordering output across iterations.

## Common pitfalls

- **`feat-02` will replace this hand-written enumeration with `fileset()`** — see [feat-02-fileset-function.md](../../workstreams/feat-02-fileset-function.md). When that workstream lands, this example will be upgraded to `for_each = fileset("prompts", "*.md")`.
- **Relative paths** — file paths in `for_each` are resolved relative to the workflow directory; ensure fixture files exist at the declared paths.

## See also

- [LANGUAGE-SPEC.md § for_each](../LANGUAGE-SPEC.md#for_each)
- [03-iteration-for-each.md](./03-iteration-for-each.md) — the sequential iteration pattern this extends.
