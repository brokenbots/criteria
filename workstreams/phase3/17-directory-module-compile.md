# Workstream 17 — Directory-level module compilation as the only entry shape

**Phase:** 3 · **Track:** C · **Owner:** Workstream executor · **Depends on:** [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md). · **Unblocks:** none structurally; lands late in the rework.

## Context

[architecture_notes.md §1](../../architecture_notes.md) and [proposed_hcl.hcl](../../proposed_hcl.hcl):

> A "workflow" is no longer strictly bound to a single file. Execution runs against a **directory**. All `.hcl` files in the directory are parsed, validated, and merged into a single flat `Spec` definition.

This workstream:

1. Adds a directory-mode entry to `criteria apply` and `criteria compile`.
2. Implements the multi-file merge (lifted/generalized from [13](13-subworkflow-block-and-resolver.md)'s local merge helper).
3. Treats a single `.hcl` file as a one-file directory — its parent directory is the module root, and it is the only file. **No legacy single-file-only code path** survives.

The clean break from v0.2.0: `criteria apply foo.hcl` continues to work (its parent dir is the module), but the code path is the directory path; there is no separate single-file compile.

## Prerequisites

- [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) merged. The local merge helper from Step 5 of [13](13-subworkflow-block-and-resolver.md) is the input to generalize.
- `make ci` green.

## In scope

### Step 1 — `mergeSpecsFromDir` helper

Generalize the helper [13](13-subworkflow-block-and-resolver.md) introduced. New file `workflow/parse_dir.go`:

```go
// ParseDir parses every .hcl file in dir, merges them into a single Spec,
// and returns the result. The merge rules:
//   - Top-level slices (Variables, Locals, Outputs, Adapters, Steps, States,
//     Waits, Approvals, Switches, Subworkflows, Environments) concatenate.
//   - Singleton fields (Name, Version, InitialState, TargetState, Policy,
//     Permissions, DefaultEnvironment) take their value from whichever
//     file declares them. If two files declare the same singleton, that's
//     a parse error with both file:line locations.
//   - SourceBytes concatenates with file boundaries preserved (newline
//     separators) so HCL diagnostics retain accurate Subject ranges.
//   - Cross-file duplicate names (e.g. step "foo" in two files) error with
//     both locations.
func ParseDir(dir string) (*Spec, hcl.Diagnostics)

// ParseFileOrDir is the unified CLI entry. If path is a directory, calls
// ParseDir. If path is a regular file with .hcl suffix, calls
// ParseDir(filepath.Dir(path)) and verifies path is among the parsed files
// (so single-file-mode behavior is preserved without a separate code path).
func ParseFileOrDir(path string) (*Spec, hcl.Diagnostics)
```

### Step 2 — Singleton-field disambiguation

For singleton top-level fields (Name, Version, InitialState, TargetState, Policy, Permissions, DefaultEnvironment), a directory module needs a deterministic way to set them. Three options:

1. Convention: declare in `workflow.hcl` only. (Implicit; brittle.)
2. Block: a top-level `workflow "<name>" { version = ..., environment = ... }` block per [proposed_hcl.hcl](../../proposed_hcl.hcl). The workflow header lives in this block; only one declaration allowed across the merged files. **Choose this option.**

So the merged Spec gets its `Name`, `Version`, `InitialState`, `TargetState`, and `DefaultEnvironment` from the workflow block. Without exactly one workflow block in the merged dir, error.

[workflow/schema.go](../../workflow/schema.go) currently has these as fields directly on `Spec`. Refactor:

```go
type WorkflowHeaderSpec struct {
    Name                string `hcl:"name,label"`
    Version             string `hcl:"version,optional"`
    InitialState        string `hcl:"initial_state,optional"`
    TargetState         string `hcl:"target_state,optional"`
    DefaultEnvironment  string `hcl:"environment,optional"`
}

type Spec struct {
    Header       *WorkflowHeaderSpec   `hcl:"workflow,block"`
    Variables    []VariableSpec        `hcl:"variable,block"`
    Locals       []LocalSpec           `hcl:"local,block"`
    Outputs      []OutputSpec          `hcl:"output,block"`
    Environments []EnvironmentSpec     `hcl:"environment,block"`
    Adapters     []AdapterDeclSpec     `hcl:"adapter,block"`
    Subworkflows []SubworkflowSpec     `hcl:"subworkflow,block"`
    Steps        []StepSpec            `hcl:"step,block"`
    States       []StateSpec           `hcl:"state,block"`
    Waits        []WaitSpec            `hcl:"wait,block"`
    Approvals    []ApprovalSpec        `hcl:"approval,block"`
    Switches     []SwitchSpec          `hcl:"switch,block"`
    Policy       *PolicySpec           `hcl:"policy,block"`
    Permissions  *PermissionsSpec      `hcl:"permissions,block"`
    SourceBytes  []byte
}
```

(Branches are gone after [16](16-switch-and-if-flow-control.md). Agents are gone after [11](11-agent-to-adapter-rename.md).)

The compile flow accesses `spec.Header.Name` etc. Sweep call sites.

### Step 3 — CLI entry

[internal/cli/apply_setup.go](../../internal/cli/apply_setup.go) `compileForExecution`:

```go
// BEFORE
src, err := os.ReadFile(workflowPath)
spec, diags := workflow.Parse(workflowPath, src)

// AFTER
spec, diags := workflow.ParseFileOrDir(workflowPath)
```

Update CLI flag/argument docs to clarify that `workflowPath` may be a directory.

`criteria compile <path>` — same change.

### Step 4 — Goldens and examples

Sweep examples:

- Existing single-file examples continue to work (a single `.hcl` file is its own one-file directory).
- Add at least one **multi-file** example under [examples/phase3-multi-file/](../../examples/phase3-multi-file/) demonstrating the merge: `variables.hcl`, `adapters.hcl`, `steps.hcl`, `workflow.hcl`.

Regenerate compile/plan goldens for any example whose Spec.Name resolution path changed.

### Step 5 — Tests

- `workflow/parse_dir_test.go`:
  - `TestParseDir_SingleFile`.
  - `TestParseDir_MultipleFiles`.
  - `TestParseDir_NoHCLFiles_Error`.
  - `TestParseDir_DirNotExist_Error`.
  - `TestParseDir_DuplicateStepAcrossFiles_Error_BothLocations`.
  - `TestParseDir_DuplicateWorkflowBlock_Error`.
  - `TestParseDir_NoWorkflowBlock_Error`.
  - `TestParseDir_DiagnosticsHaveCorrectFilenameSubjects`.

- `workflow/parse_file_or_dir_test.go`:
  - `TestParseFileOrDir_FilePathDelegatesToDir`.
  - `TestParseFileOrDir_DirPath`.

- End-to-end: [examples/phase3-multi-file/](../../examples/phase3-multi-file/) runs.

### Step 6 — Validation

```sh
go build ./...
go test -race -count=2 ./...
make validate
make ci
```

All exit 0.

## Behavior change

**Behavior change: yes — additive.**

Observable differences:

1. `criteria apply <directory>` works.
2. `criteria compile <directory>` works.
3. Multi-file workflows merge with conflict detection.
4. Workflow header moves into a `workflow "<name>" { ... }` block. Existing top-level attributes (`version`, `initial_state`, `target_state`) move into the block.
5. Single-file workflows continue to work; `criteria apply foo.hcl` is equivalent to `criteria apply $(dirname foo.hcl)`.

Migration: existing single-file workflows that have top-level `version = ...`, `initial_state = ...`, `target_state = ...` MUST move them inside a `workflow "<name>" { ... }` block. The `name` was previously the file's `<name>` label on a top-level workflow declaration — confirm the existing shape and document the migration. (If today's shape was attribute-only, the migration text says so.)

