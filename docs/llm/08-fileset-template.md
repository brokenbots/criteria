# Pattern: File-driven prompts

## When to use

Use when step inputs are stored as files (e.g., Markdown prompt templates)
and loaded at runtime. The `file()` function reads a file relative to the
workflow directory. Combined with `for_each`, it drives one step execution
per file.

## Minimal example

```hcl
workflow "file-prompts" {
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

step "process" {
  target   = adapter.shell.default
  for_each = ["./prompts/alpha.md", "./prompts/beta.md"]

  input {
    command = file(each.value)
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

- **`file(path)`** — reads the file at `path` relative to the workflow directory; the path must stay within the workflow directory or `CRITERIA_WORKFLOW_ALLOWED_PATHS`.
- **`for_each = [...]` with `file(each.value)`** — drives one adapter call per file path; extend the list to process more files.
- **`fileexists(path)`** — returns a bool; use it to guard optional file reads without causing a compile or runtime error.

## Common pitfalls

- **`feat-02` will replace this hand-written enumeration with `fileset()`** — see [feat-02-fileset-function.md](../../workstreams/feat-02-fileset-function.md). When `feat-02` lands, use `for_each = fileset("./prompts", "*.md")` instead of a hard-coded list.
- **Path confinement** — paths that escape the workflow directory (e.g., `../../etc/passwd`) are rejected at runtime with a security error; never construct paths from untrusted input.

## See also

- [LANGUAGE-SPEC.md § Functions](../LANGUAGE-SPEC.md#functions)
- [LANGUAGE-SPEC.md § Iteration semantics](../LANGUAGE-SPEC.md#iteration-semantics)
- [03-iteration-for-each.md](03-iteration-for-each.md) for the sequential iteration baseline.
