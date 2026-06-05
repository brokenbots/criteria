# WS06 — `--var-file` CLI support and universal `.chcl` file extension

**Phase:** Language Cleanup · **Track:** CLI · **Owner:** Workstream executor · **Depends on:** none (standalone CLI changes). · **Unblocks:** large-workflow variable management; criteria-native tooling using `.chcl` extension. · **Base branch:** `main`

## Context

Two related CLI improvements land together because they both touch the file-loading layer and share a single central extension registry:

1. **`--var-file` flag.** The existing `--var key=value` flag works but is cumbersome for large variable sets — switching between adapters or model providers typically requires 4+ variables. A `--var-file` flag (JSON or `.chcl`/`.hcl` format) mirrors Terraform's `--var-file` and lets authors maintain named override files per environment.

2. **Universal `.chcl` extension.** `.chcl` is the criteria-native file extension, introduced to enable criteria-specific tooling (syntax highlighting, LSP, file-type associations) without colliding with generic `.hcl` files. `.hcl` remains supported for compatibility. The supported extension set is defined in one place and applied everywhere the tool currently accepts `.hcl` files — workflow files, var-files, and any future file-loading code — so updating the extension as the project settles on a convention is a one-line change.

## Prerequisites

- None. This workstream is independent of WS03–WS05.

## In scope

### Step 1 — Central extension registry

**New file:** `internal/cli/filetypes.go`

```go
package cli

// HCLExtensions lists the file extensions the tool recognises as HCL.
// .chcl is the criteria-native extension; .hcl is accepted for compatibility.
// To change the canonical extension, update this slice.
var HCLExtensions = []string{".chcl", ".hcl"}
```

All other steps reference `HCLExtensions` from this one location. No extension string is hardcoded elsewhere in the CLI layer.

### Step 2 — Universal `.chcl` recognition for workflow files

Audit all sites in `internal/cli/` (and `internal/run/` if applicable) where workflow file paths are accepted or validated by extension. Replace any hardcoded `.hcl` string comparisons or assumptions with a check against `HCLExtensions`.

Common patterns to update:
- File-extension validation in command argument handling (e.g. "file must have .hcl extension" error messages).
- File glob or discovery code that filters by `*.hcl`.
- Help text and usage strings that mention `.hcl` — update to list `.chcl` and `.hcl`.

The HCL parser itself does not care about extension; only criteria's own validation and discovery code needs updating.

### Step 3 — `--var-file` flag

**File:** [internal/cli/apply.go](../../internal/cli/apply.go) (mirror in `run.go` and `plan.go` if they also expose `--var`)

Add the flag alongside `--var`:

```go
cmd.Flags().StringArrayVar(&opts.varFiles, "var-file", nil,
    "Load variable overrides from a .chcl, .hcl, or .json file (repeatable; --var takes precedence)")
```

Add `varFiles []string` to the relevant options struct.

### Step 4 — `parseVarFile` parser

**File:** [internal/cli/env.go](../../internal/cli/env.go)

Add `parseVarFile(path string) (map[string]string, error)`:

- Detect format by extension using `HCLExtensions`:
  - Any extension in `HCLExtensions` (`.chcl`, `.hcl`) → parse as HCL using `github.com/hashicorp/hcl/v2/hclsimple` with a flat `key = "value"` schema.
  - `.json` → unmarshal with `encoding/json` into `map[string]string`.
  - Anything else → return a clear error listing supported extensions.
- File format (HCL): flat top-level attributes matching the `key=value` shape of `--var`.
- File format (JSON): `{ "key": "value" }` flat object.
- Return `map[string]string` matching `parseVarOverrides` output shape so merge logic is trivial.

### Step 5 — Merge precedence

**File:** [internal/cli/apply.go](../../internal/cli/apply.go) (and mirrored commands)

After parsing, merge in order (last wins within each group, `--var` wins over all files):

1. Evaluate `--var-file` flags left-to-right; later files overwrite earlier ones.
2. Apply `--var` overrides on top (highest precedence).

```go
merged := map[string]string{}
for _, path := range opts.varFiles {
    fileVars, err := parseVarFile(path)
    if err != nil { return err }
    for k, v := range fileVars { merged[k] = v }
}
for k, v := range parseVarOverrides(opts.varOverrides) {
    merged[k] = v
}
// use merged instead of parseVarOverrides(opts.varOverrides) going forward
```

### Step 6 — Tests

**`internal/cli/env_test.go`** (new or existing):
- `parseVarFile` with a `.json` file loads key/value pairs correctly.
- `parseVarFile` with a `.chcl` file loads key/value pairs correctly.
- `parseVarFile` with a `.hcl` file loads key/value pairs correctly (compatibility).
- `parseVarFile` with an unsupported extension returns an error listing supported extensions.
- `parseVarFile` with a non-existent path returns a clear error.
- `parseVarFile` with a malformed file returns a clear error.
- Merge precedence: `--var foo=cli` overrides `--var-file` entry `foo=file`.
- Merge precedence: later `--var-file` overrides earlier `--var-file` entry with same key.

**Integration**:
- A workflow invocation with `--var-file` loads variables and the workflow executes correctly.
- `criteria run --var-file vars.chcl workflow.chcl` is accepted end-to-end.

## Out of scope

- HCL var-file with nested objects or non-string types (initial support is flat string map matching `--var` semantics; type coercion happens downstream in `ApplyVarOverrides` the same way `--var` values are coerced today).
- Auto-discovery of var-files by convention (e.g. `workflow.chcl.auto.chcl`) — explicit `--var-file` only.
- Changing the `.hcl` extension on existing example or workflow files — `.chcl` is accepted going forward; existing `.hcl` files are not renamed.

## Reuse pointers

- Existing [`parseVarOverrides`](../../internal/cli/env.go) — same output shape; merge logic wraps both.
- Existing [`ApplyVarOverrides`](../../workflow/eval.go#L286) — unchanged; receives the merged map.
- `github.com/hashicorp/hcl/v2/hclsimple` — already a project dependency.

## Behavior change

**User-facing:** `--var-file path` is now a valid flag on `apply`, `run`, and `plan` commands. `.chcl` files are accepted everywhere `.hcl` files were.

**Existing workflows:** no change. All `.hcl` files continue to work; `--var` flag behavior is unchanged.

## Tests required

- All existing tests pass.
- New tests in Step 6 pass.
- `go vet ./...` clean.
- Manual: `criteria run --var-file examples/vars.chcl examples/hello_world.chcl` executes successfully.
- Manual: `criteria run --var-file a.chcl --var-file b.chcl --var key=override` applies overrides in the correct precedence order.

## Implementation Notes

### Checklist

- [ ] Step 1 — `internal/cli/filetypes.go` with `HCLExtensions`
- [ ] Step 2 — Universal `.chcl` recognition in workflow file loading
- [ ] Step 3 — `--var-file` flag added to `apply.go` (and `run.go`, `plan.go`)
- [ ] Step 4 — `parseVarFile` in `env.go`
- [ ] Step 5 — Merge precedence logic
- [ ] Step 6 — Tests

### Reviewer Notes

_To be filled in during review._