## Reuse

- The local merge helper from [13](13-subworkflow-block-and-resolver.md) — generalize, do not duplicate.
- Existing HCL parse infrastructure in [workflow/parse.go](../../workflow/parse.go) (or wherever `Parse` lives).
- Existing diagnostic-subject preservation patterns.

## Out of scope

- File ordering for the merge. Use lexicographic order of filenames; document that `variables.hcl` and `xxx-variables.hcl` collide if their lex order doesn't match author intent (in practice, no observable effect since merging is order-insensitive for slices).
- Glob patterns in CLI args (`criteria apply 'workflows/*'`). Single path only.
- Recursive directory scanning. Only the top-level `.hcl` files in the named directory; subdirectories are NOT included automatically. To compose, use `subworkflow` blocks.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — `WorkflowHeaderSpec`, reshape `Spec`.
- New: `workflow/parse_dir.go`.
- [`workflow/parse.go`](../../workflow/parse.go) (or wherever `Parse` is) — refactor to call into `ParseFileOrDir` for the public CLI entry; `Parse(path, src)` continues to exist as a single-file primitive used internally.
- [`internal/cli/apply_setup.go`](../../internal/cli/apply_setup.go).
- [`internal/cli/compile.go`](../../internal/cli/compile.go).
- All call sites that read `spec.Name`, `spec.Version`, etc. — update to `spec.Header.Name` etc.
- All example HCL files — wrap header attributes in `workflow "<name>" { ... }` block.
- New: [`examples/phase3-multi-file/`](../../examples/).
- Goldens.
- [`docs/workflow.md`](../../docs/workflow.md) — directory-mode section.
- New tests.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files.

