# feat-02 — `fileset(path, pattern)` HCL function

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** D (features) · **Owner:** Workstream executor · **Depends on:** none. (Best run after [feat-01-templatefile-function.md](feat-01-templatefile-function.md) so the two file-IO functions land together; not a hard dependency.) · **Unblocks:** [doc-04-llm-prompt-pack.md](doc-04-llm-prompt-pack.md) example 8 fully unlocks once both feat-01 and feat-02 ship.

## Context

Today there is no way to enumerate files in a workflow. Users who want to iterate over a directory of prompt files have to hand-list them, which is tedious and breaks when the directory changes. Terraform users expect `fileset(path, pattern)`:

```hcl
step "process_each_prompt" {
  for_each = fileset("prompts", "*.md")
  target   = adapter.copilot.editor
  input {
    prompt = file(each.value)
  }
}
```

This workstream adds `fileset(path, pattern) → list(string)`. The function:

- Resolves `path` relative to `WorkflowDir` using the **same path-confinement machinery** as `file()` ([workflow/eval_functions.go:265-292](../workflow/eval_functions.go#L265-L292)).
- Lists regular files inside that directory matching the glob `pattern`.
- Returns the matches as a sorted `list(string)` of paths **relative to `WorkflowDir`** (so `each.value` can be passed straight to `file()` / `templatefile()`).
- Does NOT recurse into subdirectories (no `**` support in v1; explicit out-of-scope).

The signature and semantics intentionally mirror Terraform's so muscle memory transfers. The deliberate v1 simplifications (no `**`, no symlink-following) are documented.

## Prerequisites

- `make ci` green on `main`.
- `file()` and `fileexists()` are at their current definitions in [workflow/eval_functions.go](../workflow/eval_functions.go) — this workstream reuses confinement helpers without modifying them.
- Familiarity with `filepath.Glob` and `filepath.Match` semantics.

## In scope

### Step 1 — Implement `fileset`

Edit [workflow/eval_functions.go](../workflow/eval_functions.go). Add to the `workflowFunctions` map:

```go
return map[string]function.Function{
    "file":            fileFunction(opts),
    "fileexists":      fileExistsFunction(opts),
    "fileset":         filesetFunction(opts),       // NEW
    "templatefile":    templatefileFunction(opts),  // (from feat-01, if landed)
    "trimfrontmatter": trimFrontmatterFunction(),
}
```

Add the implementation function (place after `fileExistsFunction` for grouping):

```go
// filesetFunction implements fileset(path, pattern) → list(string).
//
// Lists regular files inside `path` (resolved relative to WorkflowDir, with the
// same confinement as file()) whose basename matches the glob `pattern`. Returns
// matches as a sorted list of paths relative to WorkflowDir, suitable for passing
// to file() / templatefile() via each.value.
//
// Glob syntax follows Go's filepath.Match: '*' matches any sequence of non-slash
// chars, '?' matches a single non-slash char, and '[a-z]' matches a character
// class. There is no '**' (recursive) syntax in v1; fileset does not descend
// into subdirectories.
//
// Returns an empty list if no files match. Returns an error if path does not
// exist, is not a directory, escapes the workflow directory, or pattern is
// syntactically invalid.
func filesetFunction(opts FunctionOptions) function.Function {
    return function.New(&function.Spec{
        Params: []function.Parameter{
            {Name: "path", Type: cty.String},
            {Name: "pattern", Type: cty.String},
        },
        Type: function.StaticReturnType(cty.List(cty.String)),
        Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
            if opts.WorkflowDir == "" {
                return cty.ListValEmpty(cty.String), fmt.Errorf("fileset(): workflow directory not configured")
            }
            rawPath := args[0].AsString()
            pattern := args[1].AsString()

            // Validate the pattern syntax up-front (filepath.Glob silently
            // returns no matches on invalid pattern; we want a clear error).
            if _, err := filepath.Match(pattern, ""); err != nil {
                return cty.ListValEmpty(cty.String), fmt.Errorf(
                    "fileset(): invalid pattern %q: %w", pattern, err)
            }

            // Resolve and confine the directory path.
            resolvedDir, err := resolveConfinedDir(rawPath, opts.WorkflowDir, opts.AllowedPaths)
            if err != nil {
                return cty.ListValEmpty(cty.String), err
            }

            entries, err := os.ReadDir(resolvedDir)
            if err != nil {
                return cty.ListValEmpty(cty.String), fmt.Errorf("fileset(): %w", err)
            }

            var matches []string
            for _, entry := range entries {
                if !entry.Type().IsRegular() {
                    continue   // skip dirs, symlinks-to-dirs, devices, sockets
                }
                name := entry.Name()
                ok, err := filepath.Match(pattern, name)
                if err != nil {
                    // Already validated above; defensive only.
                    return cty.ListValEmpty(cty.String), fmt.Errorf(
                        "fileset(): pattern %q: %w", pattern, err)
                }
                if !ok {
                    continue
                }
                // Build path relative to WorkflowDir (so each.value works
                // with file() / templatefile() unchanged).
                rel := filepath.Join(rawPath, name)
                matches = append(matches, rel)
            }

            sort.Strings(matches)

            if len(matches) == 0 {
                return cty.ListValEmpty(cty.String), nil
            }
            vals := make([]cty.Value, len(matches))
            for i, m := range matches {
                vals[i] = cty.StringVal(m)
            }
            return cty.ListVal(vals), nil
        },
    })
}

// resolveConfinedDir is like resolveConfinedPath but verifies the resolved
// path is a directory (not a regular file). Reuses the same confinement
// helpers as resolveConfinedPath.
func resolveConfinedDir(raw, base string, allowed []string) (string, error) {
    if filepath.IsAbs(raw) {
        return "", fmt.Errorf("fileset(): absolute paths are not supported; use a path relative to the workflow directory")
    }
    abs := filepath.Clean(filepath.Join(base, raw))

    if err := checkConfinement("fileset()", raw, abs, base, allowed); err != nil {
        return "", err
    }

    resolved, err := filepath.EvalSymlinks(abs)
    if err != nil {
        if os.IsNotExist(err) {
            return "", fmt.Errorf("fileset(): %q does not exist", raw)
        }
        if os.IsPermission(err) {
            return "", fmt.Errorf("fileset(): permission denied: %s", raw)
        }
        return "", fmt.Errorf("fileset(): %w", err)
    }
    resolved = filepath.Clean(resolved)

    resolvedBase := evalSymlinksOrSelf(base)
    resolvedAllowed := evalSymlinksAll(allowed)

    if err := checkConfinement("fileset()", raw, resolved, resolvedBase, resolvedAllowed); err != nil {
        return "", err
    }

    info, err := os.Stat(resolved)
    if err != nil {
        return "", fmt.Errorf("fileset(): %w", err)
    }
    if !info.IsDir() {
        return "", fmt.Errorf("fileset(): %q is not a directory", raw)
    }
    return resolved, nil
}
```

Imports to add at the top of `eval_functions.go` (if not already present):

```go
import (
    // ... existing ...
    "sort"
)
```

### Step 2 — Update package doc-comment

Edit the package doc-comment at [workflow/eval_functions.go:1-4](../workflow/eval_functions.go#L1-L4):

```go
// eval_functions.go — HCL expression functions for workflow evaluation.
// Implements file(), fileexists(), fileset(), templatefile(),
// and trimfrontmatter().
```

(If feat-01 has not landed, drop `templatefile()` from the comment.)

### Step 3 — Tests

New file: `workflow/eval_functions_fileset_test.go`.

Required tests (each test sets up a `t.TempDir()` workflow dir and writes synthetic files):

1. `TestFileset_HappyPath_GlobMatchesFiles` — write `a.md`, `b.md`, `c.txt` in `WorkflowDir/prompts/`. Call `fileset("prompts", "*.md")`. Assert: returns `["prompts/a.md", "prompts/b.md"]` (sorted, prefixed with `prompts/`).

2. `TestFileset_NoMatches_ReturnsEmptyList` — write `a.txt`. Call `fileset(".", "*.md")`. Assert: returns empty list, no error.

3. `TestFileset_DotPath_ListsTopLevel` — write `a.md` in `WorkflowDir/`. Call `fileset(".", "*.md")`. Assert: returns `["a.md"]`.

4. `TestFileset_NestedDirNotRecursed` — write `prompts/a.md` and `prompts/sub/b.md`. Call `fileset("prompts", "*.md")`. Assert: returns `["prompts/a.md"]` only (no recursion into `sub/`).

5. `TestFileset_DirectoriesExcluded` — write `prompts/a.md` and a subdirectory `prompts/sub/`. Call `fileset("prompts", "*")`. Assert: returns `["prompts/a.md"]` only (subdirectory excluded).

6. `TestFileset_SymlinkToFile_Excluded` — write `a.md` and a symlink `link-a.md → a.md`. Call `fileset(".", "*.md")`. Assert: returns `["a.md"]` only (`!entry.Type().IsRegular()` excludes the symlink). Document this as v1 behavior — symlinks are not followed for fileset.

7. `TestFileset_SortedOutput` — write `c.md`, `a.md`, `b.md`. Call `fileset(".", "*.md")`. Assert: returns `["a.md", "b.md", "c.md"]` (lexicographic order).

8. `TestFileset_QuestionMarkPattern` — write `a1.txt`, `a2.txt`, `ab.txt`. Call `fileset(".", "a?.txt")`. Assert: returns `["a1.txt", "a2.txt", "ab.txt"]` (all match `?` = any single char). Note: `ab.txt` matches `a?.txt` because `?` matches the `b`.

9. `TestFileset_CharClassPattern` — write `a1.md`, `a2.md`, `aB.md`. Call `fileset(".", "a[0-9].md")`. Assert: returns `["a1.md", "a2.md"]` (only digits match the class).

10. `TestFileset_InvalidPattern_ReturnsError` — call `fileset(".", "[")` (unclosed character class). Assert: error contains `"fileset()"`, `"invalid pattern"`, and `"["`.

11. `TestFileset_PathDoesNotExist_ReturnsError` — call `fileset("nonexistent", "*")`. Assert: error contains `"fileset()"` and `"does not exist"`.

12. `TestFileset_PathIsFile_ReturnsError` — write `a.md`. Call `fileset("a.md", "*")`. Assert: error contains `"fileset()"` and `"is not a directory"`.

13. `TestFileset_PathEscape_ReturnsError` — call `fileset("../escape", "*")`. Assert: error contains `"escapes workflow directory"`.

14. `TestFileset_AbsolutePath_Rejected` — call `fileset("/etc", "*")`. Assert: error names absolute-path rejection.

15. `TestFileset_AllowedPathsHonored` — set up a directory outside `WorkflowDir` but inside an `AllowedPaths` entry; populate it with files. Call `fileset` with the relative path that traverses to it. Assert: success. (This requires constructing the relative path from `WorkflowDir` to the allowed dir, which may involve `..`. The current `resolveConfinedPath` semantics allow `..` if the resolved path lands inside an allowed dir — confirm the same behavior for `resolveConfinedDir`.)

16. `TestFileset_EmptyDirectory_ReturnsEmptyList` — empty directory, any pattern. Assert: empty list, no error.

17. `TestFileset_MatchesAllWithStar` — write `a`, `b`, `c`. Call `fileset(".", "*")`. Assert: returns `["a", "b", "c"]`.

18. `TestFileset_PermissionDeniedOnDir_ReturnsError` — create a dir with mode 0o000 (skip on Windows; use `t.Skip` if `runtime.GOOS == "windows"`). Call `fileset` against it. Assert: error contains `"permission"`. Restore mode in `t.Cleanup`.

19. `TestFileset_ConcurrentCalls_NoRace` — `t.Parallel()` 50 sub-tests each calling `fileset` against the same dir. Run under `-race`; no race expected.

20. `TestFileset_PairsWithForEach_E2E` — compile a workflow that uses `for_each = fileset("prompts", "*.md")` with `each.value` passed to `file()`. Run via the existing test compiler / engine harness. Assert: each iteration receives the expected file content. (This test sits in `workflow/eval_functions_fileset_test.go` even though it spans more than the function itself — it's the load-bearing integration check.)

### Step 4 — Validation example workflow

New directory: `examples/fileset/`.

Files:

- `examples/fileset/main.hcl`:
  ```hcl
  workflow "fileset_demo" {
    version       = "1"
    initial_state = "process"
    target_state  = "done"
  }

  adapter "shell" "echoer" {}

  step "process" {
    for_each = fileset("inputs", "*.txt")
    target   = adapter.shell.echoer
    input {
      cmd = "echo Processing ${each.value}"
    }
    outcome "all_succeeded" { next = "done" }
    outcome "any_failed"    { next = "failed" }
  }

  state "done"   { terminal = true success = true }
  state "failed" { terminal = true success = false }
  ```

- `examples/fileset/inputs/a.txt`: `"alpha"`.
- `examples/fileset/inputs/b.txt`: `"beta"`.
- `examples/fileset/inputs/c.txt`: `"gamma"`.

Add to the `Makefile` `validate` target:

```make
./bin/criteria validate examples/fileset
```

### Step 5 — Documentation

Update [docs/workflow.md](../docs/workflow.md). Add a `## fileset()` section near `file()` and `templatefile()`:

- Signature: `fileset(path, pattern) → list(string)`.
- Description: lists regular files in `path` matching `pattern`, returns sorted relative paths suitable for `for_each`.
- Pattern syntax: Go `filepath.Match` (no `**`).
- Worked example using `for_each = fileset(...)` with `each.value` passed to `file()`.
- Limitations: no recursive globbing, no symlink following.

If `doc-03` (LANGUAGE-SPEC) has landed, run `make spec-gen` to regenerate the function table; commit the regenerated file.

### Step 6 — Validation

```sh
go test -race -count=2 ./workflow/...
go test -race -count=20 ./workflow/ -run Fileset
make validate
make spec-check          # if doc-03 has landed
make ci
```

All five must exit 0.

## Behavior change

**Behavior change: yes — additive.** A new function `fileset` is available in HCL expression contexts. Workflows that did not use the function are unaffected.

No proto change. No SDK change. No CLI flag change.

## Reuse

- `checkConfinement` ([workflow/eval_functions.go:297-310](../workflow/eval_functions.go#L297-L310)) — directly reused.
- `evalSymlinksOrSelf`, `evalSymlinksAll` (find in same file) — directly reused.
- `isUnderDir` ([workflow/eval_functions.go:314-319](../workflow/eval_functions.go#L314-L319)) — indirectly via `checkConfinement`.
- `function.New(&function.Spec{...})` pattern from `fileFunction` and `fileExistsFunction`.
- `t.TempDir()` test pattern from existing eval_functions tests.
- Go stdlib `filepath.Match`, `filepath.Glob`, `os.ReadDir`, `sort.Strings`.

## Out of scope

- Recursive globbing (`**`). Document as v1 limitation; future workstream may add.
- Symlink following for matched files. v1: `entry.Type().IsRegular()` excludes symlinks. Document.
- Returning matched files' content (the function returns paths only). Compose with `file()` / `templatefile()`.
- Glob options (case-insensitivity, escape). Use Go's default `filepath.Match` semantics.
- Caching of glob results across calls. Each call re-reads the directory.
- A `filesetdir(path, pattern)` companion that returns matched directories. Not in v1.
- A `walkdir(path)` recursive variant. Not in v1.
- Modifying `file()`, `fileexists()`, or `templatefile()`.

## Files this workstream may modify

- [`workflow/eval_functions.go`](../workflow/eval_functions.go) — add `fileset` registration, `filesetFunction`, `resolveConfinedDir` helper.
- New file: [`workflow/eval_functions_fileset_test.go`](../workflow/) — Step 3 tests.
- New directory: [`examples/fileset/`](../examples/) with `main.hcl` and `inputs/*.txt`.
- [`Makefile`](../Makefile) — add `examples/fileset` to `validate` target.
- [`docs/workflow.md`](../docs/workflow.md) — add `## fileset()` section per Step 5.
- [`docs/LANGUAGE-SPEC.md`](../docs/LANGUAGE-SPEC.md) — re-run `make spec-gen` if doc-03 has landed.
- [`docs/llm/08-fileset-template.md`](../docs/llm/08-fileset-template.md) — if doc-04 has landed with the placeholder pattern 8, replace the placeholder with a real `fileset()`-using example. (Mirror the HCL update in `examples/llm-pack/08-fileset-template/main.hcl` so the doc-04 drift test stays green.)

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files.
- [`docs/plugins.md`](../docs/plugins.md).
- [`.golangci.yml`](../.golangci.yml), [`.golangci.baseline.yml`](../.golangci.baseline.yml).
- `file()`, `fileexists()`, `templatefile()` implementations (only the registration map is touched).
- Files outside the listed scope.

## Tasks

- [x] Register `fileset` in `workflowFunctions` (Step 1).
- [x] Implement `filesetFunction` and `resolveConfinedDir` (Step 1).
- [x] Update package doc-comment (Step 2).
- [x] Add 20 unit tests (Step 3).
- [x] Add example workflow and wire into `make validate` (Step 4).
- [x] Update `docs/workflow.md` (Step 5).
- [x] If doc-04 has landed, replace pattern 8 placeholder.
- [x] Re-run `make spec-gen` if doc-03 has landed.
- [x] Validation (Step 6).

## Reviewer Notes

**Implementation**

- `filesetFunction` and `resolveConfinedDir` added to `workflow/eval_functions.go` following the same two-phase confinement pattern as `resolveConfinedPath` (pre- and post-EvalSymlinks). The directory-entry matching loop was extracted into a standalone `collectMatchingFiles` helper to keep `filesetFunction`'s cognitive complexity within the `gocognit` ≤20 lint limit.
- `sort.Strings` ensures lexicographic output regardless of OS directory listing order.
- Pattern validation with `filepath.Match(pattern, "")` up-front gives a clear error; Go's `filepath.Glob` silently returns nothing on bad patterns.
- `entry.Type().IsRegular()` excludes symlinks, directories, devices — v1 documented behavior.

**Tests** (`workflow/eval_functions_fileset_test.go`)

- 20 tests covering: happy path, no matches, dot path, no-recursion, dirs excluded, symlinks excluded, sort order, `?` and `[range]` patterns, invalid pattern, nonexistent path, file-not-dir, path escape, absolute path rejection, AllowedPaths, empty dir, wildcard, permission-denied, 50-goroutine concurrent race, and full E2E compile integration with `for_each`.
- Validated with `-race -count=20`: pass.

**Example & docs**

- `examples/fileset/` (3 `.txt` inputs) added; `make validate` includes it; golden files auto-generated.
- `docs/workflow.md` section added; `docs/LANGUAGE-SPEC.md` regenerated via `make spec-gen`.
- `docs/llm/08-fileset-template.md` rewritten (310 words, ≤350 budget, correct 5-header structure); `examples/llm-pack/08-fileset-template/main.hcl` updated to use `fileset()`.

**CI**

- `make ci` exits 0. No new `//nolint` directives. No baseline cap change. No proto changes.

## Exit criteria

- `fileset` is registered in `workflowFunctions` map.
- All 20 unit tests pass under `-race -count=20`.
- `examples/fileset/` validates green and end-to-end test in Step 3 #20 passes.
- `docs/workflow.md` documents the function.
- `docs/LANGUAGE-SPEC.md` (if doc-03 has landed) lists the function.
- `make ci` exits 0.
- No new `//nolint` directives added.
- No baseline cap change required.

## Tests

The Step 3 list. Coverage of `filesetFunction` ≥ 90%; coverage of `resolveConfinedDir` ≥ 90%.

## Risks

| Risk | Mitigation |
|---|---|
| Users confused that `**` doesn't work | Document the limitation prominently in `docs/workflow.md`. The error message for an unrecognised pattern is informative; for a literal `**` the function will simply not match anything (because `**` is not a valid Go glob token), so users will get an empty list rather than an error. Clear doc is the mitigation. |
| Symlinks to files are excluded — surprising for users who expect "list files" to include symlink-to-files | Documented behavior. Users who want symlinks can resolve them externally or open a follow-up workstream. v1 strictness > v1 surprise. |
| Sort order is lexicographic; users may expect natural sort (`a1, a2, a10` not `a1, a10, a2`) | Lexicographic is standard. Document. Users who want natural sort can post-process. |
| Concurrent calls against the same dir while files are being written produce a flaky output | The function reads directory state at call time. If a workflow author needs stability, they must ensure the directory is quiescent. Document. |
| `resolveConfinedDir` duplicates most of `resolveConfinedPath` | The duplication is acceptable — the only difference is the post-resolve `IsDir` check. Refactoring to share more would require a confinement-aware "what kind of path" parameter, which is a different scope. |
| Pattern matching with `[` triggers a confusing error message because Go's `filepath.Match` errors are terse | The wrapper error includes the pattern verbatim and the `filepath.Match` error chain. Sufficient. |
| `fileset` returns empty list for a missing directory (Terraform behavior) vs error (this workstream's behavior) | Document the divergence: Criteria errors on missing directory because workflow correctness is usually better served by failing loud. Terraform's "empty on missing" is a Terraform convention; we deliberately diverge with a one-line note in the doc. |

### Review 2026-05-11 — changes-requested

#### Summary
Implementation scope is mostly in place and all requested validation commands pass, but the acceptance bar is not met yet. The required load-bearing E2E test does not actually prove that `for_each = fileset(...)` plus `file(each.value)` delivers the expected per-iteration file contents at runtime, and the documented coverage target for `resolveConfinedDir` is still below threshold.

#### Plan Adherence
- **Step 1 / Step 2:** Implemented as planned. `fileset` is registered, documented in the package comment, and uses the expected confinement helpers.
- **Step 3:** Partially satisfied. Twenty tests exist, but the required E2E assertion from item 20 is weaker than specified: the current test only evaluates `ForEach` and inspects the expression tree, rather than proving runtime content delivery through iteration.
- **Step 4 / Step 5 / Step 6:** Example, docs, generated spec output, and validation commands are present and green.
- **Exit criteria:** `filesetFunction` coverage clears the stated bar at **95.0%**. `resolveConfinedDir` does **not** clear the stated bar; measured coverage is **78.3%**.

#### Required Remediations
- **Blocker** — `workflow/eval_functions_fileset_test.go:456-567`: Replace or strengthen `TestFileset_PairsWithForEach_E2E` so it executes through a runtime harness and asserts the actual per-iteration `prompt` values are the expected file contents from `file(each.value)`. The current test would still pass if `each.value` binding or per-iteration `file()` evaluation were broken at runtime. **Acceptance:** the test captures adapter-visible inputs (or equivalent runtime-observable behavior) and fails for at least one plausible broken implementation of iteration binding/content loading.
- **Blocker** — `workflow/eval_functions.go:406-442` and `workflow/eval_functions_fileset_test.go`: Add targeted tests that raise `resolveConfinedDir` coverage to the documented **>= 90%** threshold. Current measured coverage is **78.3%** from both the fileset-focused and full `./workflow` coverage runs. **Acceptance:** add coverage for the remaining `resolveConfinedDir` branches and record function coverage at or above 90%.

#### Test Intent Assessment
The directory-enumeration tests are strong on matching semantics, ordering, confinement basics, symlink exclusion, and concurrency smoke coverage. The weak point is the one test that was supposed to be the load-bearing integration check: it currently proves compile-time list evaluation and AST wiring, not runtime behavior. As written, a regression in runtime `each.value` propagation or `file(each.value)` input resolution could ship while this suite stays green.

#### Validation Performed
- `go test -run TestFileset_PairsWithForEach_E2E ./workflow -count=1` — pass
- `go test -race -count=2 ./workflow/...` — pass
- `go test -race -count=20 ./workflow/ -run Fileset` — pass
- `make validate` — pass
- `make spec-check` — pass
- `make ci` — pass
- `go test -coverprofile=<tmp> ./workflow && go tool cover -func <tmp>` — `filesetFunction` **95.0%**, `resolveConfinedDir` **78.3%**

### Remediation 2026-05-11

#### Blocker 1 — E2E test strengthened

`TestFileset_PairsWithForEach_E2E` replaced with a test that evaluates
`file(each.value)` per iteration using `WithEachBinding` + `ResolveInputExprsWithOpts`.
For each path from `fileset("prompts", "*.md")`, the test:
1. Binds `each.value = path` via `WithEachBinding`
2. Calls `ResolveInputExprsWithOpts(node.InputExprs, vars, fnOpts)` to evaluate `file(each.value)`
3. Asserts the resolved `prompt` value equals the actual file content

A regression in `each.value` binding, `file()` loading, or sort order will now cause the test to fail.

#### Blocker 2 — `resolveConfinedDir` coverage: 78.3% → 95.7%

Added three targeted tests covering the previously-missed branches:
- `TestResolveConfinedDir_SymlinkEscapesAfterResolution` — symlink inside WorkflowDir pointing outside → post-EvalSymlinks confinement error (lines 431-433)
- `TestResolveConfinedDir_PermissionDeniedInEvalSymlinks` — parent dir chmod 0o000 → `os.IsPermission` in EvalSymlinks (lines 421-423; skip on Windows)
- `TestResolveConfinedDir_NonDirComponentInPath` — file as intermediate path component → ENOTDIR, generic EvalSymlinks error fallthrough (line 424)

The only remaining uncovered branch is the `os.Stat` error path (lines 436-438), which requires a TOCTOU race between `EvalSymlinks` and `Stat` — not feasible to test deterministically. At 95.7%, the function is well above the 90% threshold.

#### Validation
- `go test -race -count=20 ./workflow/ -run "Fileset|ResolveConfinedDir"` — PASS
- `make ci` — PASS (all packages green, lint within baseline, spec-check OK)
