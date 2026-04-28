# Workstream 6 — Third-party plugin example

**Owner:** Doc / engine agent · **Depends on:** [W03](03-public-plugin-sdk.md) · **Unblocks:** [W08](09-phase0-cleanup-gate.md).

## Context

Once [W03](03-public-plugin-sdk.md) lands a public plugin-author SDK,
the next missing piece is proof: an example plugin that lives outside
this repo's module, imports only the public SDK and the generated
proto bindings, and runs against `overseer apply`. Without this, the
"third-party plugins are possible" story is theoretical.

The split-era reviewer notes called this out as deferred work (W08
reviewer, "third-party 'hello world' overseer plugin example").

This workstream produces a small example repo (or example directory
that *could* become its own repo) that demonstrates the full path:
clone, build, install into `~/.overseer/plugins/`, run a workflow
that uses it, observe expected output.

## Prerequisites

- [W03](03-public-plugin-sdk.md) merged with the public SDK
  available at a stable import path.
- `make plugins` builds the bundled adapters successfully.

## In scope

### Step 1 — Pick the form

Two viable shapes:

- **Sibling repo** at e.g. `github.com/brokenbots/overseer-example-plugin-greeter`.
  Most realistic — proves the import works from outside this module
  with no replace directive. More overhead (separate repo, separate
  CI).
- **In-tree example directory** at e.g. `examples/plugins/greeter/`
  with its own `go.mod` so it imports the public SDK as an external
  module (using a `replace` directive only for local development).
  Less overhead, but an importer with a sharp eye sees the
  `replace` and questions whether the example is honest.

Recommend the in-tree directory with **no `replace` directive in the
committed `go.mod`** — the example pins the published SDK version
explicitly. A local-dev `go.work` file (gitignored) lets contributors
test against unreleased SDK changes; the committed example always
builds against a real published tag.

### Step 2 — Build the example

`examples/plugins/greeter/`:

- `go.mod` declaring its own module path and depending on
  `github.com/brokenbots/overseer/sdk@<latest>` (the public plugin
  SDK package from W03).
- `main.go` — a small adapter that takes a `name` input and returns
  `"hello, <name>"`.
- `README.md` — install + run instructions, written for a developer
  who has never seen this repo.
- A workflow file under `examples/plugins/greeter/example.hcl` that
  uses the adapter.

### Step 3 — Wire into CI

Add a `make example-plugin` target that:

- Builds the greeter plugin into the example's `bin/`.
- Copies it to a temp `OVERSEER_PLUGINS` dir.
- Runs `overseer apply` against `example.hcl`.
- Asserts the run completes and produces expected output.

CI runs `make example-plugin` after `make build`. Failure means the
public plugin SDK regressed in a way that broke an external consumer —
exactly the signal this workstream exists to catch.

### Step 4 — Document

Update `docs/plugins.md` to reference the greeter example as the
canonical "minimum third-party plugin". Replace any older inline
sample code with a pointer.

## Out of scope

- Authoring a sibling repo. The in-tree directory is enough proof.
  Spawning a real sibling repo can happen later if external authors
  want a starter template.
- Demonstrating advanced plugin features (sessions, streaming
  responses, permission negotiation). The greeter is intentionally
  minimal.
- Multi-language plugin examples. Go-only.

## Files this workstream may modify

- `examples/plugins/greeter/` (new directory).
- `Makefile` (new `example-plugin` target).
- `.github/workflows/ci.yml` (new step running `make example-plugin`).
- `docs/plugins.md` (pointer update).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
or other workstream files.

## Tasks

- [x] Pick the form (in-tree directory recommended).
- [x] Author the greeter `main.go`, `go.mod`, `README.md`, `example.hcl`.
- [x] Add `make example-plugin` target.
- [x] Wire into CI.
- [x] Update `docs/plugins.md`.
- [x] Verify `make example-plugin` exits 0 against the published SDK
      version (or against the in-tree SDK if no published version
      yet, with a forward-pointer comment).

## Exit criteria

- `examples/plugins/greeter/` exists and builds with no `replace`
  directive in its committed `go.mod` (or, if the published SDK
  version doesn't yet exist, a documented temporary `replace`
  with a follow-up to remove it after [W08](09-phase0-cleanup-gate.md)
  cuts the first tag).
- `make example-plugin` runs end-to-end and asserts output.
- CI gates `make example-plugin` on every PR.
- `docs/plugins.md` points at the example.

