# Workstream 7 — `file()` expression function

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md), [W02](02-golangci-lint-adoption.md), [W04](04-split-oversized-files.md) · **Unblocks:** users who currently work around the gap with shell pre-steps. Source feedback: [user_feedback/01-support-file-function-user-story.txt](../user_feedback/01-support-file-function-user-story.txt).

## Context

Workflow authors cannot load file contents from HCL expressions
today. The expression evaluator
([workflow/eval.go](../workflow/eval.go)) registers no HCL
functions; only the `var`, `steps`, and `each` variables are
exposed. Authors who need agent profiles, prompts, or templates
have been adding shell pre-steps that `cat`/`awk` files into a
step output and then reference them from later steps.

This is forced hacky workflow: a shell adapter invocation just to
move bytes the workflow could load directly. It also crosses the
shell-adapter trust boundary
([W05](05-shell-adapter-sandbox.md))
unnecessarily — once W05's defaults land, those workarounds will
hit the env allowlist, command-path hygiene, and timeout
constraints, breaking workflows that have nothing to do with
shell.

This workstream adds a `file()` expression function to the HCL
evaluation context, plus two thin convenience helpers
(`fileexists()` and `trimfrontmatter()`) that the user story
explicitly calls out. The function is workspace-relative,
read-only, and validated at compile time where possible.

## Prerequisites

- [W04](04-split-oversized-files.md) merged — the workflow compile
  files are split, so adding compile-time validation lands in
  `compile_steps.go` (or `compile_validation.go`) rather than the
  1099-line monolith.
- `make ci` green on `main`.

## In scope

### Step 1 — Define semantics

The `file()` function:

- **Signature:** `file(path string) -> string`
- **Path resolution:** the argument is resolved relative to the
  HCL file's directory (the file in which the expression appears).
  This is the natural mental model — workflow authors think in
  terms of "the prompt file next to my workflow.hcl" — and avoids
  CWD-of-the-runner ambiguity.
- **Encoding:** UTF-8. The function returns the decoded string;
  invalid UTF-8 produces a runtime error with the path and byte
  offset of the first invalid sequence.
- **Size cap:** 1 MiB. Files larger than the cap produce a runtime
  error naming the cap and the file size. Override via the env
  var `CRITERIA_FILE_FUNC_MAX_BYTES` (positive integer; bounds:
  1024 to 64 MiB). The cap exists to protect the engine from a
  workflow that accidentally references a multi-GB log file.
- **Path confinement:** the resolved absolute path must remain
  under the HCL file's directory **or** under a path explicitly
  listed in `CRITERIA_WORKFLOW_ALLOWED_PATHS` (colon-separated
  env var, mirrors the convention from
  [W05](05-shell-adapter-sandbox.md)). Paths containing `..` after
  cleaning are rejected before any I/O happens.
- **Errors:**
  - File missing → `file(): no such file: <path>` (runtime).
  - Permission denied → `file(): permission denied: <path>`.
  - Path escape → `file(): path %q escapes workflow directory; add to CRITERIA_WORKFLOW_ALLOWED_PATHS to permit`.
  - Size cap exceeded → `file(): %q is %d bytes; max is %d (set CRITERIA_FILE_FUNC_MAX_BYTES to raise)`.
  - Invalid UTF-8 → `file(): %q contains invalid UTF-8 at byte %d`.

The `fileexists()` function:

- **Signature:** `fileexists(path string) -> bool`
- Same path resolution and confinement as `file()`.
- Returns `true` only if the path resolves to a regular file
  readable by the runner. Symlinks resolve and the target is
  what's checked. Directories return `false`. Errors other than
  "not exists" propagate (e.g. permission denied is an error,
  not `false`).

The `trimfrontmatter()` function:

- **Signature:** `trimfrontmatter(content string) -> string`
- Pure string function (no I/O). Detects YAML frontmatter
  (leading `---\n...---\n` block) and returns `content` with the
  frontmatter and the immediately following newline removed.
- If the input does not start with `---\n`, returns `content`
  unchanged.
- The closing `---\n` must occur within the first 64 KiB of the
  content; if not, the function returns the input unchanged
  (treats it as not-frontmatter rather than erroring).

`trimfrontmatter` is the cheap version of "load an `.agent.md`
and skip the YAML preamble" the user story flags as a
recurring need. A future workstream can add a richer set
(`yamlfrontmatter() -> object`, etc.); this one stays minimal.

Newline normalization is **not** in scope — agents that need
LF-only content can do it explicitly. Adding implicit
normalization makes the function harder to reason about.

### Step 2 — Implement the functions

Register the functions in
[workflow/eval.go](../workflow/eval.go) by extending
`BuildEvalContext` to populate `EvalContext.Functions`:

```go
return &hcl.EvalContext{
    Variables: ctxVars,
    Functions: workflowFunctions(opts),
}
```

`workflowFunctions(opts FunctionOptions) map[string]function.Function`
returns the three functions. `FunctionOptions` carries:

