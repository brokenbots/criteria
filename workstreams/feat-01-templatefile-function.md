# feat-01 — `templatefile(path, vars)` HCL function

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** D (features) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** [doc-04-llm-prompt-pack.md](doc-04-llm-prompt-pack.md) example 8 may upgrade to use `templatefile()` once this lands.

## Context

Today the only file-reading function is `file(path)` ([workflow/eval_functions.go:106-146](../workflow/eval_functions.go#L106-L146)) which returns the file contents verbatim. LLM-driven workflows commonly want **interpolated** templates: a prompt file with placeholders that are filled in per step. The Terraform-style helper for this is `templatefile(path, vars)`:

```hcl
step "draft" {
  target = adapter.copilot.editor
  input {
    prompt = templatefile("prompts/draft.tmpl", {
      topic   = var.topic
      style   = local.tone,
      example = steps.outline.summary,
    })
  }
}
```

This workstream adds `templatefile(path, vars) → string`. The function:

- Reads the file at `path` using the **same path-confinement and size-cap machinery** as `file()` — reuse `resolveConfinedPath` ([workflow/eval_functions.go:265-292](../workflow/eval_functions.go#L265-L292)) and the `MaxBytes` cap.
- Renders the file content as a Go `text/template` template with `vars` (a cty object) as the data context.
- Returns the rendered string.

We use `text/template` (not `html/template`, not the HCL native template engine) for three reasons:

1. **Familiarity** — Go developers and Terraform users already know the `{{ .field }}` syntax.
2. **Simplicity** — `text/template` is in the stdlib, no new dependency.
3. **Predictability** — `text/template` does not auto-escape, which is what we want for prompt content.

This intentionally diverges from Terraform's `templatefile` (which uses HCL template syntax). The diverging choice is documented in the function's doc-comment so users coming from Terraform are not surprised.

## Prerequisites

- `make ci` green on `main`.
- The existing `file()` function is unchanged from [workflow/eval_functions.go:106](../workflow/eval_functions.go#L106) (path confinement, MaxBytes cap, UTF-8 validation). This workstream reuses that machinery; do not duplicate.
- Familiarity with `cty.Value.AsValueMap()` for converting cty objects to Go maps.

## In scope

### Step 1 — Implement `templatefile`

Edit [workflow/eval_functions.go](../workflow/eval_functions.go). Add to the `workflowFunctions` map at lines 98–104:

```go
return map[string]function.Function{
    "file":            fileFunction(opts),
    "fileexists":      fileExistsFunction(opts),
    "templatefile":    templatefileFunction(opts),   // NEW
    "trimfrontmatter": trimFrontmatterFunction(),
}
```

Add the implementation function (place it after `fileFunction` for grouping, before `fileExistsFunction`):

```go
// templatefileFunction implements templatefile(path, vars) → string.
//
// Reads the UTF-8 file at path (resolved relative to WorkflowDir using the
// same path-confinement and size-cap machinery as file()), then renders
// the file contents as a Go text/template template with vars as the data
// context. vars must be an object value; its attributes become the
// template's . fields.
//
// Note: this uses Go's text/template syntax (`{{ .field }}`), not HCL's
// native template syntax (`${field}`). This is an intentional divergence
// from Terraform's templatefile() — the rationale is text/template is in
// the stdlib and predictable for prompt content (no auto-escaping).
func templatefileFunction(opts FunctionOptions) function.Function {
    return function.New(&function.Spec{
        Params: []function.Parameter{
            {Name: "path", Type: cty.String},
            {Name: "vars", Type: cty.DynamicPseudoType, AllowNull: false},
        },
        Type: function.StaticReturnType(cty.String),
        Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
            if opts.WorkflowDir == "" {
                return cty.StringVal(""), fmt.Errorf("templatefile(): workflow directory not configured")
            }
            raw := args[0].AsString()
            varsVal := args[1]

            // Validate vars is an object (or map). Reject primitives and lists.
            ty := varsVal.Type()
            if !ty.IsObjectType() && !ty.IsMapType() {
                return cty.StringVal(""), fmt.Errorf(
                    "templatefile(): vars must be an object or map; got %s", ty.FriendlyName())
            }

            // Read file content via the same confinement + size cap as file().
            resolved, err := resolveConfinedPath(raw, opts.WorkflowDir, opts.AllowedPaths)
            if err != nil {
                // Replace "file()" prefix in error with "templatefile()" for clarity.
                return cty.StringVal(""), rewriteFuncName(err, "file()", "templatefile()")
            }
            info, err := os.Stat(resolved)
            if err != nil {
                return cty.StringVal(""), mapOSErrorWithFuncName(raw, err, "templatefile()")
            }
            if info.Size() > opts.MaxBytes {
                return cty.StringVal(""), fmt.Errorf(
                    "templatefile(): %q is %d bytes; max is %d (set CRITERIA_FILE_FUNC_MAX_BYTES to raise)",
                    raw, info.Size(), opts.MaxBytes)
            }
            data, err := os.ReadFile(resolved)
            if err != nil {
                return cty.StringVal(""), mapOSErrorWithFuncName(raw, err, "templatefile()")
            }
            if !utf8.Valid(data) {
                offset := invalidUTF8Offset(data)
                return cty.StringVal(""), fmt.Errorf(
                    "templatefile(): %q contains invalid UTF-8 at byte %d", raw, offset)
            }

            // Convert cty vars to Go map for text/template.
            ctxMap, err := ctyToGoMap(varsVal)
            if err != nil {
                return cty.StringVal(""), fmt.Errorf("templatefile(): converting vars: %w", err)
            }

            // Parse and render.
            // Template name is the basename of path so error messages reference
            // the source file.
            tmpl, err := template.New(filepath.Base(raw)).
                Option("missingkey=error").  // strict: missing key is an error, not "<no value>"
                Parse(string(data))
            if err != nil {
                return cty.StringVal(""), fmt.Errorf("templatefile(): %q parse: %w", raw, err)
            }
            var buf bytes.Buffer
            if err := tmpl.Execute(&buf, ctxMap); err != nil {
                return cty.StringVal(""), fmt.Errorf("templatefile(): %q execute: %w", raw, err)
            }
            return cty.StringVal(buf.String()), nil
        },
    })
}

// rewriteFuncName rewrites the prefix "<from>" to "<to>" in err's message.
// Used to retag errors from shared confinement helpers with the calling
// function's name (e.g. file()-prefixed errors become templatefile()-prefixed).
func rewriteFuncName(err error, from, to string) error {
    msg := err.Error()
    if strings.HasPrefix(msg, from) {
        return fmt.Errorf("%s%s", to, strings.TrimPrefix(msg, from))
    }
    return err
}

// mapOSErrorWithFuncName is like mapOSError but with a custom function-name prefix.
func mapOSErrorWithFuncName(raw string, err error, funcName string) error {
    base := mapOSError(raw, err)
    return rewriteFuncName(base, "file()", funcName)
}
```

If `mapOSError` already has a function-name parameter, use it directly; the `rewriteFuncName` helper is needed only because the existing helper is hardcoded to `file()`. Read [workflow/eval_functions.go](../workflow/eval_functions.go) `mapOSError` definition before adding the helper — if it already accepts a name param, drop `mapOSErrorWithFuncName`.

### Step 2 — Implement `ctyToGoMap`

Add to the same file:

```go
// ctyToGoMap converts a cty object or map value into a Go map[string]any
// suitable for text/template. Nested objects/maps recurse; lists become
// []any; primitives become string/int64/float64/bool. Null values become
// nil. Unknown values return an error (templatefile cannot meaningfully
// render an unknown).
func ctyToGoMap(v cty.Value) (map[string]any, error) {
    if !v.IsKnown() {
        return nil, fmt.Errorf("vars value is unknown")
    }
    if v.IsNull() {
        return nil, fmt.Errorf("vars must not be null")
    }
    out := make(map[string]any)
    it := v.ElementIterator()
    for it.Next() {
        k, val := it.Element()
        kStr := k.AsString()
        gv, err := ctyToGoValue(val)
        if err != nil {
            return nil, fmt.Errorf("key %q: %w", kStr, err)
        }
        out[kStr] = gv
    }
    return out, nil
}

// ctyToGoValue converts a single cty.Value to its Go-template equivalent.
func ctyToGoValue(v cty.Value) (any, error) {
    if !v.IsKnown() {
        return nil, fmt.Errorf("value is unknown")
    }
    if v.IsNull() {
        return nil, nil
    }
    ty := v.Type()
    switch {
    case ty == cty.String:
        return v.AsString(), nil
    case ty == cty.Bool:
        return v.True(), nil
    case ty == cty.Number:
        // Prefer int64 when representable; else float64.
        if i, acc := v.AsBigFloat().Int64(); acc == big.Exact {
            return i, nil
        }
        f, _ := v.AsBigFloat().Float64()
        return f, nil
    case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
        var out []any
        it := v.ElementIterator()
        for it.Next() {
            _, elem := it.Element()
            gv, err := ctyToGoValue(elem)
            if err != nil { return nil, err }
            out = append(out, gv)
        }
        return out, nil
    case ty.IsObjectType() || ty.IsMapType():
        return ctyToGoMap(v)
    default:
        return nil, fmt.Errorf("unsupported type: %s", ty.FriendlyName())
    }
}
```

Imports to add at the top of `eval_functions.go`:

```go
import (
    "bytes"
    // ... existing ...
    "math/big"
    "text/template"
)
```

### Step 3 — Update package doc-comment

Edit the package doc-comment at [workflow/eval_functions.go:1-4](../workflow/eval_functions.go#L1-L4):

```go
// eval_functions.go — HCL expression functions for workflow evaluation.
// Implements file(), fileexists(), templatefile(), and trimfrontmatter().
```

### Step 4 — Tests

New file: `workflow/eval_functions_templatefile_test.go`.

Required tests:

1. `TestTemplatefile_HappyPath_BasicSubstitution` — write a file with content `"hello {{ .name }}"`, call `templatefile("greeting.tmpl", { name = "world" })`, assert returned `cty.String` is `"hello world"`.

2. `TestTemplatefile_NestedFields` — content `"{{ .person.name }} is {{ .person.age }}"`; vars `{ person = { name = "Ada", age = 36 } }`; assert renders `"Ada is 36"`.

3. `TestTemplatefile_ListIteration` — content `"{{ range .items }}- {{ . }}\n{{ end }}"`; vars `{ items = ["a","b","c"] }`; assert renders `"- a\n- b\n- c\n"`.

4. `TestTemplatefile_BoolConditional` — content `"{{ if .ready }}go{{ else }}wait{{ end }}"`; vars `{ ready = true }`; assert renders `"go"`. Then with `ready = false` assert `"wait"`.

5. `TestTemplatefile_NumberFloat` — vars `{ pi = 3.14 }`; content `"{{ .pi }}"`; assert renders `"3.14"`.

6. `TestTemplatefile_NumberInt` — vars `{ n = 42 }`; content `"{{ .n }}"`; assert renders `"42"` (NOT `"42.0"`). This locks in the int-preferred conversion in `ctyToGoValue`.

7. `TestTemplatefile_NullValueRendersAsEmpty` — vars `{ x = null }`; content `"got: {{ .x }}"`; assert renders `"got: <nil>"` (Go's default for nil; document this in the function comment as the behavior).

8. `TestTemplatefile_MissingKey_ReturnsError` — vars `{ a = "x" }`; content `"{{ .b }}"`; assert error contains `"templatefile()"`, `"execute"`, and `"missingkey"` or similar (the strict `missingkey=error` mode triggers).

9. `TestTemplatefile_UnknownVar_ReturnsError` — vars contains `cty.UnknownVal(cty.String)`; assert error names "unknown".

10. `TestTemplatefile_NullVarsArg_ReturnsError` — `templatefile("x", null)`; assert error names "must not be null".

11. `TestTemplatefile_PrimitiveVarsArg_ReturnsError` — `templatefile("x", "not a map")`; assert error names "object or map".

12. `TestTemplatefile_FileNotFound_ReturnsError` — call with a non-existent path; assert error contains `"templatefile()"` and `"no such file"`.

13. `TestTemplatefile_PathEscape_ReturnsError` — `templatefile("../escape.tmpl", {})`; assert error contains `"templatefile()"` and `"escapes workflow directory"`.

14. `TestTemplatefile_AbsolutePath_Rejected` — `templatefile("/etc/passwd", {})`; assert error names absolute-path rejection.

15. `TestTemplatefile_OverSizeCap_ReturnsError` — write a file larger than `opts.MaxBytes` (use a tiny `MaxBytes` like 1 KiB in test setup); assert error names size and `"max is"`.

16. `TestTemplatefile_InvalidUTF8_ReturnsError` — write a file containing invalid UTF-8 bytes; assert error names "invalid UTF-8".

17. `TestTemplatefile_EmptyTemplate_ReturnsEmptyString` — empty file, any vars; assert returned `""`.

18. `TestTemplatefile_AllowedPathsHonored` — write a template outside `WorkflowDir` but inside an `AllowedPaths` entry; assert success. Mirrors `file()` behavior.

19. `TestTemplatefile_TemplateParseError_ReturnsError` — content `"{{ .unclosed"`; assert error contains `"parse"` and the path.

20. `TestTemplatefile_ConcurrentCalls_NoRace` — `t.Parallel()` 50 sub-tests each calling `templatefile` with a small template. Run under `-race`; no race expected.

Each test uses `t.TempDir()` for the workflow dir; constructs `FunctionOptions{ WorkflowDir: tmpDir, MaxBytes: 1024 }`; invokes the function via `templatefileFunction(opts).Call([]cty.Value{...})`.

### Step 5 — Validation example workflow

New directory: `examples/templatefile/`.

Files:

- `examples/templatefile/main.hcl`:
  ```hcl
  workflow "templatefile_demo" {
    version       = "1"
    initial_state = "render"
    target_state  = "done"
  }

  variable "topic" {
    type    = string
    default = "release notes"
  }

  adapter "shell" "echoer" {}

  step "render" {
    target = adapter.shell.echoer
    input {
      cmd = templatefile("prompts/intro.tmpl", { topic = var.topic })
    }
    outcome "success" { next = "done" }
  }

  state "done" { terminal = true success = true }
  ```

- `examples/templatefile/prompts/intro.tmpl`:
  ```
  echo "Welcome to {{ .topic }}!"
  ```

Add to the `Makefile` `validate` target:

```make
./bin/criteria validate examples/templatefile
```

### Step 6 — Documentation

Update [docs/workflow.md](../docs/workflow.md) — find the existing `file()` documentation (search for "## file()" or the equivalent heading). Add a sibling `## templatefile()` section with:

- Signature: `templatefile(path, vars) → string`
- One-paragraph description (template syntax is Go `text/template`, not HCL native; vars must be object or map; same path confinement and size cap as `file()`).
- A 4-line example.
- A "Differences from Terraform" callout: "Terraform's `templatefile` uses HCL native template syntax (`${field}`); Criteria's uses Go `text/template` syntax (`{{ .field }}`). This is intentional and documented for prompt-friendly rendering."
- Cross-link to the `file()` section.

The doc-03 generator (if it has landed) will pick up the new function automatically — no manual edit to `docs/LANGUAGE-SPEC.md` needed (run `make spec-gen` after this lands; the generator update is in feat-01's scope).

If `doc-03` has landed, run `make spec-gen` and commit the regenerated `docs/LANGUAGE-SPEC.md`.

### Step 7 — Validation

```sh
go test -race -count=2 ./workflow/...
go test -race -count=20 ./workflow/ -run Templatefile   # higher race-pressure on the new tests
make validate
make spec-check          # if doc-03 has landed
make ci
```

All five must exit 0.

## Behavior change

**Behavior change: yes — additive.** A new function `templatefile` is available in HCL expression contexts. Workflows that did not use the function are unaffected.

No proto change. No SDK change (the function is exposed only through HCL evaluation). No CLI flag change.

## Reuse

- [`fileFunction`](../workflow/eval_functions.go) — same `function.Spec` shape and error idioms.
- `resolveConfinedPath` ([workflow/eval_functions.go:265-292](../workflow/eval_functions.go#L265-L292)) — path confinement.
- `checkConfinement`, `isUnderDir` ([workflow/eval_functions.go:297-319](../workflow/eval_functions.go#L297-L319)) — same.
- `mapOSError` and `invalidUTF8Offset` (find in same file) — error mapping. If `mapOSError` already accepts a function-name parameter, use it; otherwise add `mapOSErrorWithFuncName` per Step 1.
- `opts.MaxBytes` size-cap convention.
- `os.Stat` / `os.ReadFile` / `utf8.Valid` patterns from `fileFunction`.
- Go stdlib `text/template`, `bytes`, `math/big`.
- Existing test fixtures pattern in `workflow/eval_functions_test.go` (if it exists; otherwise mirror `file()` test patterns).

## Out of scope

- HCL native template syntax (`${field}`). Use `text/template` (`{{ .field }}`). Documented divergence.
- Custom template functions (`funcs(map[string]any{...})`). The default Go `text/template` builtins (e.g. `printf`, `range`, `if`) are sufficient for v1; user-defined funcs are a separate workstream.
- `html/template`. We deliberately use `text/template` to avoid HTML auto-escaping in prompt strings.
- Caching of parsed templates across calls. Each call re-parses; performance is acceptable for the size cap.
- Streaming render. `templatefile` returns a single string.
- Recursive template imports / `{{ template }}` includes. Single-file only.
- Template-side I/O (e.g. a `{{ file "x" }}` builtin). Templates render with the provided vars only.
- Auto-converting cty number to specific Go numeric types beyond int64/float64. The two-tier conversion (int64 if exact, float64 else) is the contract.
- Modifying `file()` or `fileexists()` to share more code with `templatefile()`. The duplication in I/O is acceptable.

## Files this workstream may modify

- [`workflow/eval_functions.go`](../workflow/eval_functions.go) — add `templatefile` registration (line 98-104), implementation function, `ctyToGoMap`/`ctyToGoValue` helpers, optional `rewriteFuncName`/`mapOSErrorWithFuncName` helpers.
- New file: [`workflow/eval_functions_templatefile_test.go`](../workflow/) — Step 4 tests.
- New directory: [`examples/templatefile/`](../examples/) with `main.hcl` and `prompts/intro.tmpl`.
- [`Makefile`](../Makefile) — add `examples/templatefile` to `validate` target.
- [`docs/workflow.md`](../docs/workflow.md) — add `## templatefile()` section per Step 6.
- [`docs/LANGUAGE-SPEC.md`](../docs/LANGUAGE-SPEC.md) — re-run `make spec-gen` (no manual edit) if doc-03 has landed.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files.
- [`docs/plugins.md`](../docs/plugins.md) — not relevant.
- [`.golangci.yml`](../.golangci.yml), [`.golangci.baseline.yml`](../.golangci.baseline.yml).
- Files outside `workflow/`, `examples/templatefile/`, the Makefile, and the listed docs.

## Tasks

- [ ] Register `templatefile` in `workflowFunctions` (Step 1).
- [ ] Implement `templatefileFunction` and helpers (Step 1).
- [ ] Implement `ctyToGoMap` and `ctyToGoValue` (Step 2).
- [ ] Update package doc-comment (Step 3).
- [ ] Add 20 unit tests (Step 4).
- [ ] Add example workflow and wire into `make validate` (Step 5).
- [ ] Update `docs/workflow.md` (Step 6).
- [ ] Re-run `make spec-gen` if doc-03 has landed (Step 6).
- [ ] Validation (Step 7).

## Exit criteria

- `templatefile` is registered in `workflowFunctions` map.
- All 20 unit tests pass under `-race -count=20`.
- `examples/templatefile/` validates green.
- `docs/workflow.md` documents the function with the Terraform divergence callout.
- `docs/LANGUAGE-SPEC.md` (if doc-03 has landed) lists the function in the generated section.
- `make ci` exits 0.
- No new `//nolint` directives added.
- No baseline cap change required.

## Tests

The Step 4 list. Coverage of `templatefileFunction` ≥ 90%; coverage of `ctyToGoMap`/`ctyToGoValue` ≥ 85% (the helpers can have a default branch for unsupported cty types that is provably unreachable from valid inputs — exclude the unreachable branch from the coverage target if needed and document).

## Risks

| Risk | Mitigation |
|---|---|
| Users confused by Go-template-vs-HCL-template syntax difference | The doc-comment and `docs/workflow.md` callout state the divergence explicitly. The function name is identical to Terraform's; users who type-check at the syntax level will get a parse error from `text/template` and the error message names the file path. |
| `text/template`'s `missingkey=error` is too strict and breaks a workflow that intentionally references an optional key | Document that `missingkey=error` is the contract; users who want optional keys use `{{ if .x }}{{ .x }}{{ end }}`. Strict-by-default catches typos. |
| `ctyToGoValue` doesn't handle a cty type that arrives in the wild (e.g. cty capsule) | The default branch returns an error. Tests cover the common types; capsules are not produced by the workflow language so the unreachable branch is acceptable. |
| Large vars objects (e.g. a 10k-entry map) explode rendering time | The `MaxBytes` cap is on file size, not template-data size. If a workflow author passes a huge vars object, they own the consequences. Document in the function comment. |
| Templates with non-trivial logic (`range`, `if`) become hard to debug | Errors include the file path and Go template's line number context. Sufficient for the v1 surface. |
| The `rewriteFuncName` hack is ugly and fragile | If `mapOSError` already accepts a function-name parameter (read it first), drop the hack. Otherwise, the alternative is to extend `mapOSError` itself, which is out-of-scope refactoring; the hack is the bounded choice. |
| `examples/templatefile/` doesn't actually run end-to-end because shell adapter doesn't echo back the input | `criteria validate` only compiles, it does not run. The example proves the syntax compiles; runtime correctness is unit-tested in Step 4. |
