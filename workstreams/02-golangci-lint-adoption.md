# Workstream 2 — `golangci-lint` adoption

**Owner:** Workstream executor · **Depends on:** [W01](01-flaky-test-fix.md) · **Unblocks:** [W03](03-god-function-refactor.md), [W04](04-split-oversized-files.md), [W06](06-coverage-bench-godoc.md).

## Context

The Phase 0 tech evaluation flagged code-quality debt as the dominant
risk for Phase 1 velocity: 100+ line functions, high cyclomatic
complexity, spotty GoDoc on exported symbols. A linter is the cheapest
way to (a) establish a measurable baseline, (b) keep that baseline from
regressing during the rest of Phase 1, and (c) give every later
workstream a concrete punch-list of suppressions to burn down as it
touches each file.

This workstream adopts `golangci-lint` v1.64+ (the v1 line — v2 is
still in alpha at the time of writing; revisit when v2 is GA) across
all three modules (`./`, `./sdk`, `./workflow`). The configuration is
deliberately strict; existing findings are quarantined into a
**baseline-suppress file** so day one is green and subsequent
workstreams remove suppressions as they fix the underlying issues.

`funlen` and `gocyclo` are configured as **hard-fail with per-file
suppressions** so the suppression list functions as the explicit
punch-list for [W03](03-god-function-refactor.md). When W03 finishes a
function refactor, it must also delete the matching suppression.

## Prerequisites

- [W01](01-flaky-test-fix.md) merged. The baseline must be captured
  against a green, deterministic test suite; otherwise you cannot
  tell a real lint regression from a flake-induced rerun.
- `make build`, `make test`, `make lint-imports`, `make validate`
  green on `main`.

## In scope

### Step 1 — Pin the linter version

Pin `golangci-lint` v1.64.x (latest v1) by recording the exact
version in two places:

- **`tools/tools.go`** (new file) using the Go-tool blank-import
  pattern, so the linter version is part of `go.mod` and reproducible
  across contributors:

  ```go
  //go:build tools
  // +build tools

  package tools

  import (
      _ "github.com/golangci/golangci-lint/cmd/golangci-lint"
  )
  ```

- **`Makefile`** target `lint-go` that invokes the linter via
  `go tool` (Go 1.24+) or `go run` against the pinned version, never
  via a globally-installed binary.

If `go tool golangci-lint` is unavailable on the pinned Go version,
fall back to `go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.x`
with the version pinned in `Makefile` and document the rationale in
reviewer notes.

### Step 2 — Author `.golangci.yml`

Write `.golangci.yml` at the repo root with the exact configuration
below. Comments explain each non-default knob; preserve them.

```yaml
# golangci-lint configuration for the criteria repo.
# See https://golangci-lint.run/usage/configuration/ for option docs.

run:
  timeout: 5m
  # Lint all three modules in the workspace.
  modules-download-mode: readonly
  # Generated proto bindings are excluded via issues.exclude-dirs.

linters:
  disable-all: true
  enable:
    # Correctness
    - govet              # standard vet checks
    - staticcheck        # SA-series checks
    - errcheck           # unchecked errors
    - ineffassign        # ineffective assignments
    - unused             # unused symbols
    - gosimple           # simplifications
    - typecheck          # always on; safety net
    - bodyclose          # response.Body left open
    - rowserrcheck       # sql.Rows.Err() not checked
    - sqlclosecheck      # sql.Rows / sql.Stmt not closed
    - contextcheck       # context not propagated
    - nilerr             # returns nil after non-nil err check
    - errorlint          # %w / errors.Is/As correctness
    # Hygiene
    - gofmt
    - goimports
    - misspell
    - unconvert          # unnecessary type conversions
    - unparam            # unused function parameters / return values
    - prealloc           # slice prealloc opportunities
    - dupword            # accidental "the the" in comments
    # Complexity (hard-fail; suppressions are W03's punch-list)
    - funlen
    - gocyclo
    - gocognit
    # Style / API hygiene (hard-fail; revive carries doc-comment rule)
    - revive
    - gocritic
    - nakedret
    - nolintlint         # nolint directives must be specific + justified

linters-settings:
  funlen:
    # Tech eval target: no function > 50 lines outside generated code.
    lines: 50
    statements: 40

  gocyclo:
    min-complexity: 15

  gocognit:
    min-complexity: 20

  revive:
    rules:
      # GoDoc on exported symbols (drives W06).
      - name: exported
        arguments:
          - "checkPrivateReceivers"
          - "disableStutteringCheck"
      - name: package-comments
      - name: var-naming
      - name: receiver-naming
      - name: indent-error-flow
      - name: error-return
      - name: error-naming
      - name: error-strings
      - name: range-val-in-closure
      - name: superfluous-else
      - name: unreachable-code
      - name: redefines-builtin-id

  gocritic:
    enabled-tags:
      - diagnostic
      - performance
      - style
    disabled-checks:
      # ifElseChain fires too often on outcome-routing switches; keep them readable.
      - ifElseChain
      # whyNoLint is noisy in tandem with nolintlint.
      - whyNoLint

  nolintlint:
    require-explanation: true
    require-specific: true
    allow-unused: false

  errcheck:
    # Common ignored returns; document them so we don't silently grow this list.
    exclude-functions:
      - (io.Closer).Close
      - (*os.File).Close
      - fmt.Fprint
      - fmt.Fprintf
      - fmt.Fprintln

  goimports:
    local-prefixes: github.com/brokenbots/criteria

issues:
  # Day-one baseline lives in this file; W03/W04/W06 burn it down.
  exclude-files:
    - ".*\\.pb\\.go$"
    - ".*\\.connect\\.go$"
    - "sdk/pb/.*"
  exclude-dirs:
    - bin
    - tools
  exclude-rules:
    # Test files: relax funlen/gocyclo/gocognit and require less GoDoc.
    - path: _test\.go
      linters:
        - funlen
        - gocyclo
        - gocognit
        - revive
        - errcheck
    # main.go for adapter binaries: short bootstrap, no GoDoc requirement.
    - path: cmd/.*/main\.go
      linters:
        - revive
        - funlen
  max-issues-per-linter: 0
  max-same-issues: 0
  new: false
```

