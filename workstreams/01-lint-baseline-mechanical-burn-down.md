# Workstream 1 — Lint baseline mechanical burn-down

**Owner:** Workstream executor · **Depends on:** none · **Unblocks:** [W02](02-lint-ci-gate.md), [W08](08-contributor-on-ramp.md) (good-first-issue material).

## Context

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md))
parks the project at **Tech Debt = C** primarily because of a 240-entry,
962-line `.golangci.baseline.yml` carrying suppressions tagged
`W03=42`, `W04=133`, `W06=54`, `W10=11`. About 80 of those entries are
purely mechanical: 71 `gofmt`, 40 `goimports`, 10 `unused` findings —
most of them artifacts of the W04 file-split that landed in Phase 1.
Another ~27 are `revive` rules suppressing proto-generated names
(`Envelope_*`, `LogStream_*`) that are untouchable without regenerating
protos.

This workstream burns down the mechanical chunk and re-classifies the
proto-generated `revive` entries from baselined-debt to permanent
`//nolint:revive` annotations with justifications. The targets are:

- W04 entries: from 133 → < 40
- Total baseline: from 240 → ≤ 120

The non-mechanical residuals (W03 funlen/gocyclo on
`handlePermissionRequest`, real `unused` cases that need code review,
W06 style findings) stay for [W03](03-copilot-file-split.md) and a
later phase. The point of this workstream is to remove the mass of
debt-paid-with-debt so the *real* exceptions are visible.

## Prerequisites

- `make ci` green on `main`.
- Local Go toolchain ≥ the version pinned in `go.mod`.
- `goimports` installed (`go install golang.org/x/tools/cmd/goimports@latest`).

## In scope

### Step 1 — Mechanical formatting pass

Run from repo root:

```sh
gofmt -w $(git ls-files '*.go')
goimports -w $(git ls-files '*.go' | grep -v '\.pb\.go$' | grep -v '\.pb\.gw\.go$')
```

Excluding generated files (`*.pb.go`, `*.pb.gw.go`) from `goimports` is
deliberate — those files are managed by `make proto`, not by hand.

After the pass, run `make lint-go` and check:

- gofmt entries in `.golangci.baseline.yml` should drop to zero.
- goimports entries should drop to zero.
- All previously-baselined `gofmt` and `goimports` lines tagged
  `# W04:` are removed from `.golangci.baseline.yml`.

If `make lint-go` reports new findings the pass cannot remove (e.g. an
import that goimports cannot order because of a build tag), document
each remaining finding with a `//nolint:goimports // <justification>`
inline annotation, not a baseline entry.

### Step 2 — Dead-code review for `unused` findings

The 10 `unused` baseline entries are individual judgement calls. For
each one:

1. Identify the symbol from the baseline-line context (file:line + rule).
2. Inspect the symbol. If it is genuinely dead code, **delete it**.
3. If it is part of an exported public API and intentionally unused
   internally (e.g. a struct field for future use, a method required by
   an interface), keep the symbol and convert the baseline entry to an
   inline `//nolint:unused // <reason>` with a one-sentence
   justification.
4. If the symbol is referenced only by tests in a different package,
   confirm the tests still compile and run.

Do not preserve dead code "in case we need it later."

### Step 3 — Reclassify proto-generated `revive` suppressions

Approximately 27 of the 54 W06-tagged entries suppress `revive`
findings on proto-generated names like `Envelope_TYPE_X` or
`LogStream_KIND_Y`. These names cannot be renamed without breaking the
wire contract.

For every such entry:

1. Locate the generated file (`sdk/pb/criteria/v1/*.pb.go`).
2. Add a single `//nolint:revive // proto-generated; cannot rename
   without breaking wire contract` annotation **at the top of the
   file** (file-level nolint), not per-symbol.
3. Remove the corresponding entries from `.golangci.baseline.yml`.

If `make proto` regenerates these files, the file-level annotation
must be re-added. Update `tools/proto-gen/` (or the equivalent
generation script) to inject the `//nolint:revive` header so the
annotation survives regeneration. If the generation tooling does not
support a header inject, document this in the workstream notes and add
a Makefile post-step that prepends the line — but a generator-side fix
is preferred.

### Step 4 — Validate baseline counts

After Steps 1–3, verify:

```sh
wc -l .golangci.baseline.yml
grep -c '^\s*-' .golangci.baseline.yml
```

The total baseline entry count must be ≤ 120. If it is higher,
investigate which class of finding survived and whether Step 1 missed
files (e.g. a build-tagged `_test.go` file).

Also check distribution:

- `# W04:` entries: < 40
- `# gofmt` entries: 0
- `# goimports` entries: 0 (excepting generated files)
- `# revive` proto-name entries: 0 (replaced by file-level nolint)

