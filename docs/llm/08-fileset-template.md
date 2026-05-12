# Pattern: File-driven prompts with `fileset()`

## When to use

Use when step inputs are stored as files (Markdown prompts, config snippets)
and you want the workflow to enumerate them automatically. `fileset(path, pattern)`
lists regular files matching a glob and returns sorted relative paths ready to
pass to `file()` or `templatefile()` via `each.value`. Add or remove files in
the directory and the workflow adapts without editing HCL.

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
  for_each = fileset("prompts", "*.md")

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

- **`fileset(path, pattern)`** — lists regular files in `path` matching `pattern` (Go `filepath.Match`); returns a sorted `list(string)` of paths relative to the workflow directory.
- **`for_each = fileset(...)`** — one adapter call per matched file; `each.value` is the relative path.
- **`file(each.value)`** — reads the file for the current iteration; no path adjustment needed.
- **`trimfrontmatter(file(each.value))`** — compose with `trimfrontmatter` to strip YAML front matter from Markdown prompts.

## Common pitfalls

- **No recursive globbing** — `**` is not supported; `fileset` only lists files directly inside `path`.
- **Symlinks excluded** — only regular files are returned (`entry.Type().IsRegular()`).
- **Missing directory is an error** — unlike Terraform, Criteria errors when `path` does not exist.
- **Lexicographic sort** — `a1, a10, a2` not `a1, a2, a10`. Post-process if natural sort is needed.
- **Path confinement** — paths escaping the workflow directory are rejected with a security error.

## See also

- [LANGUAGE-SPEC.md § Functions](../LANGUAGE-SPEC.md#functions)
- [LANGUAGE-SPEC.md § Iteration semantics](../LANGUAGE-SPEC.md#iteration-semantics)
- [03-iteration-for-each.md](03-iteration-for-each.md) for the sequential iteration baseline.


