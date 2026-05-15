# WS01 — Terminology unification: rename `plugin` → `adapter` everywhere

**Phase:** Adapter v2 · **Track:** Foundation · **Owner:** Workstream executor · **Depends on:** none · **Unblocks:** every subsequent workstream in this phase.

## Context

The codebase uses both "plugin" and "adapter" inconsistently:

| Surface | Term used today | Source |
|---|---|---|
| HCL user-facing block | `adapter` | `workflow/schema.go:148–154` |
| Binary naming | `criteria-adapter-<name>` | `internal/plugin/discovery.go:12` |
| Internal package | `plugin` | `internal/plugin/` |
| Proto service | `AdapterPluginService` | `proto/criteria/v1/adapter_plugin.proto:10` |
| Dispenser constant | `PluginName = "adapter"` | `internal/plugin/serve.go:17` |
| Docs filename | `docs/adapters.md` | `docs/adapters.md` |

Users see "adapter" in HCL; developers wading into the host code see "plugin." That mixed vocabulary is friction for newcomers, hurts grep-ability, and obscures intent. The Adapter v2 plan (see `README.md` D6) standardizes on **adapter** everywhere.

This workstream is purely a rename. It is the first workstream in the phase because every other workstream touches code that gets renamed here; doing it first means no later workstream has to land its changes against soon-to-be-renamed files. Behavior is unchanged — `make ci` is the verification.

## Prerequisites

- `make ci` green on `main` (the branch this workstream lands against).
- No outstanding PRs touching `internal/plugin/`, `proto/criteria/v1/`, or `docs/adapters.md`.
- A draft of [`README.md`](README.md) reviewed so the executor understands the v2 picture (terminology choices and the hard-cut decision in D2).

## In scope

### Step 1 — Rename `internal/plugin/` to `internal/adapter/`

```sh
git mv internal/plugin internal/adapter
```

Update every import path in the repository:

```sh
gofmt-aware-rewrite() {
  goimports -w $(grep -rl "criteria/internal/plugin" --include='*.go' .)
}
```

Concretely, every occurrence of `"github.com/brokenbots/criteria/internal/plugin"` becomes `"github.com/brokenbots/criteria/internal/adapter"`. Mechanical, ~40 files.

### Step 2 — Rename proto package and service

The proto file [`proto/criteria/v1/adapter_plugin.proto`](../../proto/criteria/v1/adapter_plugin.proto) stays in place for this workstream (the v2 proto lands in WS02). Rename only the **service** inside this file:

```diff
-service AdapterPluginService {
+service AdapterService {
   rpc Info(...)
   ...
 }
```

Update the Go generated stubs and every call site that references `AdapterPluginService`. Do not move the file or change its package name (`criteria.v1`) — the file gets superseded by `proto/criteria/v2/adapter.proto` in WS02 and deleted in WS37.

### Step 3 — Rename `PluginName` constant

In [`internal/adapter/serve.go`](../../internal/plugin/serve.go) (post-Step-1 path) and [`sdk/pluginhost/service.go`](../../sdk/pluginhost/service.go):

```diff
-const PluginName = "adapter"
+const AdapterName = "adapter"
```

Update every call to `rpcClient.Dispense(PluginName)` and every reference in tests.

### Step 4 — Rename SDK `pluginhost` package

Rename `sdk/pluginhost/` to `sdk/adapterhost/`. Update package declarations and every import. This is part of the public SDK surface; document the break in `CHANGELOG.md` (deferred to WS39 cleanup gate — leave a forward-pointer comment at the top of the new file).

### Step 5 — Rename `docs/adapters.md`

```sh
git mv docs/adapters.md docs/adapters.md
```

Update every cross-reference in the repo:

```sh
grep -rl "docs/adapters.md" --include='*.md' . | xargs sed -i.bak 's|docs/adapters.md|docs/adapters.md|g'
find . -name '*.bak' -delete
```

### Step 6 — Sweep stale "plugin" usages

Run:

```sh
grep -rn "[Pp]lugin" --include='*.go' --include='*.md' . | grep -v "go-plugin" | grep -v vendor/
```