### Step 5 — Document the burn-down in `tools/lint-baseline/`

Update `tools/lint-baseline/README.md` (or whatever the convention
file is — check `docs/contributing/lint-baseline.md`) to note the
counts before and after this workstream. Include the rule-level
breakdown so future contributors know what the residual baseline
contains. Do **not** edit `PLAN.md`, `README.md`, `AGENTS.md`, or
`CHANGELOG.md` — those are owned by [W14](14-phase2-cleanup-gate.md).

## Behavior change

**No behavior change.** This workstream is mechanical formatting and
suppression hygiene. The lock-in is the existing test suite plus
`make lint-go` itself. All existing unit, integration, and conformance
tests must pass unchanged. No HCL surface change. No CLI flag change.
No event change. No log change. No new errors.

If any test fails after Step 1's mechanical pass, the failure is a
pre-existing bug exposed by reformatting — investigate and fix as
part of this workstream (it counts as scope) but document it
explicitly in reviewer notes.

## Reuse

- The lint baseline tooling lives in `tools/lint-baseline/`. Reuse
  `make lint-go` and the existing baseline diff/cap script — do not
  reimplement.
- Existing `.golangci.yml` rule configuration is correct; this
  workstream does not edit `.golangci.yml`, only `.golangci.baseline.yml`.

## Out of scope

- W03-tagged `funlen` / `gocyclo` entries on `handlePermissionRequest`
  and `permissionDetails`. Those move with [W03](03-copilot-file-split.md).
- Real (non-mechanical) `unused` findings that uncover dead code in
  active subsystems. If removal is non-trivial, leave the entry, file
  a follow-up, and document in reviewer notes.
- Adding new linter rules to `.golangci.yml`. New rules belong in a
  later phase.
- Editing generated proto files by hand to "fix" naming. Wire contract
  is immutable in this workstream.
- Changes to the lint CI gate. That is [W02](02-lint-ci-gate.md).

## Files this workstream may modify

- Any non-generated `*.go` file under the repo (mechanical formatting
  only, except for genuinely dead code removal in Step 2).
- `.golangci.baseline.yml` (entry removals only).
- `sdk/pb/criteria/v1/*.pb.go` — file-level `//nolint:revive` header
  only; do not edit generated symbols.
- `tools/proto-gen/` (if it exists, to inject the `//nolint:revive`
  header) — otherwise the generation Makefile target.
- `docs/contributing/lint-baseline.md` (update count snapshot).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [ ] Run `gofmt -w` and `goimports -w` across non-generated `*.go`.
- [ ] Remove `# W04:`-tagged gofmt and goimports entries from
      `.golangci.baseline.yml`.
- [ ] Triage all `unused` baseline entries; delete dead code or convert
      to inline `//nolint:unused`.
- [ ] Reclassify proto-generated `revive` suppressions to file-level
      `//nolint:revive`; update the generator (or Makefile) to keep
      the header on regen.
- [ ] Verify `make lint-go` clean.
- [ ] Verify total baseline entry count ≤ 120.
- [ ] Update `docs/contributing/lint-baseline.md` count snapshot.
- [ ] `make ci` green.

## Exit criteria

- `make lint-go` exits 0 from a clean tree on the workstream branch.
- `.golangci.baseline.yml` has ≤ 120 entries.
- W04-tagged entries < 40 (down from 133).
- Zero `gofmt` and zero `goimports` baseline entries (excepting
  generated files where applicable).
- Zero proto-generated `revive` baseline entries (replaced by
  file-level nolint).
- `make test -race -count=1` green across all three modules.
- `make ci` green.

## Tests

This workstream does not add new tests. The signal is:

- `make ci` green proves the formatting pass did not break anything.
- The reduced baseline count proves the burn-down landed.
- A regeneration of the proto bindings (`make proto`) followed by
  `make lint-go` proves the file-level nolint survives proto regen.

Reviewer should run `make proto && make lint-go` once locally to
confirm Step 3 is durable.

## Risks

| Risk | Mitigation |
|---|---|
| `goimports` reorders an import group inside a build-tagged file in a way that breaks compilation on a non-default build | Run `make ci` after the mechanical pass; investigate any build-tag failures and inline-nolint rather than baseline. |
| The proto generator strips file-level comments on regen | Add the `//nolint:revive` header injection to the generator script (preferred) or as a Makefile post-step (fallback). Document the choice in reviewer notes. |
| Removing dead code in Step 2 turns out to break a downstream consumer | Run `make ci` after each removal. Removed code can be restored in the same PR if a consumer surfaces. |
| The baseline drops below the cap [W02](02-lint-ci-gate.md) is going to enforce | This is the desired outcome — W02 sizes its cap from W01's final count. |