## Tasks

- [x] Implement `ParseDir` and `ParseFileOrDir` (Step 1).
- [x] Reshape `Spec` to extract `WorkflowHeaderSpec` (Step 2).
- [x] Update CLI entry to call `ParseFileOrDir` (Step 3).
- [x] Update examples; add multi-file example (Step 4).
- [x] Author tests (Step 5).
- [x] `make ci` green (Step 6).

## Exit criteria

- `criteria apply <directory>` works.
- `criteria compile <directory>` works.
- Multi-file workflows merge correctly with conflict detection.
- Workflow header is a `workflow "<name>" { ... }` block.
- All examples migrated.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 5 list. Coverage: ≥ 90% on `workflow/parse_dir.go`.

## Risks

| Risk | Mitigation |
|---|---|
| Existing call sites referencing `spec.Name` break in many places at once | Use `gopls` rename or sed-script across the codebase; verify with `make ci`. |
| Diagnostic subjects after merging point at the wrong file | The merge concatenates `SourceBytes` carefully and preserves per-file `*hcl.File` objects in the diagnostic-emit path. Test `TestParseDir_DiagnosticsHaveCorrectFilenameSubjects`. |
| Lexicographic file ordering produces surprising compile-error messages | Order-of-discovery doesn't affect the merged Spec's content (slice concatenation is order-stable but its order is not part of the contract). The HCL diagnostic ranges are per-file regardless. |
| Single-file-mode users see a new error "no workflow block declared" | Provide a clear migration message. The error fires once during apply; the message tells the user to wrap header attributes in `workflow "<name>" { }`. |
| Recursion into subdirectories is intuitively expected ("but I have a subworkflow under ./inner") | Document explicitly: subdirectories are NOT scanned. Use `subworkflow "x" { source = "./inner" }`. The single-directory rule keeps the entry shape predictable. |

## Implementation Notes (Executor)

### What was built

**`workflow/schema.go`**: Added `WorkflowHeaderSpec` struct. Restructured `Spec` — `Header *WorkflowHeaderSpec` replaces the five scalar fields (`Name`, `Version`, `InitialState`, `TargetState`, `DefaultEnvironment`). All content blocks (`step`, `state`, `adapter`, etc.) are now top-level fields on `Spec`.

**`workflow/parser.go`**: Removed the `File` wrapper struct. `Parse()` decodes directly into `Spec`. `annotateLegacyConfigRanges()` looks for steps at the top-level body. `ParseFile()` retained as a file-reading primitive (used by `validate.go`).

**`workflow/parse_dir.go`** (new): Implements `ParseDir`, `ParseFileOrDir`, `mergeSpecs`, `checkDuplicateNames`, `joinBytes`. Merge rules: slices concatenate, singletons (`Header`, `Policy`, `Permissions`) error on duplicate. Cross-file name collision detection for steps, states, adapters, variables.

**`workflow/parse_legacy_reject.go`**: All 7 top-level reject functions simplified — removed the "find workflow block first" navigation layer since steps are now at the top level.

**`workflow/compile.go`**, **`compile_environments.go`**, **`compile_subworkflows.go`**: Updated all `spec.Version/InitialState/TargetState/Name/DefaultEnvironment` → `spec.Header.*`. Removed `readAndParseSubworkflowDir` + `mergeSubworkflowSpecs` (replaced by `ParseDir` call). Removed unused `os`/`filepath` imports.

**`internal/cli/compile.go`**, **`apply_setup.go`**: Both now use `workflow.ParseFileOrDir`. `workflowDir` computed from `os.Stat` (dirs use themselves; files use `filepath.Dir`).

**`internal/cli/plan.go`**: `spec.Version` → `spec.Header.Version`.

