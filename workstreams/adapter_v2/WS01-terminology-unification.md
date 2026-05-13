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
| Docs filename | `docs/plugins.md` | `docs/plugins.md` |

Users see "adapter" in HCL; developers wading into the host code see "plugin." That mixed vocabulary is friction for newcomers, hurts grep-ability, and obscures intent. The Adapter v2 plan (see `README.md` D6) standardizes on **adapter** everywhere.

This workstream is purely a rename. It is the first workstream in the phase because every other workstream touches code that gets renamed here; doing it first means no later workstream has to land its changes against soon-to-be-renamed files. Behavior is unchanged — `make ci` is the verification.

## Prerequisites

- `make ci` green on `main` (the branch this workstream lands against).
- No outstanding PRs touching `internal/plugin/`, `proto/criteria/v1/`, or `docs/plugins.md`.
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

### Step 5 — Rename `docs/plugins.md`

```sh
git mv docs/plugins.md docs/adapters.md
```

Update every cross-reference in the repo:

```sh
grep -rl "docs/plugins.md" --include='*.md' . | xargs sed -i.bak 's|docs/plugins.md|docs/adapters.md|g'
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
  ! test -f docs/plugins.md
  ! test -d internal/plugin
  ```

  All five must return exit code 1 (no matches / does not exist).

## Exit criteria

- `make ci` green.
- The five sanity greps above return no matches.
- `docs/adapters.md` exists and renders correctly.
- A single PR landed; CHANGELOG entry deferred to WS39 cleanup gate (with a forward-pointer comment in this PR's description).

## Files this workstream may modify

- Everything under `internal/plugin/` (which becomes `internal/adapter/`) and `sdk/pluginhost/` (which becomes `sdk/adapterhost/`).
- `proto/criteria/v1/adapter_plugin.proto` (service rename only).
- Generated proto Go stubs.
- Every file in the repo that imports the renamed packages or uses the renamed constants — mechanical edits only.
- `docs/plugins.md` → `docs/adapters.md`.
- Test files matching the rename pattern.

## Files this workstream may NOT edit

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`.
- `workstreams/README.md`.
- Any other workstream file in `workstreams/` (including this directory's other WS files).
- The actual *content* of `proto/criteria/v1/adapter_plugin.proto` beyond the service rename (no field/message changes — those go to v2 in WS02).
