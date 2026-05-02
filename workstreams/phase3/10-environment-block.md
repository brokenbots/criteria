# Workstream 10 — `environment "<type>" "<name>"` blocks (declaration surface only)

**Phase:** 3 · **Track:** B · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md). · **Unblocks:** [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md) (the new `adapter` block declares `environment = ...`), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md) (subworkflows can override environment).

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl) introduces `environment "<type>" "<name>" { variables = ..., config = ... }` as a typed environment declaration:

```hcl
environment "<type>" "<name>" {
    variables = map(string)         // env var injections
    config = map(any)               // type-specific config (shape determined by type)
}
```

The intent for Phase 3 is **declaration surface only** — the block is parsed, validated, stored on `FSMGraph`, and referenced by `adapter`/`step`/`subworkflow` blocks via `environment = <type>.<name>`. The **isolation runtime** (where an environment actually changes how an adapter is executed — sandboxing, container, restricted PATH, etc.) is the originally-planned Phase 3 theme, now **deferred to Phase 4** with a new contributor.

This workstream lays the slot the Phase 4 plug-architecture will fill. Without the slot, the rename in [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md) would have nowhere to attach the `environment` reference, and [13](13-subworkflow-block-and-resolver.md) would have no way for a subworkflow to declare its environment context.

For v0.3.0, the only **runtime** behavior an environment provides is **process-environment-variable injection**: when an adapter is invoked, the env vars from the bound environment's `variables` map are added to the adapter subprocess's environment. The `config` map is parsed and stored but **not wired** into adapter behavior — that's Phase 4. This is enough to make the surface useful (env-var injection covers a lot of use cases) without blocking on the isolation runtime.

## Prerequisites

- [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md) merged: `FoldExpr` available for compile-time fold of `variables`/`config` map keys.
- [08-schema-unification.md](08-schema-unification.md) merged: schema is consolidated.
- `make ci` green on `main`.

## In scope

### Step 1 — Schema

In [workflow/schema.go](../../workflow/schema.go) add `EnvironmentSpec` and `EnvironmentNode`:

```go
// EnvironmentSpec declares a typed execution environment.
// The HCL form has two labels: type then name.
//   environment "shell" "default" { variables = {...}, config = {...} }
type EnvironmentSpec struct {
    Type   string   `hcl:"type,label"`
    Name   string   `hcl:"name,label"`
    Remain hcl.Body `hcl:",remain"` // captures variables and config attributes
}

// EnvironmentNode is a compiled environment declaration.
type EnvironmentNode struct {
    Type      string
    Name      string
    Variables map[string]string  // resolved env vars (compile-folded)
    Config    map[string]cty.Value // type-specific config (compile-folded; shape unenforced for v0.3.0)
}
```

In `Spec`, add `Environments []EnvironmentSpec \`hcl:"environment,block"\`` between `Locals` and `Outputs`.

In `FSMGraph`, add:

```go
Environments map[string]*EnvironmentNode  // keyed by "<type>.<name>"
DefaultEnvironment string                 // optional; set if exactly one env is declared without a competing default flag (see Step 3)
```

### Step 2 — Compile environment blocks

New file `workflow/compile_environments.go`:

```go
// compileEnvironments folds and stores every environment block.
// Both variables and config maps must fold at compile (no runtime-only refs).
func compileEnvironments(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics
```

Validation:

1. `Type` must be one of the registered environment types. For v0.3.0 the only registered type is `shell`. Future types (`docker`, `firecracker`, etc.) are added in Phase 4.
2. `Name` must match `^[a-zA-Z][a-zA-Z0-9_-]*$`.
3. `<Type>.<Name>` must be unique across the spec. Duplicate is a compile error.
4. The `variables` attribute is optional; when present must fold to `cty.Map(cty.String)` (every value coerced to string via the existing `decodeAttrsToStringMap` semantics).
5. The `config` attribute is optional; when present must fold to a `cty.Object` or `cty.Map`. The shape is **not validated against a per-type schema in this workstream** — the schema lookup lands with the Phase 4 environment-plug abstraction. For v0.3.0 the config map is stored verbatim.

### Step 3 — Default environment resolution

A workflow can declare zero or more environments. Resolution rules for "which environment does an adapter/step/subworkflow use?":