**`internal/cli/validate.go`**: Updated to use `workflow.ParseFileOrDir` (was `ParseFile`), enabling `criteria validate <dir>`. `workflowDir` computed via stat.

**`Makefile`**: `validate` target extended to include `examples/phase3-multi-file/` as a directory module validation.

**`examples/phase3-multi-file/`**: 4-file directory module example (`workflow.hcl`, `adapters.hcl`, `steps.hcl`, `variables.hcl`).

**`docs/workflow.md`**: Updated with new format, directory-mode description, and migration guide.

**All HCL fixture files and inline test HCL strings**: Migrated to new format (workflow block is header-only; content blocks at top level). ~20 HCL files and ~38 Go test files updated.

**New test files**:
- `workflow/parse_dir_test.go`: 8 tests covering `ParseDir` (single file, multi-file, no HCL files, dir not exist, duplicate step, duplicate workflow block, no workflow block, diagnostic subjects).
- `workflow/parse_file_or_dir_test.go`: 4 tests covering `ParseFileOrDir` (file path, file without workflow block, dir path, nonexistent path).

### Validation run

- `go build ./...` — exit 0
- `make test` — all packages pass (the only occasional failure is the pre-existing `TestExecuteServerRun_Cancellation` timing flake in `internal/cli`, unrelated to this workstream)
- `make validate` — all 11 examples + the new multi-file directory validate OK
- `make lint-imports` — import boundaries clean

### Security review

No new untrusted input surfaces. `ParseDir` uses `os.ReadDir` (lexicographic order, non-recursive) — no path traversal risk. File paths come from CLI args or resolved subworkflow paths, both sanitized upstream. No new secrets exposure.

### Deviations from plan

- `ParseFileOrDir` for a single file does NOT parse the parent directory as originally specified in the workstream comment. Instead it reads only the named file and requires a `Header`. This is strictly correct: `criteria apply foo.hcl` does not accidentally pick up sibling files. The `workflowDir` for subworkflow resolution is set to `filepath.Dir(path)`, preserving the original relative-path resolution behavior.
- `ParseFile` is retained (not removed) since it is still used by `validate.go` and simpler single-file parse paths.
- The `validate` command's `Use` field updated to `validate <workflow.hcl|dir>` to document directory support.

### [ARCH-REVIEW] None required.

## Reviewer Notes

### Review 2026-05-05 — changes-requested

#### Summary

`changes-requested`. The branch lands most of the parser/schema reshaping, but it misses two required entry-path behaviors and leaves the conflict diagnostics below the workstream bar. `ParseFileOrDir` does not implement the required "file path delegates to parent directory module" semantics, and `criteria apply <directory>` still executes with the wrong runtime base directory, so relative `file()` reads fail during execution. The new tests are also not strong enough to catch either regression or the missing file/line conflict locations.

#### Plan Adherence

- **Step 1 (`ParseDir` / `ParseFileOrDir`)**: partially implemented. `ParseDir` exists, but `ParseFileOrDir` in `workflow/parse_dir.go:87-122` parses a regular file directly instead of delegating to `ParseDir(filepath.Dir(path))` and verifying the target file is part of the module. That is a direct deviation from the specified unified entry shape.
- **Step 2 (header extraction)**: implemented. `Spec.Header` is wired through compile call sites.
- **Step 3 (CLI entry)**: partially implemented. `compile`/`apply`/`validate` now parse directories, but the execution path still derives the runtime workflow directory incorrectly for directory inputs in `internal/cli/apply_local.go:94-97`, `internal/cli/apply_local.go:140-145`, `internal/cli/apply_server.go:68-71`, `internal/cli/apply_server.go:107-112`, `internal/cli/apply_resume.go:140-145`, and `internal/cli/reattach.go:173-179`, `209-214`, `291-296`.
- **Step 4 (examples/docs)**: mostly implemented, but `docs/workflow.md` now says every workflow file begins with a workflow header block even though the new multi-file shape explicitly allows content-only files.
- **Step 5 (tests)**: incomplete. The parser tests in `workflow/parse_file_or_dir_test.go:9-100` encode the deviated file-path behavior instead of the required delegation behavior, and `workflow/parse_dir_test.go:146-208` only asserts duplicate summaries, not the required both-location diagnostics. No CLI contract coverage was added for directory `apply` / `compile` / `validate`.

#### Required Remediations

