# Lint Baseline — Burn-Down Contract

This document explains how the lint baseline works, how to remove entries from
it, and why `make lint-go` is a hard PR gate.

## What is `.golangci.baseline.yml`?

`.golangci.baseline.yml` is a generated suppression file that quarantines
pre-existing lint findings on day one. Running `golangci-lint` against the
current `main` found ~230 issues — mostly long functions (`funlen`/`gocyclo`),
missing GoDoc (`revive`), and import formatting (`goimports`). Rather than
blocking every PR until all 230 are fixed, the baseline file suppresses them so
the lint job is green immediately. Each subsequent workstream removes the
suppressions it has already fixed.

The key insight: the baseline is **not a permanent allowlist**. It is a
punch-list. Every entry is annotated with the workstream that will remove it,
for example:

```yaml
    - path: internal/engine/engine.go
      linters:
        - funlen
      text: 'Function ''runLoop''' # W03: refactor runLoop
```

## How is the merged config assembled?

`golangci-lint` v1 does not support multiple config files natively. The
`lint-go` Makefile target assembles a temporary `.golangci.merged.yml` at
build time:

```sh
cat .golangci.yml > .golangci.merged.yml
tail -n +3 .golangci.baseline.yml >> .golangci.merged.yml
```

`.golangci.yml` ends with `issues.exclude-rules:` as the last section. The
`tail -n +3` strips the `issues:` and `exclude-rules:` header lines from the
baseline file and appends the baseline entries directly into that list. The
merged file is deleted after `golangci-lint` exits.

**Never commit `.golangci.merged.yml`** — it is listed in `.gitignore`.

## How is the linter invoked?

The linter is pinned via the Go 1.24+ `tool` directive in the root module's
`go.mod`:

```
tool github.com/golangci/golangci-lint/cmd/golangci-lint
```

Always invoke it through `go tool golangci-lint` (or `make lint-go`), never
through a globally-installed binary. This guarantees every contributor and the
CI runner use exactly the same version (v1.64.8 at time of writing).

In a Go workspace, `go tool golangci-lint` is accessible from any workspace
module directory because the tool is registered in the root module.

## The burn-down rule

**A workstream that touches a file with a baseline suppression must remove the
suppression as part of its diff.**

Concretely:
1. When a workstream refactors a function that has a `funlen` or `gocyclo`
   baseline entry, it must delete that entry from `.golangci.baseline.yml`.
2. When a workstream adds GoDoc to an exported symbol, it must delete the
   corresponding `revive` entry.
3. When a workstream reformats a file (e.g., via `goimports`), it must delete
   the `goimports` entry.

The reviewer enforces this. A PR that fixes the underlying issue but leaves the
baseline entry should not be merged.

## W01 snapshot (mechanical burn-down)

W01 removed mechanical suppressions (`gofmt`, `goimports`, `unused`) and moved
proto-name `revive` suppressions for `sdk/events.go` and
`sdk/payloads_step.go` to file-level `//nolint:revive` with wire-compatibility
justification.

| Snapshot | Total | W03 | W04 | W06 | W10 |
|---|---:|---:|---:|---:|---:|
| Before W01 | 240 | 42 | 133 | 54 | 11 |
| After W01 | 117 | 42 | 38 | 29 | 8 |

Residual baseline by linter after W01:

| Linter | Count |
|---|---:|
| `funlen` | 30 |
| `gocritic` | 25 |
| `gocognit` | 18 |
| `gocyclo` | 13 |
| `contextcheck` | 9 |
| `errcheck` | 9 |
| `revive` | 9 |
| `staticcheck` | 3 |
| `nilerr` | 1 |

**Adding new suppressions** (e.g., for a legitimately complex function that
cannot be simplified) requires:
- A workstream-pointer comment naming who removes it.
- An explicit justification in the PR description.

## The merge gate

`make lint-go` must exit 0 on every PR. There is no `--allow-failure` mode and
no way to skip it: the CI job runs `make lint-go` after `make lint-imports` and
before `make build`.

