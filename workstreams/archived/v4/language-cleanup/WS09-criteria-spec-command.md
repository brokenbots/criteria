# WS09 — `criteria spec` command

**Phase:** Language Cleanup · **Track:** CLI / LLM ergonomics · **Owner:** Workstream executor · **Depends on:** WS07 (spec must be current before embedding it). · **Unblocks:** Any workflow where someone pastes the language spec into an LLM context; WS11 (langserver can reuse the embedded docs). · **Base branch:** `main`

## Context

The `docs/llm/README.md` instructs users to concatenate `LANGUAGE-SPEC.md` with eight pattern files to produce an LLM system prompt (~7,200 tokens). Today that requires manual file-cat or a custom script. A `criteria spec` command embeds those files at compile time and emits them on demand, making the LLM workflow one command:

```sh
criteria spec                  # emit LANGUAGE-SPEC.md
criteria spec --with-patterns  # emit spec + all 8 pattern files (ready-to-paste system prompt)
```

The files are embedded using `go:embed` so the command works from any directory and is always in sync with the binary's language version.

## Prerequisites

- WS07 merged (spec must be current before embedding it in the binary).

## In scope

### Step 1 — `internal/cli/spec.go`

New file. Embeds and emits the language spec and optional LLM pattern pack.

```go
package cli

import (
    _ "embed"
    "fmt"
    "io"
    "os"
    "strings"

    "github.com/spf13/cobra"
)

//go:embed ../../docs/LANGUAGE-SPEC.md
var embeddedLangSpec string

// embeddedPatterns lists the LLM prompt-pack pattern files in order.
// go:embed does not support computed paths, so each file is embedded individually.

//go:embed ../../docs/llm/01-linear.md
var llmPattern01 string

//go:embed ../../docs/llm/02-branching-switch.md
var llmPattern02 string

//go:embed ../../docs/llm/03-iteration-for-each.md
var llmPattern03 string

//go:embed ../../docs/llm/04-iteration-parallel.md
var llmPattern04 string

//go:embed ../../docs/llm/05-subworkflow.md
var llmPattern05 string

//go:embed ../../docs/llm/06-approval-and-wait.md
var llmPattern06 string

//go:embed ../../docs/llm/07-shared-variable.md
var llmPattern07 string

//go:embed ../../docs/llm/08-fileset-template.md
var llmPattern08 string

var llmPatterns = []string{
    llmPattern01, llmPattern02, llmPattern03, llmPattern04,
    llmPattern05, llmPattern06, llmPattern07, llmPattern08,
}

func NewSpecCmd() *cobra.Command {
    var withPatterns bool

    cmd := &cobra.Command{
        Use:   "spec",
        Short: "Print the Criteria workflow language specification",
        Long: `Print the Criteria workflow language specification to stdout.

With --with-patterns, also appends the eight LLM prompt-pack pattern files,
producing a complete system prompt for LLM-assisted workflow authoring.

Examples:
  criteria spec                           # print spec only
  criteria spec --with-patterns           # print spec + all patterns
  criteria spec --with-patterns | pbcopy  # copy to clipboard (macOS)
  criteria spec > spec.md                 # write to file`,
        Args: cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            cmd.SilenceUsage = true
            return printSpec(os.Stdout, withPatterns)
        },
    }

    cmd.Flags().BoolVar(&withPatterns, "with-patterns", false,
        "Append the eight LLM prompt-pack pattern files after the spec")

    return cmd
}

func printSpec(w io.Writer, withPatterns bool) error {
    if _, err := fmt.Fprint(w, embeddedLangSpec); err != nil {
        return err
    }
    if !withPatterns {
        return nil
    }
    for _, pattern := range llmPatterns {
        if _, err := fmt.Fprintf(w, "\n\n---\n\n%s", strings.TrimRight(pattern, "\n")); err != nil {
            return err
        }
    }
    _, err := fmt.Fprintln(w)
    return err
}
```

### Step 2 — Register the command

In `cmd/criteria/main.go`, add:

```go
root.AddCommand(cli.NewSpecCmd())
```

alongside the existing `cli.NewCompileCmd()`, `cli.NewPlanCmd()`, etc.

### Step 3 — Tests

**New file: `internal/cli/spec_test.go`**

```go
package cli_test

import (
    "bytes"
    "strings"
    "testing"
)

func TestPrintSpec_SpecOnly(t *testing.T) {
    var buf bytes.Buffer
    if err := printSpec(&buf, false); err != nil {
        t.Fatalf("printSpec error: %v", err)
    }
    out := buf.String()
    // Spec must contain the normative section headers
    for _, anchor := range []string{"## Blocks", "## Functions", "## Iteration semantics", "## Outcome model"} {
        if !strings.Contains(out, anchor) {
            t.Errorf("spec output missing expected anchor %q", anchor)
        }
    }
}

func TestPrintSpec_WithPatterns(t *testing.T) {
    var buf bytes.Buffer
    if err := printSpec(&buf, true); err != nil {
        t.Fatalf("printSpec error: %v", err)
    }
    out := buf.String()
    // All eight patterns must appear
    for _, marker := range []string{
        "Pattern: Linear", "Pattern: Branching switch",
        "Pattern: Sequential iteration", "Pattern: Concurrent iteration",
        "Pattern: Subworkflow", "Pattern: Approval and wait",
        "Pattern: Shared variable", "Pattern: File-driven",
    } {
        if !strings.Contains(out, marker) {
            t.Errorf("combined output missing pattern marker %q", marker)
        }
    }
}
```

Note: `printSpec` must be exported or the test must live in `package cli` (not `cli_test`) to call the unexported function. Either approach is fine; prefer `package cli` for white-box tests.

### Step 4 — Smoke-test the binary

After building (`make build` or `go build ./cmd/criteria`):

```sh
criteria spec | head -5            # should print the spec header
criteria spec --with-patterns | wc -l  # should be > 1000 lines
criteria spec --with-patterns | grep "Pattern:"  # should list all 8 patterns
```

## Out of scope

- `--format json` structured grammar output — requires a schema reflection API; deferred.
- `criteria spec --pattern <n>` to emit a single pattern — not needed; pipe and grep suffices.
- Embedding `docs/workflow.md` (the human reference) — the spec is sufficient for LLM use.

## Reuse pointers

- `go:embed` directive — `embed` package from stdlib; already used in test fixtures elsewhere.
- `cmd/criteria/main.go` — register pattern mirrors `cli.NewCompileCmd()`.
- `docs/llm/README.md` — documents token budget (~7,200 tokens) for the combined output.

## Behavior change

**New command:** `criteria spec` and `criteria spec --with-patterns`. No existing commands change. The binary size increases by the size of the embedded docs (~15 KB).

## Tests required

- `TestPrintSpec_SpecOnly` passes.
- `TestPrintSpec_WithPatterns` passes.
- `make build` succeeds (embed paths resolve).
- `go vet ./...` clean.

## Implementation Notes

### Checklist

- [ ] Step 1 — `internal/cli/spec.go` created with embedded files and `NewSpecCmd`
- [ ] Step 2 — `root.AddCommand(cli.NewSpecCmd())` added to `cmd/criteria/main.go`
- [ ] Step 3 — `internal/cli/spec_test.go` created with smoke tests
- [ ] Step 4 — Binary built and smoke-tested manually

### Reviewer Notes

_To be filled in during review._