## Tests

- The `make example-plugin` end-to-end check is the test.
- A regression here is a regression in the public plugin SDK
  contract (the W03 deliverable).

## Risks

| Risk | Mitigation |
|---|---|
| Example go.mod pins a specific SDK version that lags master | Acceptable; bumping the pin is one PR. The CI gate catches breakage early; the cost is one bump per minor SDK release. |
| Example becomes an unmaintained drift point as the SDK evolves | The CI gate is the maintenance forcing function. If the example fails to build, it's blocking; that means it gets fixed. |
| In-tree example with `replace` masks real external-author breakage | Hard rule: no `replace` in the committed `go.mod` once W08 cuts a tag. Until then, document the temporary `replace` with an explicit follow-up issue. |
| The example's HCL accidentally exercises non-public engine behavior | Keep the example small and read-only against the SDK contract. If the engine internals leak through, that's a W03 bug, not a W06 bug — file accordingly. |

## Reviewer Notes

**Implementation complete. Ready for review.**

### Form chosen
In-tree directory at `examples/plugins/greeter/` with its own `go.mod`
(module `example.com/overseer-adapter-greeter`). Demonstrates the full external-author
path: separate module, no imports from `internal/`, only `sdk/pluginhost` and
`sdk/pb/overseer/v1`.

### Temporary replace directive
`go.mod` includes a `replace github.com/brokenbots/overseer/sdk => ../../../sdk`
with a `TODO(W08)` comment. The zeroth SDK tag has not been cut yet. Once W08
tags the first release, remove the replace and update the require line to the
published version.

### Files created/modified
- `examples/plugins/greeter/main.go` — greeter plugin implementation
- `examples/plugins/greeter/go.mod` + `go.sum` — standalone module
- `examples/plugins/greeter/example.hcl` — workflow exercising the adapter
- `examples/plugins/greeter/README.md` — install/run instructions for plugin authors
- `Makefile` — added `example-plugin` target (build → temp plugin dir → apply → assert)
- `.github/workflows/ci.yml` — new step `Run example plugin end-to-end`
- `docs/plugins.md` — updated "Writing Your Own Plugin" section to lead with the greeter example

### Validation
- `make example-plugin` exits 0 locally ✓
- Events file contains `"hello, world"` in both `StepLog` and `StepOutputCaptured` events ✓
- `make build test lint-imports validate` all pass ✓
- Greeter's `example.hcl` validates cleanly with `overseer validate` ✓

### Security
- No user input reaches a shell or file system. `name` is only used in `fmt.Sprintf`.
- No credentials or secrets anywhere in the example.
- Plugin handshake cookie (`OVERSEER_PLUGIN`) gates subprocess startup.
- No new external dependencies (only the in-tree SDK via replace).

---

### Review 2026-04-27 — changes-requested

#### Summary

The core deliverables are solid: `examples/plugins/greeter/` exists with a correct `main.go`, `go.mod`, `README.md`, and `example.hcl`; the `make example-plugin` target builds, runs, and asserts output; the CI step is wired; and `docs/plugins.md` is updated. The temporary `replace` directive is documented appropriately with a `TODO(W08)`. No security concerns. Three required remediations below — all executor-level nits that must be resolved before approval; none require architectural coordination.

#### Plan Adherence

- [x] Pick the form — in-tree directory chosen. ✓
- [x] Author `main.go`, `go.mod`, `README.md`, `example.hcl` — all present and correct. ✓
- [x] Add `make example-plugin` target — implemented, asserts `"hello, world"` in events file. ✓
- [x] Wire into CI — `.github/workflows/ci.yml` step added after `make validate`. ✓
- [x] Update `docs/plugins.md` — pointer added at top of "Writing Your Own Plugin". ✓
- [x] Verify `make example-plugin` exits 0 — confirmed locally. ✓
- Exit criterion: `go.mod` has no `replace` once tag exists, or temporary `replace` documented — documented with `TODO(W08)`. ✓
- Exit criterion: `make example-plugin` runs end-to-end and asserts output — confirmed. ✓
- Exit criterion: CI gates `make example-plugin` on every PR — met via direct step in `ci.yml`. ✓
- Exit criterion: `docs/plugins.md` points at the example — met. ✓

#### Required Remediations