`make lint-baseline-check` is a second lint gate. It compares the current
baseline entry count to `tools/lint-baseline/cap.txt` and fails if the baseline
grows beyond the cap. The count is produced by `go run ./tools/lint-baseline
-count .golangci.baseline.yml`, which currently counts top-level
`- path:` entries under `issues.exclude-rules`. If the baseline file format
changes, update the count mode in `tools/lint-baseline/main.go`.

If you introduce a new lint violation, you have two options:
1. Fix the underlying issue (preferred).
2. Add a suppression entry to `.golangci.baseline.yml` with a workstream-pointer
   comment and a justification comment in the PR.

## Branch protection

Branch protection for `main` must require the `Lint` status check and must
disallow direct pushes. All changes go through pull requests so lint and baseline
cap policy are enforced uniformly.

If the baseline cap must increase, do it as a separate, reviewable commit that
updates only `tools/lint-baseline/cap.txt` with explicit reviewer agreement.
Applying branch protection is an admin action; [W14](../../workstreams/14-phase2-cleanup-gate.md)
tracks verification that this setting is active.

## Regenerating the baseline

If a workstream makes changes that cause entirely new findings (e.g., a new
linter is enabled), regenerate the baseline:

```sh
# 1. Capture findings for all three modules.
go tool golangci-lint run --out-format=json ./... > /tmp/r.json
(cd sdk      && go tool golangci-lint run --out-format=json ./... > /tmp/s.json)
(cd workflow && go tool golangci-lint run --out-format=json ./... > /tmp/w.json)

# 2. Merge and generate.
python3 -c "
import json
all = []
for f in ['/tmp/r.json', '/tmp/s.json', '/tmp/w.json']:
    all.extend((json.load(open(f)).get('Issues') or []))
json.dump({'Issues': all, 'Report': {}}, open('/tmp/all.json', 'w'))
"
go run ./tools/lint-baseline -in /tmp/all.json -out .golangci.baseline.yml

# 3. Verify lint-go is green.
make lint-go
```

Note: golangci-lint's internal issue ordering can cause suppressing one issue to
reveal another. If `make lint-go` still fails after the first generation, repeat
the capture+generate cycle using the merged config until the run is stable.

## Linters and their owning workstreams

| Linter | Workstream |
|--------|-----------|
| `funlen`, `gocyclo`, `gocognit` | W03 — god-function refactor |
| `revive`, `gocritic` (style/doc) | W06 — coverage, bench, godoc |
| Everything else | W04 — split oversized files / general hygiene |

## Phase 3 W01 snapshot (mechanical burn-down)

W01 (Phase 3) removed mechanical suppressions: all `errcheck`, `revive` (naming), and
`contextcheck` findings (context threading), and most `gocritic` findings
(rangeValCopy, unnamedResult, emptyStringTest, builtinShadow, stringXbytes, hugeParam
where feasible). This reduced the baseline from 70 to 20 entries — well below the ≤ 50
target.

Starting count (v0.2.0 tag): **70**

Final count (this workstream): **20**

Per-rule change:

| Linter | Before (v0.2.0) | After | Notes |
|---|---:|---:|---|
| `errcheck` | 9 | 0 | All fixed |
| `contextcheck` | 9 | 0 | All fixed; final 2 via new RunFailed/StepResumed ctx-bearing methods |
| `gocritic` | 24 | 1 | 19 fixed; 4 hugeParam fixed by pointer conversion; 1 hugeParam kept (applyOptions/W02); 3 dead entries removed |
| `revive` | 9 | 0 | All fixed (internal-test function renames) |
| `gocognit` | 7 | 7 | Deferred to W03/W07 siblings |
| `gocyclo` | 6 | 6 | Deferred to W03/W02 siblings |
| `funlen` | 6 | 6 | Deferred to W02/W03 siblings |

## Phase 3 W03 snapshot (split compile_steps.go)

W03 split the 622-LOC `workflow/compile_steps.go` monolith into 5 focused files:
`compile_steps.go` (dispatcher), `compile_steps_adapter.go`, `compile_steps_workflow.go`,
`compile_steps_iteration.go`, and `compile_steps_graph.go`.
The three `compileSteps` baseline entries (`gocognit`, `funlen`, `gocyclo`) were
removed because the function itself no longer exists — replaced by a ≤96-LOC thin
dispatcher.