- `WorkflowDir string` — the directory of the HCL file being
  evaluated (used as the resolution base for `file()` and
  `fileexists()`).
- `MaxBytes int64` — the size cap, sourced from
  `CRITERIA_FILE_FUNC_MAX_BYTES` with the 1 MiB default.
- `AllowedPaths []string` — sourced from
  `CRITERIA_WORKFLOW_ALLOWED_PATHS`.

`BuildEvalContext` gains a sibling
`BuildEvalContextWithOpts(vars, opts)`. The bare
`BuildEvalContext(vars)` keeps backwards compatibility and
constructs default options (no allowed paths, default size cap,
empty workflow dir → file() always errors with a clear "workflow
directory not configured" message).

The compile path
([workflow/compile.go](../workflow/compile.go)) is the source
of `WorkflowDir` — it already has the HCL file path. Plumb the
directory through to wherever `BuildEvalContext` is called for
runtime evaluation.

The implementation lives in a new file:
`workflow/eval_functions.go`. Each of the three functions is
≤ 50 lines and includes the matching error mapping.

### Step 3 — Compile-time validation where possible

For `file()` calls whose argument is a constant string literal
(the common case — `prompt = file("./prompts/exec.md")`),
validate at compile time:

- Resolve the path against `WorkflowDir`.
- Run the path-confinement check.
- Stat the file; require it to exist and be readable.
- Do **not** read the file at compile time (size cap, UTF-8 check,
  and content are runtime concerns).

Compile-time errors surface as HCL diagnostics tied to the
expression's source range. Examples:

- `file("missing.md")` where `missing.md` doesn't exist next to
  the HCL file: error at compile time, with the source range of
  the literal.
- `file(var.path)` where `path` is dynamic: skip compile-time
  validation; runtime catches it.

Compile-time validation lives in `workflow/compile_steps.go`
(post-W04 location) or `workflow/compile_validation.go`. It hooks
into the existing input-expression validation pass.

### Step 4 — Tests

Tests live in `workflow/eval_functions_test.go` (new) and a
fixture directory `workflow/testdata/eval_functions/` (new).

**Unit tests** (`workflow/eval_functions_test.go`):

1. `file("hello.txt")` returns the file's UTF-8 content.
2. `file("missing.txt")` returns the no-such-file error.
3. `file("../escape.txt")` returns the path-escape error.
4. `file("../escape.txt")` with the parent dir in
   `CRITERIA_WORKFLOW_ALLOWED_PATHS` succeeds.
5. `file("big.txt")` (2 MiB fixture) errors with the size-cap
   message; with `CRITERIA_FILE_FUNC_MAX_BYTES=4194304`, succeeds.
6. `file("invalid_utf8.bin")` (deliberately-malformed fixture)
   errors with the UTF-8 byte offset.
7. `fileexists("hello.txt")` returns `true`.
8. `fileexists("missing.txt")` returns `false`.
9. `fileexists("subdir/")` returns `false` (directory, not a
   regular file).
10. `trimfrontmatter("---\nfoo: 1\n---\nbody\n")` returns
    `"body\n"`.
11. `trimfrontmatter("no frontmatter\n")` returns the input
    unchanged.
12. `trimfrontmatter("---\nopen but never closed...\n" + 100KiB body)`
    returns the input unchanged (no closing `---` within 64 KiB).

**Compile-time tests** (`workflow/compile_file_function_test.go`):

13. A workflow whose step input contains `prompt =
    file("missing.md")` fails `Compile` with a diagnostic
    naming the file and the expression's source range.
14. A workflow whose step input contains `prompt =
    file(var.dynamic)` compiles successfully (dynamic argument
    skips compile-time check).

**Integration tests** (extend
`internal/cli/testdata/compile/` with a new golden if helpful;
extend `make validate` corpus with a new example):

15. New example `examples/file_function.hcl` that loads a prompt
    from a sibling file and runs to completion. `make validate`
    passes; running it via `./bin/criteria apply` produces the
    expected output.

### Step 5 — Document

Update **`docs/workflow.md`** with a new "Expression functions"
section listing the three functions, their signatures, semantic
contract, and the env-var configuration knobs.

Add an example file under `examples/`:
`examples/file_function.hcl` with a sibling
`examples/file_function_prompt.md` it loads. The example is
intentionally minimal — one step, one `file()` call — so it
serves as a copy-paste template.

If [W05](05-shell-adapter-sandbox.md)'s working-directory
confinement convention has shipped first, cross-link the
allowed-paths convention from `docs/workflow.md` to
`docs/security/shell-adapter-threat-model.md`.

## Out of scope

- Other expression functions (e.g. `env()`, `templatefile()`,
  `jsondecode()`, `yamldecode()`). Each is its own user-story
  follow-up; this workstream ships exactly three.
- Implicit newline normalization in `file()` or
  `trimfrontmatter()`.
- Writing files from expressions. `file()` is read-only by
  design.
- Recursive frontmatter or non-YAML frontmatter formats.
- Caching `file()` results across iterations of `for_each`. The
  function reads on every call; that is fine for the file sizes
  in scope.
- Watching files for changes during a long-running workflow.

## Files this workstream may modify

**Created:**

- `workflow/eval_functions.go`
- `workflow/eval_functions_test.go`
- `workflow/compile_file_function_test.go`
- `workflow/testdata/eval_functions/hello.txt`
- `workflow/testdata/eval_functions/big.txt` (2 MiB; deterministic
  content)
- `workflow/testdata/eval_functions/invalid_utf8.bin`
- `workflow/testdata/eval_functions/subdir/.gitkeep`
- `examples/file_function.hcl`
- `examples/file_function_prompt.md`

**Modified:**

- `workflow/eval.go` (extend `BuildEvalContext` /
  `EvalContext.Functions`; add `BuildEvalContextWithOpts`)
- `workflow/compile.go` and/or
  `workflow/compile_validation.go` (post-W04) — compile-time
  `file()` validation hook
- Whichever caller currently invokes `BuildEvalContext` — plumb
  `WorkflowDir` through (likely
  `workflow/compile_steps.go` and the engine's runtime
  evaluation site)
- `docs/workflow.md`
- `.golangci.baseline.yml` (only to remove entries this
  workstream's tests cover)

This workstream may **not** edit `README.md`, `PLAN.md`,
`AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any
other workstream file. CHANGELOG entries are deferred to
[W10](10-phase1-cleanup-gate.md).

## Tasks

- [ ] Implement `file()`, `fileexists()`, `trimfrontmatter()` per
      Step 2.
- [ ] Plumb `WorkflowDir` through to every
      `BuildEvalContext` call site.
- [ ] Add compile-time validation for constant-literal `file()`
      arguments per Step 3.
- [ ] Add the 15 tests listed in Step 4.
- [ ] Add the example workflow + sibling prompt file.
- [ ] Update `docs/workflow.md`.
- [ ] `make ci`, `make lint-go`, `make test-conformance`,
      `make validate` all green.
- [ ] CLI smoke: `./bin/criteria apply examples/file_function.hcl`
      exits 0 and produces the expected log output.

## Exit criteria

- The three functions are registered in `BuildEvalContext` and
  available in every input-expression context.
- Compile-time validation surfaces missing-file errors with HCL
  source ranges for constant-literal `file()` arguments.
- The 15 tests pass under `go test -race ./workflow/...`.
- `examples/file_function.hcl` validates and runs to completion.
- `docs/workflow.md` documents all three functions and their
  env-var knobs.
- Path confinement and size cap are tested with both the default
  and the env-var override paths.
- No new entries in `.golangci.baseline.yml` from this
  workstream's diff.

## Tests

15 tests listed verbatim in Step 4. All must run in `make test`
and gate CI. The integration test (15) runs via `make validate`.

## Risks

| Risk | Mitigation |
|---|---|
| Path confinement is too tight and rejects legitimate cases (sibling dir, monorepo root) | `CRITERIA_WORKFLOW_ALLOWED_PATHS` is the documented escape valve. The default is restrictive on purpose; widening defaults later is easier than narrowing them. |
| Plumbing `WorkflowDir` through every caller is invasive | The plumbing is one extra parameter on `BuildEvalContext`. The new `BuildEvalContextWithOpts` keeps the old signature working for callers that don't need `file()`; they get a clear error if `file()` is invoked without a configured directory. |
| Compile-time validation reads files during `criteria validate` and slows it down on large workflow trees | `Stat` only, no read. Even on a workflow with hundreds of `file()` calls, this is sub-millisecond. |
| `trimfrontmatter` semantics drift from common YAML expectations | The function is intentionally minimal — it strips the leading `---...---` block, nothing more. Authors who need full YAML decoding wait for a future `yamldecode()` function. The doc explicitly notes this. |
| Authors invoke `file()` on secrets and embed them in event logs | `file()` returns a string; whether it is logged is the workflow author's choice. The threat model from [W05](05-shell-adapter-sandbox.md) covers the related concern; if `file()` becomes a common secret-exfiltration vector, add a `sensitive = true` annotation in a follow-up workstream. Not in scope here. |
| Size cap of 1 MiB is too small for some prompt files | `CRITERIA_FILE_FUNC_MAX_BYTES` raises it up to 64 MiB. The cap exists to catch accidental references (log files, binaries), not to limit deliberate use. |
| The 2 MiB `big.txt` fixture bloats the repo | Generate it deterministically in `TestMain` (write the fixture before tests run, delete after). The fixture lives under `t.TempDir()`-managed paths in tests, not in `workflow/testdata/`. Adjust Step 4 accordingly during implementation; the test list stays the same. |
| `file()` resolves symlinks and an attacker-controlled symlink in the workflow dir escapes confinement | Path confinement uses `filepath.EvalSymlinks` then `filepath.Clean` then a prefix check against the allowed roots. Document this behavior; cover with a test if the platform supports symlink creation in tests (skip on Windows if necessary). |