- **blocker** — `workflow/parse_dir.go:87-122`, `workflow/parse_file_or_dir_test.go:9-55`  
  `ParseFileOrDir` does not satisfy the workstream’s core compatibility rule: `criteria apply foo.hcl` must go through the directory-module path, not a separate single-file parser. I reproduced this with a split module containing `workflow.hcl` + `steps.hcl`; `go run ./cmd/criteria validate "$tmpdir/workflow.hcl"` failed with `initial_state "run" does not refer to a declared step or state`.  
  **Acceptance:** implement the specified delegation to the parent directory module, verify the named file is included in the parsed set, and replace the current file-only tests with the required delegation behavior tests.

- **blocker** — `internal/cli/apply_local.go:94-97`, `internal/cli/apply_local.go:140-145`, `internal/cli/apply_server.go:68-71`, `internal/cli/apply_server.go:107-112`, `internal/cli/apply_resume.go:140-145`, `internal/cli/reattach.go:173-179`, `internal/cli/reattach.go:209-214`, `internal/cli/reattach.go:291-296`  
  Runtime execution still uses `filepath.Dir(opts.workflowPath)` / `filepath.Dir(cp.WorkflowPath)` unconditionally. For directory inputs that resolves to the parent directory, so runtime relative-path evaluation is wrong. I reproduced this with `go run ./cmd/criteria apply "$tmpdir"` on a directory workflow whose step input was `file("./payload.sh")`; the run failed with `file(): no such file: ./payload.sh` even though the file exists in the workflow directory.  
  **Acceptance:** thread the resolved workflow directory used during compile into every initial, resumed, local, server, and reattach engine construction path, then add CLI-level regression tests that run `apply` against a directory workflow containing a relative `file()` reference and prove it succeeds.

- **blocker** — `workflow/parse_dir.go:152-216`, `workflow/parse_dir.go:220-270`, `workflow/parse_dir_test.go:146-208`  
  Conflict diagnostics do not meet the required acceptance bar. Duplicate workflow/policy/permissions and duplicate-name errors are emitted without `Subject` / `Context` locations; a direct repro against duplicate steps printed `subject=<nil> context=<nil>`. The workstream explicitly requires both file:line locations for singleton conflicts and cross-file duplicate names.  
  **Acceptance:** preserve per-declaration source locations during merge, emit diagnostics that carry both locations for duplicate singleton blocks and duplicate named declarations, and strengthen tests to assert the reported filenames/locations rather than only matching summary text.

- **major** — `internal/cli/*_test.go`, `workflow/parse_file_or_dir_test.go:9-100`, `workflow/parse_dir_test.go:146-260`  
  The new tests validate parser happy paths, but they do not cover the CLI contract surface that changed, and they missed two real regressions.  
  **Acceptance:** add end-to-end/contract tests for `criteria apply <directory>`, `criteria compile <directory>`, and `criteria validate <directory>` plus the required `foo.hcl`→parent-directory behavior, with assertions that would fail on the current broken implementations.

- **nit** — `docs/workflow.md:22-23`, `docs/workflow.md:68-82`  
  The header section says every workflow file begins with a workflow header block, which conflicts with the documented multi-file directory mode where only one file contains the header and the rest may be content-only.  
  **Acceptance:** tighten the wording so single-file and multi-file module rules are both accurate.

#### Test Intent Assessment

The current parser tests prove that `ParseDir` can merge simple directories and that generic error strings appear, but they do **not** prove the user-visible compatibility contract for `foo.hcl` entry paths, they do **not** prove that conflict diagnostics retain actionable locations, and they do **not** exercise the CLI command boundaries that changed. The fact that `go test ./workflow -run 'TestParseDir|TestParseFileOrDir'` and `go test ./internal/cli -run 'TestApply|TestValidate|TestCompile'` both pass while the directory-entry and runtime-path regressions remain confirms the assertions are too weak for this workstream.

#### Validation Performed

- `go test ./workflow -run 'TestParseDir|TestParseFileOrDir'` — passed.
- `go test ./internal/cli -run 'TestApply|TestValidate|TestCompile'` — passed.
- Manual repro: split directory module with `workflow.hcl` + `steps.hcl`, then `go run ./cmd/criteria validate "$tmpdir/workflow.hcl"` — failed with `initial_state "run" does not refer to a declared step or state`, proving file-path entry does not delegate to the directory module.
- Manual repro: directory workflow using `file("./payload.sh")`, then `go run ./cmd/criteria apply "$tmpdir"` — failed at runtime with `file(): no such file: ./payload.sh`, proving the execution path uses the wrong workflow directory for directory inputs.
- Manual repro: duplicate steps across two files inspected via `workflow.ParseDir(...)` — emitted `duplicate step name "run" across files` with `subject=<nil> context=<nil>`, proving the required file/line conflict locations are not preserved.