Starting count (after Phase 3 W01): **20**

Final count (this workstream): **17**

Per-rule change:

| Linter | Before | After | Notes |
|---|---:|---:|---|
| `gocognit` | 7 | 6 | `compileSteps` entry removed |
| `gocyclo` | 6 | 5 | `compileSteps` entry removed |
| `funlen` | 6 | 5 | `compileSteps` entry removed |

`cap.txt` lowered from 20 → 17.

### Kept entries (gocritic hugeParam)

One `hugeParam` entry remains for `applyOptions` in `internal/cli/apply.go`
(208 bytes). `applyOptions` is threaded through 6 apply-command functions; converting
all 6 to pointer is a broad refactor owned by W02-split-cli-apply. The entry carries a
`# kept:` annotation in `.golangci.baseline.yml`.

## Phase 4 td-01 snapshot (lint baseline ratchet 24 → 16) — 2026-05-12

- Starting count: **24**
- Final count: **16**
- Cap: 24 → **16**

### Removed entries

| Linter | Function | File | Reason |
|--------|----------|------|--------|
| `contextcheck` | CLI caller | `internal/cli/apply_setup.go` | Added `CompileWithContext(ctx, ...)` exported function; CLI callers now thread request context directly |
| `contextcheck` | CLI caller | `internal/cli/compile.go` | Same: CLI caller updated to `CompileWithContext` |
| `contextcheck` | CLI caller | `internal/cli/reattach.go` | Same: CLI caller updated to `CompileWithContext` |
| `gocognit` | `checkReachability` | `workflow/compile.go` | Extracted BFS + diagnostics into `compile_reachability.go`; function is now a 4-line delegator |
| `gocyclo` | `checkReachability` | `workflow/compile.go` | Same extraction |
| `funlen` | `checkReachability` | `workflow/compile.go` | Same extraction |
| `gocognit` | `compileSubworkflows` | `workflow/compile_subworkflows.go` | Extracted `compileSingleSubworkflow`, `buildChildOpts`, `detectSubworkflowCycle`, `missingResolverDiags`; function is now a 16-line orchestrator |
| `funlen` | `compileSubworkflows` | `workflow/compile_subworkflows.go` | Same extraction |

### Kept entries (16 remaining)

1. `compile_nodes.go` `gocognit` `compileWaits` — deferred to W04 (extract compile-node helpers)
2. `compile_nodes.go` `gocognit` `compileForEachs` — deferred to W04
3. `compile_nodes.go` `funlen` `compileForEachs` — deferred to W04
4. `compile_nodes.go` `gocyclo` `compileForEachs` — deferred to W04
5. `workflow/compile.go` `gocognit` `resolveTransitions` — deferred to W04
6. `workflow/compile.go` `funlen` `resolveTransitions` — deferred to W04
7. `workflow/compile.go` `gocyclo` `resolveTransitions` — deferred to W04
8. `workflow/eval.go` `gocognit` `SerializeVarScope` — deferred to W10 (cursor-stack serialisation complexity)
9. `workflow/eval.go` `gocyclo` `SerializeVarScope` — deferred to W10
10. `workflow/eval.go` `funlen` `SerializeVarScope` — deferred to W10
11. `internal/cli/apply.go` `gocritic` hugeParam `applyOptions` (232 bytes) — deferred to W02 (split-cli-apply); converting 6 threading sites to pointer is out of td-01 scope
12. `workflow/compile_steps_graph.go` `gocognit` `nodeTargets` — deferred to W16 (switch case added complexity)
13. `workflow/compile_switches.go` `funlen` `compileSwitchConditionBlock` — deferred to W16
14. `sdk/conformance/lifecycle.go` `gocognit` `testAdapterSessionEventsRoundTrip` — deferred to W12 (conformance test, exhaustive event validation)
15. `sdk/conformance/lifecycle.go` `funlen` `testAdapterSessionEventsOrdered` — deferred to W12
16. `sdk/conformance/lifecycle.go` `funlen` `testAdapterSessionEventsRoundTrip` — deferred to W12

