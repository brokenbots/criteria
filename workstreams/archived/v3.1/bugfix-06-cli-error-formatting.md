# Bugfix Workstream BF-06 — CLI: suppress help menu on non-argument errors; format all HCL diagnostics with file/line context

**Owner:** Workstream executor · **Depends on:** none · **Coordinates with:** BF-01 through BF-05 (independent).

## Context

Two overlapping UX problems make compile and validation failures hard to act on:

### Problem 1 — Help menu appears on every runtime error

Cobra's default behavior is to print the full command usage text whenever `RunE` returns a
non-nil error. None of the criteria subcommands set `SilenceUsage`, so a compile failure in
`criteria compile`, `criteria plan`, or `criteria apply` produces:

```
Error: compile: <message>

Usage:
  criteria compile <workflow.hcl|dir> [flags]

Flags:
  --format string   ...
  --out string      ...
  ...
```

The usage block is only appropriate when the user provided wrong or missing arguments.
A compile error, a missing file, or a network failure is not a usage mistake, and the help
text is visual clutter that buries the actual error.

### Problem 2 — HCL diagnostics are flattened into a single unreadable string

Every call site that encounters `hcl.Diagnostics` collapses them via `diags.Error()` before
wrapping in `fmt.Errorf`:

```go
// internal/cli/compile.go:272
return nil, nil, fmt.Errorf("parse: %s", diags.Error())

// internal/cli/apply_setup.go:101
return nil, nil, nil, fmt.Errorf("compile: %s", diags.Error())
```

`hcl.Diagnostics.Error()` concatenates all diagnostic `Summary` fields as a semicolon-
separated one-liner. It discards:
- `hcl.Diagnostic.Detail` — the full explanation
- `hcl.Diagnostic.Subject *hcl.Range` — the file path and line/column of the offending token
- `hcl.Diagnostic.Severity` — error vs warning distinction

When multiple errors exist they pile into one line. The user's terminal shows something like:

```
Error: compile: workflow.initial_state is required; step "run" adapter ref must be declared; ...and 15 other diagnostics
```

There is no file path, no line number, no detail text, and some errors are hidden behind a
truncation message. Debugging requires guessing which file and line triggered each message.

### Affected call sites