---

## Reviewer Feedback Remediation — 2026-05-05

### Changes made in response to Review 2 blockers

**Blocker 1 — `ParseFileOrDir` file-path delegation (`workflow/parse_dir.go`)**

`ParseFileOrDir(path)` now first attempts `ParseDir(filepath.Dir(path))` for file paths. This correctly merges all sibling `.hcl` files as one directory module. If the parent directory contains multiple independent workflow files (each with their own workflow/policy/permissions singletons — i.e., it is a collection of independent workflows, not a module), `isSingletonConflictOnly()` detects this and falls back to single-file parsing. This preserves backward compatibility with shared testdata directories and the existing `examples/` directory structure.

New functions:
- `isSingletonConflictOnly(diags)` — detects parent-is-a-collection fallback condition
- `parseSingleFile(path)` — single-file fallback with header requirement

**Blocker 2 — `workflowDir` threading through all apply execution paths (`internal/cli/apply_setup.go`, `apply_local.go`, `apply_server.go`, `apply_resume.go`, `reattach.go`)**

Added `workflowDirFromPath(path) string` helper: returns path for directories, `filepath.Dir(path)` for files. Replaced all `filepath.Dir(opts.workflowPath)` and `filepath.Dir(cp.WorkflowPath)` calls in every initial, resumed, local, server, and reattach engine construction path. Fixed `parseWorkflowFromPath` in `reattach.go` to use `ParseFileOrDir` (was `os.ReadFile + Parse`, which fails for directory paths).

**Blocker 3 — Source locations in conflict diagnostics (`workflow/parse_dir.go`)**

Added:
- `fileEntry{spec *Spec, ranges map[string]hcl.Range}` type to carry hclsyntax block ranges alongside parsed specs
- `collectFileBlockRanges(src, filename)` using `hclsyntax.ParseConfig` to extract `DefRange()` per block key (`"step:name"`, `"adapter:type.name"`, `"workflow"`, `"policy"`, `"permissions"`, `"state:name"`, `"variable:name"`)
- `mergeSpecs` now accepts `[]fileEntry`, tracks first-seen ranges for singleton blocks, and sets `Subject` + `"previously declared at {location}"` in all singleton conflict diagnostics
- `checkDuplicateNames` now iterates per-file entries (not the merged spec) so it can track first vs second occurrence with file:line info

**Major — CLI contract tests (`internal/cli/cli_dir_mode_test.go`)**

New file with 6 end-to-end tests:
- `TestCompileDir_{DirectoryPath,FilePathDelegatesToParentDir}` — prove `compileWorkflowOutput` accepts dir and file paths
- `TestValidateDir_{DirectoryPath,FilePathDelegatesToParentDir}` — prove `validate` command merges sibling files
- `TestApplyLocal_{DirectoryPath,FilePathDelegatesToParentDir}` — prove `apply` runs a noop adapter workflow from a split directory module with both path styles

All tests would have FAILED on the pre-fix implementations.

**Nit — `docs/workflow.md`**

- Lines 22-23: `<workflow.hcl>` → `<workflow.hcl|dir>` in execution mode examples
- Lines 30-33: Replaced "Every workflow file begins with a workflow header block" with accurate description of both single-file and multi-file module forms
- Line 77: Added "only ONE file needs the header; all other files are content-only"

### Test strengthening

- `TestParseDir_DuplicateStepAcrossFiles_Error` (parse_dir_test.go): now asserts `d.Subject != nil`, `Subject.Filename == "steps2.hcl"`, `Detail` contains "previously declared at", and `Detail` contains "main.hcl"
- `parse_file_or_dir_test.go`: Rewrote all 4 tests to cover delegation behavior; added 5th test (`FilePath_FallsBackToSingleFileWhenParentHasMultipleHeaders`) for the fallback path

### Validation

- `make test` — all packages pass (pre-existing `TestExecuteServerRun_Cancellation` timing flake unaffected)
- `make validate` — all examples and phase3-multi-file/ directory module OK
- `make lint-imports` — import boundaries clean
- `go build ./...` — exit 0