Do **not** widen `max-issues-per-linter` or `max-same-issues` from
zero. Either fix or suppress; never silently truncate.

### Step 3 — Generate the baseline suppression file

Run the linter against the current `main` and capture the result as
`.golangci.baseline.yml`. The intent: existing findings are
quarantined into per-file suppressions so the lint job goes green on
day one, and each subsequent workstream removes a chunk of them.

Use this exact procedure (record in reviewer notes):

```sh
# 1. Run the linter to discover every current finding.
go tool golangci-lint run --out-format=json ./... > .lint-baseline.json

# 2. Generate the suppression file from the JSON. The script lives in
#    tools/lint-baseline/ (new) and emits an `issues.exclude-rules:`
#    block keyed by (path, linter, text-prefix).
go run ./tools/lint-baseline -in .lint-baseline.json -out .golangci.baseline.yml

# 3. Wire the baseline file into golangci-lint via --config of a
#    composed file. golangci-lint does not natively merge multiple
#    config files, so the Makefile target concatenates .golangci.yml
#    + .golangci.baseline.yml into .golangci.merged.yml at build time
#    and points --config at the merged file. Document this in the
#    Makefile target.

rm .lint-baseline.json
```

The baseline file is checked in. Each suppression entry must
include:

- `path:` (file pattern, exact path preferred over wildcard).
- `linters:` (the single linter that fired; never group).
- `text:` (the exact diagnostic text or its stable prefix).
- A trailing comment naming the workstream that will remove it
  (e.g. `# W03: refactor resumeOneRun`).

Reviewer rejects suppressions that lack the workstream-pointer
comment.

The `tools/lint-baseline/` helper is a small Go program (≤ 200
lines) that reads the JSON output and emits the YAML. It does not
need tests beyond a golden-file round trip.

### Step 4 — Wire `make lint-go` and CI

Add to `Makefile`:

```makefile
lint-go: ## Run golangci-lint across all modules with the baseline allowlist
	@cat .golangci.yml .golangci.baseline.yml > .golangci.merged.yml
	go tool golangci-lint run --config .golangci.merged.yml ./...
	cd sdk      && go tool -C .. golangci-lint run --config ../.golangci.merged.yml ./...
	cd workflow && go tool -C .. golangci-lint run --config ../.golangci.merged.yml ./...
	@rm -f .golangci.merged.yml

lint: lint-imports lint-go ## Run all linters
```

Update `.PHONY` and the `ci` aggregate target to include `lint-go`.
Add `.golangci.merged.yml` to `.gitignore`.

Update `.github/workflows/ci.yml`: add a `lint-go` step after
`lint-imports` and before `build`. Use `actions/setup-go` (already
present) so the toolchain has `go tool`. Cache the linter binary if
the workflow run time grows past 60s on the lint step.

### Step 5 — Per-workstream burn-down contract

Document in **`docs/contributing/lint-baseline.md`** (new):

- What `.golangci.baseline.yml` is and why it exists.
- The rule: a workstream that touches a file with a baseline
  suppression must remove the suppression as part of its diff. The
  reviewer enforces this. Adding new suppressions requires a
  workstream-pointer comment naming who removes them.
- The merge gate: `make lint-go` must be green on every PR. There
  is no `--allow-failure` mode.

This file becomes the single source of truth for how the lint debt
is paid down. Cross-link it from `CONTRIBUTING.md` only if W06 is
also editing `CONTRIBUTING.md`; otherwise leave the cross-link to
[W11 Phase 1 cleanup gate](10-phase1-cleanup-gate.md).

## Out of scope

- Fixing the lint findings themselves. The baseline quarantines
  them; [W03](03-god-function-refactor.md), [W04](04-split-oversized-files.md),
  and [W06](06-coverage-bench-godoc.md) burn them down.
- Adding new linters not in the list above. New linters are a
  Phase 2 decision.
- Replacing `tools/import-lint/` with `golangci-lint`'s
  `depguard`. The custom import-lint encodes project-specific module
  boundaries that `depguard` cannot express cleanly. Keep both.