| File | Pattern |
|---|---|
| [internal/cli/compile.go:272](../internal/cli/compile.go#L272) | `fmt.Errorf("parse: %s", diags.Error())` |
| [internal/cli/compile.go:291](../internal/cli/compile.go#L291) | `fmt.Errorf("compile: %s", diags.Error())` |
| [internal/cli/apply_setup.go:84](../internal/cli/apply_setup.go#L84) | `fmt.Errorf("parse: %s", diags.Error())` |
| [internal/cli/apply_setup.go:101](../internal/cli/apply_setup.go#L101) | `fmt.Errorf("compile: %s", diags.Error())` |
| [internal/cli/reattach.go:310](../internal/cli/reattach.go#L310) | `fmt.Errorf("parse workflow: %s", diags.Error())` |
| [internal/cli/reattach.go:324](../internal/cli/reattach.go#L324) | `fmt.Errorf("compile workflow: %s", diags.Error())` |
| [internal/cli/validate.go:31](../internal/cli/validate.go#L31) | `fmt.Fprintf(os.Stderr, ..., diags.Error())` |
| [internal/cli/validate.go:51](../internal/cli/validate.go#L51) | `fmt.Fprintf(os.Stderr, ..., diags.Error())` |
| [internal/cli/validate.go:56](../internal/cli/validate.go#L56) | `fmt.Fprintf(os.Stderr, ..., diags.Error())` |

## Prerequisites

- Familiarity with:
  - [cmd/criteria/main.go](../cmd/criteria/main.go) — root cobra command, `Execute()` error handler.
  - [internal/cli/compile.go:269](../internal/cli/compile.go#L269) — `parseCompileForCli`.
  - [internal/cli/apply_setup.go](../internal/cli/apply_setup.go) — `setupApply`.
  - [internal/cli/reattach.go:308](../internal/cli/reattach.go#L308) — `reloadWorkflow`.
  - [internal/cli/validate.go](../internal/cli/validate.go) — `validate` command `RunE`.
  - `github.com/hashicorp/hcl/v2` — `hcl.Diagnostics`, `hcl.Diagnostic`, `hcl.Range`, `hcl.Pos` (fields: `Filename string`, `Start.Line int`, `Start.Column int`), `hcl.DiagError`, `hcl.DiagWarning`.
  - `github.com/spf13/cobra` — `Command.SilenceUsage`, `Command.SilenceErrors`.
- `make build` green on `main`.

## In scope

### Step 1 — Suppress help menu on non-argument errors

Set `SilenceUsage: true` on the root command in [cmd/criteria/main.go](../cmd/criteria/main.go):

```go
root := &cobra.Command{
    Use:          "criteria",
    Short:        "Criteria agent — local workflow executor",
    SilenceUsage: true,
}
```

Setting it on the root propagates the flag to all subcommands via cobra's execution path.
Usage will still be printed for argument count violations (`cobra.ExactArgs`, `cobra.MinimumNArgs`)
because those errors are generated before `RunE` is entered — cobra only suppresses usage when
`SilenceUsage` is true *after* `RunE` has been called, but the flag gates the usage print in
`Execute`, so setting it on the root is sufficient to suppress it for all `RunE` errors.

If testing reveals cobra still prints usage for certain error paths, set `cmd.SilenceUsage = true`
at the top of each `RunE` body as a belt-and-suspenders measure.

### Step 2 — `diagsError` sentinel type and `formatDiagnostics` helper

Add a new file [internal/cli/diags.go](../internal/cli/diags.go) with:

```go
package cli

import (
    "fmt"
    "strings"

    "github.com/hashicorp/hcl/v2"
)

// diagsError wraps hcl.Diagnostics as an error. Its Error() string formats each
// diagnostic on its own line with severity, file:line:col, summary, and detail.
// This replaces the single-line diags.Error() output that discards location info.
type diagsError struct {
    diags hcl.Diagnostics
}

func (e *diagsError) Error() string {
    return formatDiagnostics(e.diags)
}

// newDiagsError returns a *diagsError wrapping the provided diagnostics.
// Returns nil if diags contains no errors (warnings are dropped; call sites that
// want to surface warnings should do so before calling this).
func newDiagsError(diags hcl.Diagnostics) error {
    var errs hcl.Diagnostics
    for _, d := range diags {
        if d.Severity == hcl.DiagError {
            errs = append(errs, d)
        }
    }
    if len(errs) == 0 {
        return nil
    }
    return &diagsError{diags: errs}
}

// formatDiagnostics formats all diagnostics in diags, one per block, with
// file path and line/column information when available.
func formatDiagnostics(diags hcl.Diagnostics) string {
    var b strings.Builder
    for _, d := range diags {
        sev := "Error"
        if d.Severity == hcl.DiagWarning {
            sev = "Warning"
        }
        if d.Subject != nil && d.Subject.Filename != "" {
            fmt.Fprintf(&b, "%s: %s:%d,%d: %s\n",
                sev,
                d.Subject.Filename,
                d.Subject.Start.Line,
                d.Subject.Start.Column,
                d.Summary,
            )
        } else {
            fmt.Fprintf(&b, "%s: %s\n", sev, d.Summary)
        }
        if d.Detail != "" {
            // Indent detail lines for visual separation.
            for _, line := range strings.Split(strings.TrimRight(d.Detail, "\n"), "\n") {
                fmt.Fprintf(&b, "  %s\n", line)
            }
        }
    }
    return strings.TrimRight(b.String(), "\n")
}
```

### Step 3 — Replace `diags.Error()` at all affected call sites

**`internal/cli/compile.go` — `parseCompileForCli`:**

```go
// Before:
return nil, nil, fmt.Errorf("parse: %s", diags.Error())
// After:
return nil, nil, fmt.Errorf("parse errors in %s:\n%w", workflowPath, newDiagsError(diags))

// Before:
return nil, nil, fmt.Errorf("compile: %s", diags.Error())
// After:
return nil, nil, fmt.Errorf("compile errors in %s:\n%w", workflowPath, newDiagsError(diags))
```

**`internal/cli/apply_setup.go`:**

```go
// Before:
return nil, nil, nil, fmt.Errorf("parse: %s", diags.Error())
// After:
return nil, nil, nil, fmt.Errorf("parse errors:\n%w", newDiagsError(diags))

// Before:
return nil, nil, nil, fmt.Errorf("compile: %s", diags.Error())
// After:
return nil, nil, nil, fmt.Errorf("compile errors:\n%w", newDiagsError(diags))
```

**`internal/cli/reattach.go`:**

```go
// Before:
return nil, fmt.Errorf("parse workflow: %s", diags.Error())
// After:
return nil, fmt.Errorf("parse workflow:\n%w", newDiagsError(diags))

// Before:
return nil, fmt.Errorf("compile workflow: %s", diags.Error())
// After:
return nil, fmt.Errorf("compile workflow:\n%w", newDiagsError(diags))
```

**`internal/cli/validate.go`** — already writes directly to stderr, but still uses
`diags.Error()`. Replace the three `diags.Error()` calls with `formatDiagnostics(diags)`:

```go
// Before:
fmt.Fprintf(os.Stderr, "%s: parse failed:\n%s\n", path, diags.Error())
// After:
fmt.Fprintf(os.Stderr, "%s: parse failed:\n%s\n", path, formatDiagnostics(diags))
```

(Repeat for the compile and warnings calls on lines 51 and 56.)

### Step 4 — `main.go` error printer

With `SilenceErrors` left at its default (`false`), cobra prints the returned error to stderr
and `main.go` currently also prints it:

```go
if err := root.Execute(); err != nil {
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
}
```

Set `SilenceErrors: true` on the root to prevent cobra from printing the error itself
(cobra would otherwise print it a second time). Keep the `main.go` handler as the single
error printer:

```go
root := &cobra.Command{
    Use:          "criteria",
    Short:        "Criteria agent — local workflow executor",
    SilenceUsage: true,
    SilenceErrors: true,
}
// ...
if err := root.Execute(); err != nil {
    fmt.Fprintln(os.Stderr, err)
    os.Exit(1)
}
```

This gives one clean error output path: the error string printed by `main.go`, which for
diagnostic errors is now the multi-line `formatDiagnostics` output.

### Step 5 — Tests

Add to `internal/cli/diags_test.go` (new file):

1. **`TestFormatDiagnostics_WithSubject`** — diagnostic with `Subject` set; output contains
   `filename.hcl:3,5:` and the summary string.

2. **`TestFormatDiagnostics_WithDetail`** — diagnostic with both `Summary` and `Detail`; output
   contains the detail text indented by two spaces.

3. **`TestFormatDiagnostics_NoSubject`** — diagnostic with nil `Subject`; output contains the
   summary but no colon-separated file path.

4. **`TestFormatDiagnostics_MultipleErrors`** — two error diagnostics; output contains both
   summaries, each on a separate line, with no truncation.

5. **`TestFormatDiagnostics_WarningLabel`** — diagnostic with `Severity == hcl.DiagWarning`;
   output starts with `Warning:`.

6. **`TestNewDiagsError_NilOnWarningsOnly`** — diagnostics slice containing only warnings;
   `newDiagsError` returns `nil`.

7. **`TestNewDiagsError_NonNilOnErrors`** — diagnostics slice with at least one error;
   `newDiagsError` returns non-nil and its `.Error()` contains the error summary.

Add integration-level assertions to the existing `TestParseCompileForCli_MissingFile`
([internal/cli/compile_test.go:160](../internal/cli/compile_test.go#L160)) and any existing
error-path tests: assert that the returned error string does **not** contain `"; "` (old
semicolon-concatenated format) when multiple diagnostics are expected.

## Desired output shape

Before (current):

```
Error: compile: workflow.initial_state is required; step "run": adapter ref "shell.default" is not declared; and 3 other diagnostics

Usage:
  criteria compile <workflow.hcl|dir> [flags]
  ...
```

After (target):

```
compile errors in examples/hello:
Error: examples/hello/main.hcl:3,3: workflow.initial_state is required
  Set initial_state to the name of the first step or state the workflow should enter.
Error: examples/hello/main.hcl:12,5: step "run": adapter ref "shell.default" is not declared
  Declare an adapter block: adapter "shell" "default" { ... }
Error: examples/hello/main.hcl:18,1: step "run": at least one outcome is required
```

## Behavior change

**Yes — user-visible output changes.**

- Help/usage text no longer appears after a compile, parse, or runtime error.
- Diagnostic errors now appear one per block with file path, line, column, summary, and detail.
- No diagnostics are truncated; all errors in a single run are shown.
- `validate` warnings also gain file/line context.
- The exit code behavior is unchanged (non-zero on any error).
- No change to the wire contract, engine runtime, or `workflow/` package.

## Out of scope

- Colorized output (ANSI codes) — that is a separate QoL item.
- Sourcing file content to show the offending source line (requires reading files at print time).
- Changing how non-diagnostic errors (e.g. network failures, file permission errors) are formatted.
- Any change to the `workflow/` package, wire contract, or engine.

## Files this workstream may modify

- `cmd/criteria/main.go` — add `SilenceUsage: true`, `SilenceErrors: true` to root.
- `internal/cli/diags.go` — new file: `diagsError`, `newDiagsError`, `formatDiagnostics`.
- `internal/cli/diags_test.go` — new file: 7 unit tests.
- `internal/cli/compile.go` — 2 `diags.Error()` call sites in `parseCompileForCli`.
- `internal/cli/apply_setup.go` — 2 `diags.Error()` call sites.
- `internal/cli/reattach.go` — 2 `diags.Error()` call sites.
- `internal/cli/validate.go` — 3 `diags.Error()` call sites.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [x] Add `SilenceUsage: true` and `SilenceErrors: true` to root command in `cmd/criteria/main.go`.
- [x] Create `internal/cli/diags.go` with `diagsError`, `newDiagsError`, `formatDiagnostics`.
- [x] Replace 2 `diags.Error()` calls in `internal/cli/compile.go`.
- [x] Replace 2 `diags.Error()` calls in `internal/cli/apply_setup.go`.
- [x] Replace 2 `diags.Error()` calls in `internal/cli/reattach.go`.
- [x] Replace 3 `diags.Error()` calls in `internal/cli/validate.go`.
- [x] Create `internal/cli/diags_test.go` with 7 unit tests.
- [x] `make build` clean.
- [x] `make test` clean.

## Exit criteria

- `criteria compile examples/hello` on a workflow with multiple errors prints each error on its
  own line with file path and line/column; no `"; "` separator; no truncation.
- The usage/help menu does not appear after a compile, parse, or file-not-found error.
- `criteria validate` warnings include file/line context.
- `make test` clean.

## Reviewer notes

**Implementation complete.** All 9 tasks checked; `make build` and `make test` both green.

### Changes made

- **`cmd/criteria/main.go`**: Added `SilenceUsage: true` and `SilenceErrors: true` to the root
  cobra command. `SilenceErrors` prevents cobra's duplicate error print; `main.go` remains the
  single error output path. `SilenceUsage` suppresses the help block after any `RunE` error.

- **`internal/cli/diags.go`** (new): `diagsError` wraps `hcl.Diagnostics` and formats each
  diagnostic with severity label, `file:line,col:` prefix (when `Subject` is set), summary, and
  indented detail. `newDiagsError` filters out warnings and returns `nil` for warning-only slices.
  `formatDiagnostics` is the shared formatter used by both the error type and `validate.go`'s
  direct stderr writes.

- **`internal/cli/compile.go`**: Two `diags.Error()` calls in `parseCompileForCli` replaced with
  `fmt.Errorf("parse errors in %s:\n%w", workflowPath, newDiagsError(diags))` and
  `fmt.Errorf("compile errors in %s:\n%w", workflowPath, newDiagsError(diags))`.

- **`internal/cli/apply_setup.go`**: Two `diags.Error()` calls replaced with
  `newDiagsError`-wrapped errors using `parse errors:` and `compile errors:` prefixes.

- **`internal/cli/reattach.go`**: Two `diags.Error()` calls replaced with `newDiagsError`-wrapped
  errors using `parse workflow:` and `compile workflow:` prefixes.

- **`internal/cli/validate.go`**: Three `diags.Error()` calls replaced with
  `formatDiagnostics(diags)` — parse failed, compile failed, and warnings paths.

- **`internal/cli/diags_test.go`** (new): 7 unit tests covering all specified cases:
  with-subject, with-detail, no-subject, multiple-errors (no semicolons), warning label,
  nil-on-warnings-only, non-nil-on-errors (warnings dropped from output).

### Validation

- `make build`: exit 0
- `make test -race ./...`: exit 0, all packages pass
- Targeted test run: all 7 new diags tests + `TestParseCompileForCli_MissingFile` pass

### Remediation — review-2026-05-08 blockers

#### Blocker 1 — SilenceUsage split: per-RunE instead of root-level

**Root cause**: In cobra v1.9.1, `ExecuteC` checks `!cmd.SilenceUsage && !c.SilenceUsage` (OR logic on root). Setting `SilenceUsage: true` on the root command causes it to suppress usage for ALL errors including argument-count failures.

**Fix**: Removed `SilenceUsage: true` from the root command in `cmd/criteria/main.go`. Added `cmd.SilenceUsage = true` as the first statement in every `RunE` body across all subcommands: `compile.go`, `apply.go`, `plan.go`, `validate.go`, `status.go` (both status and stop), `run.go`. This ensures:
- Argument-count errors (before `RunE` is entered): `SilenceUsage` is still `false` → usage IS printed ✓
- Runtime/compile/parse errors (after `RunE` sets it): `SilenceUsage = true` → usage NOT printed ✓

Verified manually: `criteria compile /no/such/file.hcl` → no usage block; `criteria compile` (no args) → usage block shown.

#### Blocker 2 — Integration-level format and usage-behavior assertions

Added three tests to `internal/cli/compile_test.go`:

- **`TestParseCompileForCli_MissingFile`** (extended): now asserts error string does NOT contain `"; "` (old semicolon-flattened format).
- **`TestCompileCmd_UsageSuppressedForRuntimeError`**: calls `NewCompileCmd()` with a non-existent path, captures stdout and stderr via `SetOut`/`SetErr`, asserts no `"Usage:"` in combined output.
- **`TestCompileCmd_UsageShownForArgCountError`**: calls `NewCompileCmd()` with zero args (ExactArgs(1) violation), asserts cobra's usage block IS in stdout.
- **`TestCompileCmd_MultiErrorFormat`**: writes a broken HCL workflow to a temp dir, compiles it, asserts the error uses multi-line format (no `"; "` separator).

Note: cobra v1.9.1 prints usage via `c.Println` (→ stdout) and errors via `c.PrintErrln` (→ stderr). Tests capture both streams accordingly.

### Review 2026-05-08-03 — remediation

#### Blocker 1 — Root command hierarchy tests added

Added `buildTestRoot()` helper in `compile_test.go` that mirrors the exact production wiring from `cmd/criteria/main.go` (`SilenceErrors: true` on root, no `SilenceUsage` on root). Added two root-level tests:

- **`TestRootCmd_UsageSuppressedForRuntimeError`**: runs `criteria compile /no/such/workflow.hcl` through the wired root; asserts no `"Usage:"` in combined stdout/stderr. Would catch any regression where `root.SilenceUsage` is accidentally set.
- **`TestRootCmd_UsageShownForArgCountError`**: runs `criteria compile` (no args) through the wired root; asserts `"Usage:"` IS in stdout. Proves arg-count UX is preserved end-to-end.

#### Blocker 2 — Multi-error fixture produces and asserts 2+ diagnostics

Replaced the single-error `workflow "bad"` fixture with a fixture that reliably produces 3 compile errors (missing `initial_state`, missing `target_state`, undeclared adapter reference). Added assertion: `strings.Count(errStr, "Error:") >= 2`. The test now fails if the formatter truncates or collapses diagnostics.

#### Validation

- `make build`: exit 0
- `make test -race ./...`: exit 0, all packages pass
- `make lint`: exit 0, no new baseline entries
- All 6 new compile_test.go tests pass: `TestParseCompileForCli_MissingFile`, `TestCompileCmd_UsageSuppressedForRuntimeError`, `TestCompileCmd_UsageShownForArgCountError`, `TestRootCmd_UsageSuppressedForRuntimeError`, `TestRootCmd_UsageShownForArgCountError`, `TestCompileCmd_MultiErrorFormat`

Adding `cmd.SilenceUsage = true` to `NewValidateCmd`'s `RunE` body pushed the function to 51 lines (funlen limit 50). Fixed by extracting the validate loop into `runValidate(paths, subworkflowRoots []string) bool`. The extraction also:
- Matched the original `context.Background()` pattern (not threading an external context into the function) to avoid a `contextcheck` finding identical to those already in the baseline for `apply_setup.go`, `compile.go`, and `reattach.go`.
- Combined same-type parameters (`paths, subworkflowRoots []string`) to satisfy `paramTypeCombine` (gocritic).

`make build` + `make test` + `make lint` all clean after this fix. No new baseline entries needed.

- `make build`: exit 0
- `make test -race ./...`: exit 0, all packages pass
- `criteria compile /no/such/file.hcl`: multi-line diagnostic, no usage block
- `criteria compile` (no args): usage block shown correctly

#### Summary

Most of the formatter work is in place and the new diagnostic rendering behaves correctly for parse, compile, and warning output. However, the current root-command `SilenceUsage` change suppresses usage for argument-count errors too, which violates the workstream's Step 1 intent to suppress help only for non-argument/runtime failures. Test coverage is also below the acceptance bar: the required error-path assertions were not added, and there is still no automated proof for the changed CLI contract at the root-command boundary.

#### Plan Adherence

- Step 1 is only partially satisfied: `cmd/criteria/main.go` now suppresses usage for non-argument errors, but it also suppresses usage for `criteria compile` with missing args, which is outside the intended behavior.
- Steps 2 through 4 are implemented and the observed parse/compile/validate formatting matches the desired multi-line diagnostic shape.
- Step 5 is incomplete: `internal/cli/diags_test.go` covers the formatter helpers, but `internal/cli/compile_test.go` still leaves `TestParseCompileForCli_MissingFile` as a nil-check only, and there is no automated coverage for the root CLI behavior change.

#### Required Remediations

- **Blocker — `cmd/criteria/main.go:14-19`**: The root-level `SilenceUsage: true` currently removes usage output for argument-validation failures as well. Reproduce with `go run ./cmd/criteria compile`, which now prints only `accepts 1 arg(s), received 0` and no usage/help text. **Acceptance criteria:** preserve the intended behavior split: usage/help must remain available for argument-count/usage mistakes, while compile/parse/file-not-found/runtime errors must not print the help block.
- **Blocker — `internal/cli/compile_test.go:169-174`, CLI boundary coverage missing**: the workstream required integration-level assertions on the changed error shape, but `TestParseCompileForCli_MissingFile` still does not assert the new formatting, lack of semicolon flattening, or file-context output. There is also no automated test proving that usage is suppressed for non-argument errors and retained for argument errors. **Acceptance criteria:** add regression tests that fail if the old `diags.Error()` one-line format returns, fail if non-argument errors print usage, and fail if argument-count errors stop printing usage/help.

#### Test Intent Assessment

The new helper tests in `internal/cli/diags_test.go` do a good job pinning the formatter's basic string rendering. What they do not prove is the actual CLI contract that changed in this workstream: root-command error handling, usage suppression semantics, and end-to-end stderr output for command failures. As written, the test suite can stay green while the CLI regresses on missing-arg UX, which is exactly what the current implementation does.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- `go run ./cmd/criteria compile /no/such/file.hcl` — confirmed clean multi-line diagnostic output with no usage block.
- `go run ./cmd/criteria compile` — confirmed usage/help is incorrectly suppressed for an argument-count error.
- `go run ./cmd/criteria validate <temp warning fixture>` — confirmed warnings now include `file:line,col` context and detail text.

### Review 2026-05-08-02 — changes-requested

#### Summary

The CLI behavior is now correct in manual validation: runtime/diagnostic failures no longer print usage, argument-count failures do, and formatted diagnostics still include location/detail context. I am not approving yet because the new tests still do not prove the real regression stays fixed at the root-command boundary, and the new “multi-error” regression test does not actually exercise multiple diagnostics.

#### Plan Adherence

- Step 1 is behaviorally fixed: the root command no longer suppresses usage globally, and `cmd.SilenceUsage = true` is now applied inside `RunE`, which preserves usage for argument validation while suppressing it for runtime failures.
- Steps 2 through 4 remain correctly implemented.
- Step 5 is still incomplete at the acceptance-bar level: new tests were added, but they do not fully validate the changed CLI contract.

#### Required Remediations

- **Blocker — root CLI contract test still missing (`cmd/criteria/main.go`, `internal/cli/compile_test.go:182-207`)**: the new usage-behavior tests call `NewCompileCmd()` directly, not the actual root command hierarchy. That means they would not have caught the original regression, which came from `root.SilenceUsage` in `cmd/criteria/main.go`. **Acceptance criteria:** add an automated test that executes the real command tree (`criteria compile ...`) through a root command equivalent to production wiring and proves both branches: missing args still print usage, runtime/parse/file errors do not.
- **Blocker — `internal/cli/compile_test.go:210-230` does not test multi-error formatting**: `TestCompileCmd_MultiErrorFormat` writes a fixture that currently produces a single parse diagnostic (`Unsupported argument`) and then only asserts the absence of `"; "`. It does not prove multiple diagnostics are emitted on separate lines, so a broken formatter could still pass. **Acceptance criteria:** use a fixture that reliably produces multiple diagnostics and assert at least two distinct diagnostic blocks/lines are present, alongside the existing no-semicolon check.

#### Test Intent Assessment

`internal/cli/diags_test.go` remains solid for unit coverage of the formatter helper. The new command tests improve coverage, but the contract-strength is still insufficient: testing a subcommand in isolation does not pin the root-command wiring that caused the earlier bug, and the current “multi-error” test is not regression-sensitive because it exercises only a single diagnostic. The suite can still go green while the actual root CLI behavior regresses.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- `./bin/criteria compile /no/such/file.hcl` — confirmed no usage block on runtime/parse failure.
- `./bin/criteria compile` — confirmed usage block is shown for an argument-count failure.
- `./bin/criteria compile <temp invalid HCL>` — confirmed multi-line diagnostic formatting for parse errors.

### Review 2026-05-08-04 — approved

#### Summary

Approved. The previous blockers are resolved: the root command no longer suppresses usage globally, root-level regression tests now exercise the real production-style command wiring, and the multi-error regression test now proves multiple diagnostics are emitted without semicolon flattening or truncation.

#### Plan Adherence

- Step 1 is satisfied: argument-count failures still print usage, while runtime/parse/file errors do not.
- Steps 2 through 4 are satisfied: all targeted `diags.Error()` call sites were replaced with structured multi-line formatting, and `validate` warnings include file/line context.
- Step 5 is satisfied: helper-level formatter tests remain in place, and the added compile/root-command tests now cover the CLI contract that changed in this workstream.

#### Test Intent Assessment

The test suite now pins the intended behavior instead of only the implementation details. `buildTestRoot()` exercises the same `SilenceErrors`/subcommand wiring as production, so a future reintroduction of root-level `SilenceUsage` would fail the root command tests. `TestCompileCmd_MultiErrorFormat` now uses a fixture that reliably emits multiple compile diagnostics and asserts multiple `Error:` blocks, making it regression-sensitive to truncation or one-line collapsing.

#### Validation Performed

- `make build` — passed.
- `make test` — passed.
- `make lint` — passed.
- `./bin/criteria compile /no/such/file.hcl` — confirmed no usage block on runtime/parse failure.
- `./bin/criteria compile` — confirmed usage block is shown for argument-count failure.
- `./bin/criteria compile <clean temp multi-error fixture>` — confirmed multiple diagnostics are emitted as separate `Error:` lines with no `"; "` flattening.
- `./bin/criteria validate <clean temp warning fixture>` — confirmed warnings include file/line context and detail text.

### Post-approval fix — duplicate `dotStepAttrs` removed

After approval, `make build` broke due to a duplicate `dotStepAttrs` function declaration in
`internal/cli/compile.go` (lines 497–523 were an exact copy of lines 469–495, introduced during
the BF-05 dot-renderer workstream merge). Removed the second declaration. `make build` and
`make test` are green.

### Review 2026-05-08-05 — approved

#### Summary

Approved. The follow-up change is the exact remediation needed for the post-approval break: it removes a duplicate `dotStepAttrs` declaration from `internal/cli/compile.go` without changing the surviving implementation, which restores a clean build and does not regress the BF-06 CLI formatting behavior.

#### Plan Adherence

- The original BF-06 scope remains satisfied: the diagnostic-formatting and usage-suppression changes reviewed in the prior approval are still intact.
- The latest executor change is a narrowly scoped compile-fix in adjacent code, justified because the duplicate symbol blocked `make build` after the prior approval.
- No new BF-06 scope deviations, contract changes, or undocumented baseline additions were introduced in this follow-up.

#### Test Intent Assessment

This follow-up does not change runtime behavior; it deletes an exact duplicate function body that caused a compile-time redeclaration failure. Full-suite coverage remains appropriate here because the key regression risk is build breakage rather than semantic drift, and the existing BF-06 formatter/CLI tests still cover the user-visible behavior approved earlier.

#### Validation Performed

- `git diff -- internal/cli/compile.go workstreams/bugfix-06-cli-error-formatting.md` — confirmed the code change is limited to removing the duplicate `dotStepAttrs` declaration and documenting the fix in the workstream.
- `git log --oneline -n 8 -- internal/cli/compile.go workstreams/bugfix-06-cli-error-formatting.md` — reviewed the recent history for the touched files.
- `make build` — passed.
- `make test` — passed.