1. Per-step `environment = <type>.<name>` attribute (highest precedence). Lands as part of [11-agent-to-adapter-rename.md](11-agent-to-adapter-rename.md) for adapter blocks and as part of [14-universal-step-target.md](14-universal-step-target.md) for step blocks. This workstream adds the **schema field** but does not add the resolution logic.
2. Per-adapter `environment = <type>.<name>` attribute on the `adapter` block ([11](11-agent-to-adapter-rename.md)).
3. Workflow-level default. If the workflow has exactly one environment block, that is the default. If multiple, the workflow MUST declare which is default via `workflow { default_environment = <type>.<name> }`. For v0.3.0 the workflow header doesn't have this attribute (workflow header schema is per [proposed_hcl.hcl](../../proposed_hcl.hcl) just `name` / `version` / `file` / `environment`). **Decision:** the workflow-level `environment = <type>.<name>` attribute on the workflow header serves both as the explicit default declaration AND a single-source-of-truth.

   Add to `Spec`:
   ```go
   DefaultEnvironment string `hcl:"environment,optional"`  // "<type>.<name>"
   ```

   The compile error "ambiguous default environment" fires when:
   - Multiple environment blocks declared.
   - `Spec.DefaultEnvironment` is empty.
   - At least one adapter/step/subworkflow consumer does not bind `environment` explicitly.

4. If no environment blocks are declared and no consumer references one, the engine runs adapters with **no environment-injected variables and no config** — current v0.2.0 behavior. The shell adapter still works exactly as it does today.

### Step 4 — Engine consumes environment for env-var injection (only)

In [internal/plugin/loader.go](../../internal/plugin/loader.go), the adapter subprocess invocation site (around `exec.Command(path)`) currently passes a sanitized PATH and a controlled env-var allowlist (per Phase 1 W05 sandboxing).

Add: when the adapter has a bound environment (resolved per Step 3), inject the environment's `Variables` map into the subprocess's env. Conflict resolution:

- The adapter's existing controlled set wins for security-critical vars (PATH, HOME, etc.).
- Environment-declared vars are added to the safe-to-inject set.
- A duplicate key in environment.variables and the existing controlled set produces a compile-time **warning** (not an error) — the controlled set wins at runtime. Document the warning in the adapter's bound environment.

The `config` map is **not** consumed at runtime in v0.3.0 — only the `variables` map. Document this clearly in code comments and reviewer notes; the Phase 4 contributor will add `config` consumption.

### Step 5 — Examples

- New example [examples/phase3-environment/](../../examples/phase3-environment/) demonstrating:
  - One `environment "shell" "ci"` block with `variables = { CI = "true", LOG_LEVEL = "debug" }`.
  - A workflow header that sets `environment = shell.ci`.
  - An adapter step whose execution observes the injected variables (e.g. a shell step that prints `$CI`).

### Step 6 — Tests

- `workflow/compile_environments_test.go`:
  - `TestCompileEnvironments_Single` — one env block compiles.
  - `TestCompileEnvironments_DuplicateTypeAndName` — error.
  - `TestCompileEnvironments_UnknownType` — `environment "docker" "x"` errors with "unknown environment type" pointing to the future Phase 4 mention.
  - `TestCompileEnvironments_VariablesFold` — `variables = { X = var.x }` folds at compile.
  - `TestCompileEnvironments_ConfigFold` — `config = { foo = var.foo }` folds at compile.
  - `TestCompileEnvironments_VariablesRuntimeRef` — `variables = { X = each.value }` errors at compile (env vars must fold).
  - `TestCompileEnvironments_DefaultMultipleNoDefault` — multiple envs, no `Spec.DefaultEnvironment`, at least one consumer un-bound → error.

- `internal/plugin/loader_test.go`:
  - `TestLoaderInjectsEnvironmentVars` — adapter subprocess sees the injected vars.
  - `TestLoaderControlledSetWinsConflict` — env's `PATH = "/foo"` is overridden by the controlled PATH; warning emitted at compile.

- End-to-end: [examples/phase3-environment/](../../examples/phase3-environment/) runs and the adapter observes injected vars.

