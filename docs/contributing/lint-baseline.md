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
| — (adjacent consistency) | CLI caller | `internal/cli/validate.go` | Updated to `CompileWithContext` for consistency with sibling CLI entrypoints; not a baseline-entry removal. |
| `gocognit` | `checkReachability` | `workflow/compile.go` | Extracted BFS + diagnostics into `compile_reachability.go`; function is now a 4-line delegator |
| `gocyclo` | `checkReachability` | `workflow/compile.go` | Same extraction |
| `funlen` | `checkReachability` | `workflow/compile.go` | Same extraction |
| `gocognit` | `compileSubworkflows` | `workflow/compile_subworkflows.go` | Extracted `compileSingleSubworkflow`, `buildChildOpts`, `detectSubworkflowCycle`, `missingResolverDiags`; function is now a 16-line orchestrator |
| `funlen` | `compileSubworkflows` | `workflow/compile_subworkflows.go` | Same extraction |

### Kept entries (16 remaining)

1. `workflow/compile_nodes.go` `gocognit` `compileWaits` — deferred to W04 (extract compile-node helpers)
2. `workflow/compile_nodes.go` `gocognit` `compileForEachs` — deferred to W04
3. `workflow/compile_nodes.go` `funlen` `compileForEachs` — deferred to W04
4. `workflow/compile_nodes.go` `gocyclo` `compileForEachs` — deferred to W04
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

## td-02 — Inline nolint suppression sweep (62 → 31) — 2026-05-13

- **Inline directives before:** 62
- **Inline directives after:** 31
- **Baseline cap before:** 16. **After:** 22 (6 new structural entries added).

### Category A — Directives removed by fixing the underlying code (22 removed)

| Fix | Files touched | Directives removed |
|-----|--------------|-------------------|
| Converted 13 internal conformance functions from `opts Options` to `opts *Options` | `conformance.go`, `conformance_happy.go`, `conformance_lifecycle.go`, `conformance_outcomes.go`, `assertions.go` | 13 × `gocritic` |
| Also converted `info plugin.Info` to `*plugin.Info` in 4 internal lifecycle/outcomes functions | same | 0 (newly exposed by opts conversion; fixed immediately) |
| Extracted `buildAdaptersJSON` + `buildStepsJSON` from `buildCompileJSON` | `internal/cli/compile.go` | 1 × `funlen` |
| Extracted `buildOrderedOutcomes` + `appendMissingOutcomes` from `formatOutcomes` | `internal/cli/plan.go` | 1 × `gocognit` |
| Extracted `sendPermissionRoundTrip` from N-iteration loop body | `internal/plugin/testfixtures/permissive/main.go` | 1 × `funlen` |
| Extracted `compileOneAdapter` + helpers (`resolveAdapterOnCrash`, `resolveAdapterEnv`, `resolveAdapterConfig`) | `workflow/compile_adapters.go` | 1 × `funlen` |
| Extracted `validateAdapterTraversalShape` | `workflow/compile_steps_adapter_ref.go` | 1 × `funlen` |
| Extracted `readStepBodyAttr` + `requireAbsTraversal` | `workflow/compile_step_target.go` | 2 × `funlen` |
| Extracted `buildHTTPSClient` from `serverHTTPClient` | `internal/cli/http.go` | 1 × `gocognit` |
| Extracted `advanceIteration` from `routeIteratingStepInGraph` | `internal/engine/engine.go` | 1 × `funlen` |

### Category B — Moved to baseline (9 inline directives removed, 6 new baseline entries)

These suppressions are structurally correct but inline noise is worse than baseline-file noise. Each carries a `# kept:` annotation in `.golangci.baseline.yml`.

| Entry | Linter | Reason |
|-------|--------|--------|
| `internal/adapter/conformance/conformance.go` `gocritic` hugeParam opts 80 bytes | gocritic | `Run` and `RunPlugin` are public API; converting to `*Options` would break all external callers |
| `internal/adapter/conformance/conformance_lifecycle.go` `funlen` `testConcurrentSessions` | funlen | Opens two full plugin sessions for parallel-goroutine isolation test; lifecycle scaffold is inherently long |
| `internal/cli/apply_local.go` `funlen` `runApplyLocal` | funlen | Orchestrates engine lifecycle, event routing, and output rendering; the phases are already minimal |
| `internal/cli/apply_local.go` `gocritic` hugeParam opts 232 bytes | gocritic | `applyOptions` threads through the apply pipeline; by-pointer conversion is W02-split-cli-apply scope |
| `internal/cli/apply_resume.go` `gocritic` hugeParam opts 232 bytes | gocritic | Same W02 scope rationale |
| `internal/cli/apply_server.go` `gocritic` hugeParam opts 232 bytes | gocritic | Same W02 scope rationale — covers 4 server-apply functions |

### Category C — Survivors: 31 directives remain inline

All surviving directives carry a self-contained one-sentence rationale. `W03`/`W11`/`W14`/`W17` workstream cross-references removed from all 22 that had them; missing rationale added to `tools/import-lint/main.go:139`.

