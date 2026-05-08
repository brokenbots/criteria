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

- [ ] Add `SilenceUsage: true` and `SilenceErrors: true` to root command in `cmd/criteria/main.go`.
- [ ] Create `internal/cli/diags.go` with `diagsError`, `newDiagsError`, `formatDiagnostics`.
- [ ] Replace 2 `diags.Error()` calls in `internal/cli/compile.go`.
- [ ] Replace 2 `diags.Error()` calls in `internal/cli/apply_setup.go`.
- [ ] Replace 2 `diags.Error()` calls in `internal/cli/reattach.go`.
- [ ] Replace 3 `diags.Error()` calls in `internal/cli/validate.go`.
- [ ] Create `internal/cli/diags_test.go` with 7 unit tests.
- [ ] `make build` clean.
- [ ] `make test` clean.

## Exit criteria

- `criteria compile examples/hello` on a workflow with multiple errors prints each error on its
  own line with file path and line/column; no `"; "` separator; no truncation.
- The usage/help menu does not appear after a compile, parse, or file-not-found error.
- `criteria validate` warnings include file/line context.
- `make test` clean.