1. **`make ci` target does not include `example-plugin`** (nit)
   - File: `Makefile` line 82
   - `ci: build test lint-imports validate` omits `example-plugin`, yet its comment reads "Run all CI gates (build, test, lint-imports, validate)". A developer running `make ci` locally misses the e2e check that GitHub Actions runs.
   - **Acceptance criteria**: Add `example-plugin` to the `ci` target's prerequisites and update the comment to include it, so `make ci` faithfully mirrors what the CI workflow runs.

2. **`README.md` Go version claim contradicts `go.mod`** (minor)
   - File: `examples/plugins/greeter/README.md` line 12; `go.mod` line 3
   - `README.md` states "Go 1.22+" as a prerequisite, but `go.mod` declares `go 1.26`. Go 1.22 cannot build a module that requires 1.26. An external plugin author following the README will hit an immediate build failure.
   - **Acceptance criteria**: Update `README.md` to state the correct minimum Go version matching the `go` directive in `go.mod` (currently `1.26`).

3. **`example.hcl` excluded from `make validate` glob** (nit)
   - File: `Makefile` line 54; `examples/plugins/greeter/example.hcl`
   - `make validate` globs `examples/*.hcl` and does not cover `examples/plugins/**/*.hcl`. While `make example-plugin` implicitly validates the HCL through `apply`, static validation (`overseer validate`) is not run on it. If a future contributor adds more HCL files under `examples/plugins/` and expects `make validate` to cover them, it will silently not do so.
   - **Acceptance criteria**: Extend the `validate` target glob to include `examples/plugins/**/*.hcl` (e.g., iterate `examples/plugins/*/` after `examples/`), or add a comment on the `validate` target noting that plugin example HCL files are covered by `make example-plugin` instead.

**All three remediations applied:**
- `ci` target now: `build test lint-imports validate example-plugin` ✓
- `README.md` updated to "Go 1.26+" ✓
- `validate` glob extended to `examples/*.hcl examples/plugins/*/*.hcl` ✓

#### Test Intent Assessment

The workstream plan explicitly designates the `make example-plugin` e2e run as the sole test, and that framing is acceptable for a documentation/example artefact. Assessment against the rubric:

- **Behavior alignment**: ✓ The `grep -q '"hello, world"' "$eventsfile"` check maps directly to the user-visible contract (greeting appears in the run output).
- **Regression sensitivity**: ✓ A plugin that produced no output, the wrong greeting, or a non-zero exit would fail the check.
- **Failure-path coverage**: acceptable. The plan explicitly limits scope to the happy path; the empty-name default (`name = "world"`) is exercised by the workflow but not the empty-input branch independently. Given the "intentionally minimal" mandate, this is within stated scope.
- **Contract strength**: The grep catches the greeting value but does not assert the `outcome = "success"` or the `greeting` output key specifically. Acceptable given the plan's minimal-example framing, but noted: a future hardening pass (in W08 or later) could strengthen the assertion to verify outcome and output key.
- **Determinism**: ✓ No flakiness vectors observed.

Overall test intent: sufficient for the stated purpose; the three remediations above are all non-test issues.

#### Security Findings

No security concerns. The plugin binary handles only a static string through `fmt.Sprintf`; no shell execution, no file I/O, no external inputs reach the plugin at runtime in the example workflow. The `OVERSEER_PLUGIN` handshake cookie gates subprocess startup per the existing plugin model. No new external dependencies are introduced.

#### Validation Performed

```
make build          → exit 0
make test           → exit 0 (all packages pass)
make lint-imports   → exit 0 (Import boundaries OK)
make validate       → exit 0 (5 examples validated)
make example-plugin → exit 0 (greeter built, applied, assertion passed)
./bin/overseer validate examples/plugins/greeter/example.hcl → ok
```

---

### Review 2026-04-27-02 — approved

#### Summary

All three required remediations from the 2026-04-27 pass are resolved. (1) `example-plugin` is now a prerequisite of `make ci` with an updated comment. (2) `README.md` now states "Go 1.26+ (matches the `go` directive in `go.mod`)". (3) `make validate` glob extended to `examples/plugins/*/*.hcl`, confirmed to cover `examples/plugins/greeter/example.hcl`. All deliverables are correct, clean, and consistent. No open issues.

#### Plan Adherence

All tasks complete. All exit criteria met. No deviations from plan.

#### Validation Performed

```
make validate       → exit 0 (6 examples validated, including examples/plugins/greeter/example.hcl)
make example-plugin → exit 0 (greeter built, applied, assertion passed)
make ci             → exit 0 (all gates pass including example-plugin)
```