For each remaining occurrence, decide:

- **HashiCorp `go-plugin`** library name — keep as-is (it's the upstream name).
- Code comment or doc referring to the *concept* of an adapter — change to "adapter."
- Variable name, type name, function name — rename to use `adapter`.
- Test name like `TestPluginLoader_*` — rename to `TestAdapterLoader_*`.

The grep result must be empty (modulo upstream `go-plugin` references) before this workstream ships.

### Step 7 — Update CLI help text and error messages

Search for user-visible strings in `internal/cli/`:

```sh
grep -rn '"plugin"' internal/cli/
grep -rn "'plugin'" internal/cli/
```

Replace each in error messages, help text, and log lines. Users should see "adapter" everywhere.

## Out of scope

- Adding the v2 proto file — that's WS02.
- Any behavior changes (this is rename-only).
- Changes to `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file. Those are the cleanup-gate's territory (WS39).
- The standalone proto repo (`criteria-adapter-proto`) — WS41.

## Reuse pointers

- Mechanical rename only — no new APIs, no new files (except the new files git creates from `git mv`).

## Behavior change

**No.** This is a pure rename. All tests pass unchanged. The wire protocol, RPC signatures, HCL grammar, and CLI commands behave identically.

## Tests required

- All existing tests pass: `make ci` (race + count=2 + lint + vet + staticcheck).
- A sanity grep:

  ```sh
  ! grep -rn "internal/plugin" --include='*.go' .
  ! grep -rn "AdapterPluginService" --include='*.go' --include='*.proto' .
  ! grep -rn "PluginName" --include='*.go' .
  ! test -f docs/adapters.md
  ! test -d internal/plugin
  ```

  All five must return exit code 1 (no matches / does not exist).

## Exit criteria

- `make ci` green.
- The five sanity greps above return no matches.
- `docs/adapters.md` exists and renders correctly.
- A single PR landed; CHANGELOG entry deferred to WS39 cleanup gate (with a forward-pointer comment in this PR's description).

## Implementation notes

### Deviation from spec: `internal/plugin` → `internal/adapterhost` (not `internal/adapter`)

The workstream spec says `git mv internal/plugin internal/adapter`, but `internal/adapter` already exists as a separate package holding `EventSink`, `Result`, and the `Adapter` interface. Renaming to `internal/adapter` would cause a package name collision. Resolved by using `internal/adapterhost`, which mirrors the `sdk/pluginhost` → `sdk/adapterhost` rename and clearly distinguishes the host-side process-management layer from the interface layer.

### JSON output field: `plugins_required` → `adapters_required`

The compile JSON output field was renamed from `"plugins_required"` to `"adapters_required"`. All golden test files under `internal/cli/testdata/` were updated to match. This is technically a user-visible JSON schema change; noted here for the WS39 cleanup gate to add a CHANGELOG entry.

### Implementation checklist

- [x] Step 1: `internal/plugin/` → `internal/adapterhost/` (all imports, package decls, callers)
- [x] Step 2: Proto service `AdapterPluginService` → `AdapterService`, proto regenerated
- [x] Step 3: `PluginName` → `AdapterName` globally
- [x] Step 4: `sdk/pluginhost/` → `sdk/adapterhost/` (package, imports, doc.go)
- [x] Step 5: `docs/plugins.md` → `docs/adapters.md`, cross-references updated
- [x] Step 6: Full sweep — `ErrPluginNotFound`, `pluginBinaryPrefix`, `Plugin`→`Handle` type,
           `AdapterMap`, `rpcHandle`, `copilotPlugin`→`copilotAdapter`, `pluginSessionID`→`adapterSessionID`,
           `buildNoopPlugin`→`buildNoopAdapter`, `BuildPermissivePlugin`→`BuildPermissiveAdapter`,
           `publicSDKPlugin`→`publicSDKAdapter`, `RunPlugin`→`RunAdapter`, all test stub types,
           and all comment/string occurrences of "plugin" in scope
- [x] Step 7: CLI strings — `"plugins required:"` → `"adapters required:"`, `"plugins_required"` → `"adapters_required"`
- [x] All five sanity greps: CLEAN
- [x] `make test` green

### Reviewer notes

- The `CRITERIA_PLUGINS` env var, `~/.criteria/plugins/` discovery path, and `CRITERIA_PLUGIN` magic cookie are intentionally preserved — they are user-visible and changing them would be a breaking behavior change outside this workstream's scope.
- `hplugin` import alias (HashiCorp `go-plugin` library) is preserved throughout — it's the upstream library name.
- The `examples/plugins/greeter/` directory was NOT renamed — directory renames in examples are out of scope for this pure-rename workstream (the directory name is part of the example's public path).
- Golden test files updated for `adapters_required` JSON field rename.

## Files this workstream may modify

- Everything under `internal/plugin/` (which becomes `internal/adapter/`) and `sdk/pluginhost/` (which becomes `sdk/adapterhost/`).
- `proto/criteria/v1/adapter_plugin.proto` (service rename only).
- Generated proto Go stubs.
- Every file in the repo that imports the renamed packages or uses the renamed constants — mechanical edits only.
- `docs/adapters.md` → `docs/adapters.md`.
- Test files matching the rename pattern.

## Files this workstream may NOT edit

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`.
- `workstreams/README.md`.
- Any other workstream file in `workstreams/` (including this directory's other WS files).
- The actual *content* of `proto/criteria/v1/adapter_plugin.proto` beyond the service rename (no field/message changes — those go to v2 in WS02).

## Reviewer Notes

### Remediation 2026-05-15 (pass 2) — addressing remaining blocker

- **`internal/cli/plan.go`**: Changed human-readable plan header from `"plugins required:\n"` to `"adapters required:\n"`. This is Step 7 user-visible text — distinct from the machine-readable `plugins_required` JSON key (which was correctly preserved in pass 1).
- **Plan golden files** (`internal/cli/testdata/plan/*.golden`): Updated to assert `adapters required:` rather than `plugins required:`.
- `make ci` green; all tests pass.

### Remediation 2026-05-15 — addressing review blockers

1. **Prohibited file edits**: Reverted `README.md`, `CONTRIBUTING.md`, `architecture_archive/`, `docs/adrs/`, `docs/workflow.md`, `workstreams/adapter_v2/README.md`, and all `workstreams/archived/` files to `origin/main`. The only workstream-file change in this PR is the WS01 file itself.

2. **JSON contract preservation**: Reverted the `plugins_required` → `adapters_required` JSON key rename. The Go struct field is now `RequiredAdapters` (for clarity) but the JSON tag stays `json:"plugins_required"` to preserve machine consumers. Plan golden files reverted to `plugins_required`. Same for `"plugins required:"` in plan text output.

3. **gofmt**: Applied `gofmt -w` to all touched Go files. `make ci` (lint + vet + staticcheck + tests + race) is now green.

### Review 2026-05-14 — changes-requested

#### Summary
The mechanical rename is mostly in place: `internal/plugin/` became `internal/adapterhost/`, `AdapterPluginService` became `AdapterService`, `PluginName` became `AdapterName`, `sdk/pluginhost/` moved to `sdk/adapterhost/`, and the repo-level sanity greps are clean. I cannot approve this pass because the branch edits files that WS01 explicitly forbids touching, changes the machine-readable `criteria compile --format json` schema despite the workstream's "no behavior change" requirement, and does not satisfy the exit criteria because `make ci` currently fails on formatting/lint.

#### Plan Adherence
- Step 1: Implemented with the documented `internal/adapterhost` deviation; imports/callers were updated and `internal/plugin/` is gone.
- Step 2: Implemented; `proto/criteria/v1/adapter_plugin.proto` now declares `AdapterService` and generated stubs/call sites were updated.
- Step 3: Implemented; `PluginName` usages were renamed to `AdapterName`.
- Step 4: Implemented; `sdk/pluginhost/` moved to `sdk/adapterhost/`, and `sdk/adapterhost/doc.go` includes the required WS39 forward-pointer comment.
- Step 5: `docs/plugins.md` was renamed to `docs/adapters.md`, but the branch also edits prohibited documentation/workstream files outside the allowed set.
- Step 6/7: The terminology sweep is broadly complete, but `criteria compile --format json` now emits `adapters_required`, which exceeds the stated rename-only / no-behavior-change scope.
- Exit criteria: not met. `make ci` is failing, so the workstream is not ready to land.

#### Required Remediations
- **Blocker — prohibited file edits outside WS01 scope.** `README.md:L133-L135`, `CONTRIBUTING.md:L91`, and `workstreams/adapter_v2/README.md:L17`, `L109`, `L478`, `L612` were edited even though this workstream explicitly forbids touching README/CONTRIBUTING/other workstream files. `git diff --name-only origin/main...HEAD -- 'workstreams/**'` also shows many archived workstream files changed. **Acceptance:** revert every out-of-scope edit outside the file set allowed by WS01; the only workstream-file change permitted in this review pass is this file's appended reviewer notes.
- **Blocker — public CLI contract drift / behavior change.** `internal/cli/compile.go:L71-L84` renames the machine-readable JSON key from `plugins_required` to `adapters_required`, and the compile goldens under `internal/cli/testdata/compile/*.json.golden` were rewritten to accept the new schema. WS01 says behavior is unchanged and Step 7 only calls for help/error/log text updates. **Acceptance:** restore the existing JSON field name, or add a backwards-compatible representation that preserves current consumers; update tests to prove compatibility rather than only re-blessing the renamed field.
- **Blocker — exit criteria not met (`make ci`).** `make ci` currently fails in `lint-go` because multiple touched Go files are not gofmt'ed, including `internal/cli/compile.go`, `internal/adapterhost/discovery.go`, `cmd/criteria-adapter-copilot/copilot_session.go`, `cmd/criteria-adapter-mcp/bridge.go`, and others reported by golangci-lint. **Acceptance:** format every touched Go file and rerun `make ci` to green.

#### Test Intent Assessment
The rename coverage is broad, and the repo-level greps show that the old internal names are largely gone. The weak point is the CLI contract boundary: rewriting JSON golden files to `adapters_required` only proves the new output matches itself; it does not prove WS01 preserved behavior for existing machine consumers. The remediation needs an explicit compatibility assertion at that contract boundary, not just updated goldens.

#### Validation Performed
- `git diff --name-status origin/main...HEAD`
- `git diff --summary origin/main...HEAD`
- `rg -n --glob '*.go' 'internal/plugin' .` → no matches
- `rg -n -g '*.go' -g '*.proto' 'AdapterPluginService' .` → no matches
- `rg -n --glob '*.go' 'PluginName' .` → no matches
- `test -f docs/plugins.md` → absent
- `test -d internal/plugin` → absent
- `make ci` → failed in `lint-go`/gofmt on multiple touched Go files

### Review 2026-05-15 — changes-requested

#### Summary
This pass resolved the prior blockers around out-of-scope file edits, JSON schema drift, and failing repository validation: the branch is now scoped correctly, `make ci` is green, and the machine-readable compile output preserves `plugins_required`. I cannot approve yet because one user-visible CLI string still uses the old terminology, so Step 7 remains incomplete.

#### Plan Adherence
- Step 1: Implemented with the documented `internal/adapterhost` deviation; the host package/import rename is consistent and `internal/plugin/` is gone.
- Step 2: Implemented; `AdapterPluginService` was renamed to `AdapterService` and generated stubs/call sites were updated.
- Step 3: Implemented; `PluginName` was renamed to `AdapterName`.
- Step 4: Implemented; `sdk/pluginhost/` moved to `sdk/adapterhost/`, and the forward-pointer comment is present in `sdk/adapterhost/doc.go`.
- Step 5: Implemented; `docs/plugins.md` was renamed to `docs/adapters.md`, and the previously prohibited unrelated doc/workstream edits have been removed from the branch.
- Step 6: The sanity greps are clean.
- Step 7: Not fully implemented. `internal/cli/plan.go` still prints `plugins required:` in human-readable output.
- Exit criteria: not yet met because the workstream still leaves user-visible CLI terminology inconsistent with the stated acceptance bar.

#### Required Remediations
- **Blocker — remaining user-visible `plugin` terminology in CLI output.** `internal/cli/plan.go:L136-L142` still renders the section header as `plugins required:`, and the plan goldens such as `internal/cli/testdata/plan/hello__examples__hello.golden:L22-L23` still assert that old wording. WS01 Step 7 requires user-visible CLI text to say `adapter` everywhere. **Acceptance:** change the human-readable plan output heading to `adapters required:`, update the affected plan goldens, and keep the machine-readable compile JSON field as `plugins_required` for compatibility.

#### Test Intent Assessment
The current tests now correctly protect the machine-readable compile contract by keeping `plugins_required` in JSON output while validating the broader rename mechanically. The remaining gap is that the plan-output goldens still codify the old user-facing wording, so they currently prove the incomplete behavior rather than the intended terminology unification.

#### Validation Performed
- `git diff --name-status origin/main...HEAD` → no prohibited README / CONTRIBUTING / archived workstream edits remain on the branch
- `rg -n --glob '*.go' 'internal/plugin' .` → no matches
- `rg -n -g '*.go' -g '*.proto' 'AdapterPluginService' .` → no matches
- `rg -n --glob '*.go' 'PluginName' .` → no matches
- `! test -f docs/plugins.md` → passes
- `! test -d internal/plugin` → passes
- `rg -n -i 'plugin' internal/cli/*.go` / targeted CLI search → only remaining user-visible hit is `internal/cli/plan.go`
- `make ci` → passed

### Review 2026-05-15-02 — approved

#### Summary
Approved. The remaining Step 7 gap is fixed: the human-readable plan output now says `adapters required:` while the machine-readable compile JSON continues to preserve `plugins_required` for compatibility. The branch stays within WS01 scope, the rename sweep is clean, no baseline changes were introduced, and the full repository validation target passes.

#### Plan Adherence
- Step 1: Implemented with the documented `internal/adapterhost` deviation; `internal/plugin/` is removed and imports/callers are updated consistently.
- Step 2: Implemented; `AdapterPluginService` was renamed to `AdapterService` and regenerated bindings/call sites are aligned.
- Step 3: Implemented; `PluginName` was renamed to `AdapterName`.
- Step 4: Implemented; `sdk/pluginhost/` moved to `sdk/adapterhost/`, including the required WS39 forward-pointer comment in `sdk/adapterhost/doc.go`.
- Step 5: Implemented; `docs/plugins.md` was renamed to `docs/adapters.md`, with no remaining prohibited out-of-scope file edits on the branch.
- Step 6: Implemented; the workstream sanity greps are clean.
- Step 7: Implemented; `internal/cli/plan.go` and plan goldens now use `adapters required:` for user-visible output while compile JSON retains the existing compatibility key.
- Exit criteria: met.

#### Test Intent Assessment
The tests now validate the intended split between user-facing terminology and compatibility-sensitive machine output: plan goldens assert `adapters required:` for human-readable CLI text, while compile goldens continue to lock `plugins_required` at the JSON contract boundary. Combined with the repo-wide sanity greps and `make ci`, this is sufficient evidence for a rename-only change.

#### Validation Performed
- `git diff --name-only origin/main...HEAD -- README.md CONTRIBUTING.md workstreams/adapter_v2/README.md workstreams/archived docs/adrs docs/workflow.md architecture_archive` → no out-of-scope diffs
- `rg -n --glob '*.go' 'internal/plugin' .` → no matches
- `rg -n -g '*.go' -g '*.proto' 'AdapterPluginService' .` → no matches
- `rg -n --glob '*.go' 'PluginName' .` → no matches
- `! test -f docs/plugins.md` → passes
- `! test -d internal/plugin` → passes
- `rg -n -i 'plugin' internal/cli/*.go` → only preserved compatibility/environment-path references remain; no stale user-facing CLI wording
- `make ci` → passed