### Step 7 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/... ./internal/plugin/... ./internal/engine/...
make validate
make lint-go
make lint-baseline-check
make ci
```

All exit 0.

## Behavior change

**Behavior change: yes — additive.**

Observable differences:

1. New top-level block `environment "<type>" "<name>" { variables = ..., config = ... }` parses. For v0.3.0 only `<type> = "shell"` is recognized.
2. Workflow header gains an optional `environment = <type>.<name>` attribute that names the default environment.
3. When a workflow declares an environment and an adapter step runs under that environment, the subprocess receives the declared `variables` as env vars (subject to the controlled-set conflict policy in Step 4).
4. The `config` map is parsed but does not change adapter behavior in v0.3.0. This is documented as a Phase 4 plug-point.

Migration: workflows without `environment` blocks behave exactly as v0.2.0. The new surface is opt-in.

No proto change. No SDK change. No CLI flag change.

## Reuse

- `FoldExpr` ([07](07-local-block-and-fold-pass.md)).
- Existing PATH-sanitization and controlled env-var set in [internal/adapters/shell/sandbox.go](../../internal/adapters/shell/sandbox.go) and [internal/plugin/loader.go](../../internal/plugin/loader.go).
- Existing variable-type parsing for `cty.Map(cty.String)` shape coercion.
- Existing schema-decode patterns from `compile_agents.go`.

## Out of scope

- Per-type config schema enforcement. Phase 4 adds `EnvironmentTypeRegistry` so each registered type can declare its `config` shape; for v0.3.0 the config is stored as opaque `map[string]cty.Value`.
- Isolation runtime (sandbox-exec, seccomp, Docker, etc.). Phase 4 plug architecture.
- Adapter-block `environment = ...` attribute. Owned by [11](11-agent-to-adapter-rename.md).
- Step-block `environment = ...` attribute. Owned by [14-universal-step-target.md](14-universal-step-target.md).
- Subworkflow-block `environment = ...` attribute. Owned by [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md).
- Per-environment lifecycle hooks (open, close). Phase 4.
- Environment inheritance from parent → child workflow. Each scope binds its own; no implicit inheritance.

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — `EnvironmentSpec`, `EnvironmentNode`, `Spec.Environments`, `Spec.DefaultEnvironment`, `FSMGraph.Environments`, `FSMGraph.DefaultEnvironment`.
- New: `workflow/compile_environments.go`.
- The top-level compile entry — invoke `compileEnvironments` after `compileLocals` and before `compileAgents`.
- [`internal/plugin/loader.go`](../../internal/plugin/loader.go) — env-var injection at the subprocess invocation site.
- [`internal/engine/`](../../internal/engine/) — environment resolution lookup (`resolveEnvironment(g, stepName)` style helper).
- New: [`examples/phase3-environment/`](../../examples/) and supporting fixtures.
- New: tests under [`workflow/`](../../workflow/) and [`internal/plugin/`](../../internal/plugin/).
- [`docs/workflow.md`](../../docs/workflow.md) — add an "Environments" section describing the v0.3.0 surface and the Phase 4 forward-pointer.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- `.proto` files. No wire change.
- The `agent` block — owned by [11](11-agent-to-adapter-rename.md).
- The `step` block schema beyond what's needed to link to environments (the `environment = ...` attribute lands in [14](14-universal-step-target.md)).

## Tasks

- [ ] Add `EnvironmentSpec`, `EnvironmentNode`, `Spec.Environments`, `Spec.DefaultEnvironment` to schema (Step 1).
- [ ] Implement `compileEnvironments` (Step 2).
- [ ] Implement default-environment resolution rules (Step 3).
- [ ] Wire env-var injection into the adapter subprocess invocation (Step 4).
- [ ] Add `examples/phase3-environment/` (Step 5).
- [ ] Author all required tests (Step 6).
- [ ] Update [`docs/workflow.md`](../../docs/workflow.md) with the new section (Step 6 implicit; explicit in this workstream's scope).
- [ ] `make ci` green; `make validate` green for every example.

## Exit criteria

- `environment "shell" "<name>" { variables = {...}, config = {...} }` parses and compiles.
- Unknown types, duplicate names, runtime-ref values produce compile errors.
- Workflow header `environment = <type>.<name>` is accepted and validated.
- Adapter subprocesses receive injected env vars at runtime.
- Controlled-set conflict produces a compile warning, not an error.
- `examples/phase3-environment/` runs end-to-end.
- All required tests pass.
- `make ci` exits 0.

## Tests

The Step 6 test list is the deliverable. Coverage targets:

- `workflow/compile_environments.go` ≥ 90% line coverage.
- The plugin loader env-var injection branch ≥ 80% (the existing branch is 100%-ish; this just adds one path).

## Risks

| Risk | Mitigation |
|---|---|
| The `config` map being parsed but not consumed creates a "looks supported but isn't" surface | Document in [docs/workflow.md](../../docs/workflow.md) explicitly that v0.3.0 `config` is reserved for Phase 4. The HCL surface is forward-compatible: a v0.3.0 user setting `config = { sandbox_profile = "strict" }` will simply have the value ignored at runtime, with a one-time concise-mode info log on workflow start ("environment X has config keys not yet consumed"). |
| Env-var injection conflicts with the existing shell-sandbox env allowlist in unexpected ways | The conflict policy in Step 4 is conservative (controlled set wins). Add `TestLoaderControlledSetWinsConflict` to lock it in. Future Phase 4 work can broaden the allowlist via the environment type registry. |
| The default-environment resolution produces ambiguous errors when a workflow has no consumers | If no consumer references an environment, no resolution is needed; the multiple-environments-no-default case is silent. The error fires only when there's an actual ambiguous binding. Test `TestCompileEnvironments_DefaultMultipleNoDefault` covers the case. |
| The "shell" type being the only one in v0.3.0 is too restrictive | It is restrictive on purpose. Phase 4 adds the registry. The HCL surface accepts the type as a label so future types compile without schema change. |
| Tests for env-var injection are flaky on macOS due to PATH inheritance differences | Use a hermetic test fixture: a small Go test binary that prints its env, invoked as the "adapter" subprocess. Do not rely on system shell behavior. |