| File:line | Rule(s) | Rationale |
|-----------|---------|-----------|
| `cmd/criteria-adapter-copilot/copilot_permission.go:93` | funlen,gocognit,gocyclo | collecting optional fields from a struct; splitting into helpers would obscure the data contract |
| `cmd/criteria-adapter-mcp/bridge.go:177` | funlen,gocognit | event-driven tool dispatch with permission gating and chunked output |
| `cmd/criteria-adapter-mcp/bridge.go:96` | funlen,gocyclo | complex session setup across MCP config, TLS, and stdio transport |
| `events/types.go:114` | funlen,gocyclo | discriminator switch must cover every concrete payload type in the oneof |
| `events/types.go:51` | funlen,gocyclo | type switch must cover every concrete payload type in the oneof |
| `internal/adapters/shell/shell.go:203` | nilerr | timeout is a step outcome, not a Go error |
| `internal/cli/localresume/resumer.go:117` | gocritic | Options is a config struct; callers pass by value intentionally |
| `internal/cli/plan.go:36` | funlen,gocognit,gocyclo | renders full plan tree with agent/step/outcome formatting across multiple output paths |
| `internal/cli/schemas.go:18` | gocognit,gocyclo | inherently complex: error handling branches per adapter type with partial failure tolerance |
| `internal/engine/engine_test.go:151` | gocritic | sprintfQuotedString: Sprintf needed to build HCL with literal quotes |
| `internal/engine/node_step.go:433` | err113 | msg is already fully contextual |
| `internal/plugin/loader.go:100` | funlen | resolver must handle builtin registry, discovery, launch, handshake, and caching paths |
| `internal/plugin/loader.go:207` | funlen,gocognit,gocyclo | execute path handles permission gating, event routing, and partial failure recovery |
| `internal/transport/server/client_streams.go:59` | funlen,gocognit,gocyclo | reconnect loop with backoff, ready signalling, and event dispatch across stream lifecycle |
| `sdk/conformance/ack.go:39` | funlen | sequential ordering test exercises many event/ack sequence steps |
| `sdk/conformance/ack.go:106` | funlen | idempotency test requires constructing duplicate ack sequences end-to-end |
| `sdk/conformance/ack.go:173` | funlen | concurrent stream test serialises two interleaved sequences with many assertions |
| `sdk/conformance/control.go:157` | funlen | agent isolation test requires full two-agent setup and cross-visibility assertions |
| `sdk/conformance/envelope.go:32` | funlen,gocognit | round-trip test must cover every envelope type to ensure TypeString stability |
| `sdk/conformance/inmem_subject_test.go:354` | nilerr | EOF is normal end-of-stream |
| `sdk/conformance/typestring.go:28` | funlen,gocognit | stability test enumerates all envelope types with submit/retrieve/compare steps |
| `sdk/events.go:1` | revive | Proto-generated Envelope_* alias names are wire-compatibility shims and cannot be renamed |
| `sdk/payloads_step.go:1` | revive | Proto-generated LogStream_* constant names are wire-compatibility shims and cannot be renamed |
| `tools/import-lint/main.go:139` | nilerr | unparseable files are intentionally skipped; callers treat nil results as no-violations |
| `workflow/compile_steps_iteration.go:18` | funlen | comprehensive iteration step: validates parallel/serial, adapter schema, subworkflow ref, and environment override in sequence |
| `workflow/compile_steps_subworkflow.go:15` | funlen | sequential compile+validate phases for subworkflow step; splitting adds indirection without clarity gain |
| `workflow/compile_validation.go:150` | funlen,gocognit,gocyclo | exhaustive schema validation with per-field type checks, required-field enforcement, and per-adapter diagnostics |
| `workflow/eval.go:628` | gocognit | scope restoration must handle iter cursors, nested vars, and multiple scope shapes |
| `workflow/parse_dir.go:74` | funlen | file discovery + per-file parse loop + merge + validation are sequential phases; extraction would obscure the flow |
| `workflow/parse_dir.go:177` | cyclop,gocognit,gocyclo,funlen | multi-field merge with singleton conflict detection requires sequential checks across all spec fields |
| `workflow/switch_compile_test.go:44` | gocritic | sprintfQuotedString: Sprintf needed to build HCL with literal quotes |

### New baseline entries (22 total, cap = 22)

17. `internal/adapter/conformance/conformance.go` `gocritic` hugeParam opts 80 bytes — public API value receiver (Run/RunPlugin); by-pointer conversion breaks external callers
18. `internal/adapter/conformance/conformance_lifecycle.go` `funlen` `testConcurrentSessions` — 55-statement test requiring full lifecycle scaffold for two parallel sessions
19. `internal/cli/apply_local.go` `funlen` `runApplyLocal` — 41-statement apply orchestrator; by-pointer is W02-split-cli-apply scope
20. `internal/cli/apply_local.go` `gocritic` hugeParam opts 232 bytes — applyOptions by value; W02 scope
21. `internal/cli/apply_resume.go` `gocritic` hugeParam opts 232 bytes — applyOptions by value; W02 scope
22. `internal/cli/apply_server.go` `gocritic` hugeParam opts 232 bytes — applyOptions by value across 4 functions; W02 scope

## td-03 (pre-Phase-4) — 2026-05-12

- Migrated copilot adapter off deprecated `PermissionRequestResultKindDenied*` values to the non-deprecated v0.3.0 equivalents (no SDK version bump — replacements already existed in v0.3.0).
- Path A: 4 inline `//nolint:staticcheck` directives removed; no baseline entries added.
- SDK version checked: v0.3.0 (latest stable). Successor API confirmed in v0.3.0 `types.go`:
  - `PermissionRequestResultKindDeniedCouldNotRequestFromUser` → `PermissionRequestResultKindUserNotAvailable`
  - `PermissionRequestResultKindDeniedInteractivelyByUser` → `PermissionRequestResultKindRejected`
- Side effect: removing the `//nolint:staticcheck` decorators revealed a latent `funlen` violation (function was 54 lines, exactly 4 over the 50-line limit; the 4 nolint-annotated lines had been excluded from golangci-lint's line count). Resolved by extracting `buildPermissionEvent` (a 9-line helper), reducing `handlePermissionRequest` to 46 lines. No new inline suppression or baseline entry was added.
- 4 new deny-path tests added in `copilot_permission_deny_test.go` covering: no-session, inactive-session, send-error, and interactive-deny scenarios.