- Linting generated proto code.
- Editing `CHANGELOG.md`, `README.md`, `CONTRIBUTING.md`. Documentation
  beyond `docs/contributing/lint-baseline.md` is deferred to
  [W10 Phase 1 cleanup gate](10-phase1-cleanup-gate.md).

## Files this workstream may modify

- `.golangci.yml` (new)
- `.golangci.baseline.yml` (new, generated then committed)
- `tools/tools.go` (new)
- `tools/lint-baseline/main.go` (new)
- `tools/lint-baseline/main_test.go` (new)
- `tools/lint-baseline/testdata/` (new, golden round-trip fixture)
- `Makefile` (add `lint-go`, update `lint`, update `ci`, update `.PHONY`)
- `.github/workflows/ci.yml` (add `lint-go` step)
- `.gitignore` (add `.golangci.merged.yml`)
- `docs/contributing/lint-baseline.md` (new)
- `go.mod` / `go.sum` / `go.work.sum` (add the linter as a tool dep)

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any
other workstream file. It may **not** edit non-test source files in
`internal/`, `cmd/`, `sdk/`, or `workflow/` to fix lint findings —
that work belongs to W03/W04/W06.

## Tasks

- [ ] Add `tools/tools.go` with the pinned `golangci-lint` import.
- [ ] Run `go mod tidy` across all three modules; commit the
      resulting `go.mod` / `go.sum` / `go.work.sum` updates.
- [ ] Author `.golangci.yml` exactly as specified in Step 2.
- [ ] Build `tools/lint-baseline/` and a golden-file test for it.
- [ ] Generate `.golangci.baseline.yml`; annotate every entry with a
      workstream-pointer comment.
- [ ] Add `make lint-go` and update the `ci` target.
- [ ] Add the CI step.
- [ ] Author `docs/contributing/lint-baseline.md`.
- [ ] `make lint-go` exits 0 on `main` after baseline is committed.
- [ ] CI passes on this PR.

## Exit criteria

- `make lint-go` exits 0 against `main` with the baseline in place.
- `make ci` passes (`build`, `test`, `lint-imports`, `lint-go`,
  `validate`, `example-plugin`).
- `.golangci.yml` matches the spec in Step 2.
- Every entry in `.golangci.baseline.yml` has a workstream-pointer
  comment.
- Removing **any single** baseline entry causes `make lint-go` to
  fail (sanity check that the baseline isn't a paper tiger).
- `docs/contributing/lint-baseline.md` documents the burn-down
  contract.
- The CI workflow runs `lint-go` and gates merges on it.

## Tests

- Golden-file round-trip test for `tools/lint-baseline/`: given a
  fixed JSON input, the emitted YAML matches a checked-in golden.
- Manual verification that removing one baseline entry makes the
  lint job fail. Record the file/entry chosen and the failure
  message in reviewer notes.

## Risks

| Risk | Mitigation |
|---|---|
| The baseline file becomes a permanent allowlist that nobody pays down | Every entry carries a workstream-pointer comment. Reviewer notes for W03/W04/W06 must show net-negative line counts in the baseline file. The cleanup gate ([W10](10-phase1-cleanup-gate.md)) refuses to tag `v0.2.0` if the baseline still contains any `funlen`/`gocyclo` entries pointed at W03. |
| The pinned linter version drifts from contributors' local installs | The Makefile target uses `go tool` / `go run` against the pinned dep, never a global binary. CI uses the same path. Document in `docs/contributing/lint-baseline.md`. |
| `.golangci.merged.yml` build artifact gets accidentally committed | `.gitignore` entry; the `make lint-go` target removes it after running. CI has no commit step that would push it. |
| `revive`'s `exported` rule fires on legitimately internal-but-exported test helpers | The baseline absorbs day-one findings; W06 either documents the helper or moves it to a `_test.go` file. Do not silence `revive` globally. |
| `funlen` / `gocyclo` thresholds (50 lines / 15) are too aggressive and force pointless extraction | The thresholds match the tech-evaluation target. If a function genuinely cannot fit in 50 lines and 15 complexity, the W03 reviewer can grant a per-function `//nolint:funlen,gocyclo // <reason>` with explicit justification. The justification is the gate, not the threshold. |
| Lint runtime is slow enough to hurt PR feedback loop | Cache the linter binary in CI. If runtime > 90s, drop `gocritic`'s style tag (most expensive) and re-evaluate in [W10](10-phase1-cleanup-gate.md). |
| Pinned `golangci-lint` v1.64.x fails on `go 1.26` toolchain | Bump to the next v1.x patch that supports `go 1.26`; record the version in reviewer notes. If no v1.x supports `go 1.26`, escalate as `[ARCH-REVIEW]` with severity `blocker` — this changes the linter strategy. |
| `tools/lint-baseline/` becomes its own maintenance burden | Cap it at ~200 LOC. If the JSON-to-YAML transformation grows beyond that, consider committing the YAML by hand instead and deleting the tool — the tool is a convenience, not load-bearing. |
