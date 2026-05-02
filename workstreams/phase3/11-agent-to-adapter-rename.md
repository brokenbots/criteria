# Workstream 11 — Hard rename `agent` → `adapter "<type>" "<name>"`

**Phase:** 3 · **Track:** C (language surface) · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md), [10-environment-block.md](10-environment-block.md). · **Unblocks:** [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md), [14-universal-step-target.md](14-universal-step-target.md).

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl) renames the top-level `agent "name" {}` block to `adapter "<type>" "<name>" {}`. The new shape:

```hcl
adapter "copilot" "reviewer" {
    environment = shell.ci          // optional; from [10]
    config = {
        reasoning_effort = "high"
    }
}

adapter "shell" "default" {
    config = {}
}
```

Two structural changes versus the legacy `agent` form:

1. **Two labels instead of one.** The first label is the **adapter type** (matching the `--adapter <type>` registration the engine already uses internally). The second is the **instance name** referenced from steps. This makes the semantic explicit: a workflow can declare multiple instances of the same adapter type with different configs.
2. **`environment = <type>.<name>` attribute** linking to the [10-environment-block.md](10-environment-block.md) declaration.

**Hard rename. No alias. No deprecation cycle.** The user explicitly required a clean break from v0.2.0:

- The legacy `agent` block becomes a **parse error** with a hard, descriptive message pointing at the migration note.
- All internal types rename (`AgentSpec` → `AdapterDeclSpec`, `AgentNode` → `AdapterNode` — with care that the existing `AdapterInfo` type stays distinct; see Step 1).
- All examples, docs, error messages, event payload field names, and SDK references rename.

[architecture_notes.md §6](../../architecture_notes.md) calls this out: "Adapters are initialized automatically when their defining workflow scope begins". That automation lands in [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md). This workstream is just the **rename + the new shape**.

## Prerequisites

- [07](07-local-block-and-fold-pass.md), [08](08-schema-unification.md), [10](10-environment-block.md) merged.
- `make ci` green on `main`.

## In scope

### Step 1 — Schema rename and reshape

In [workflow/schema.go](../../workflow/schema.go):

```go
// BEFORE
type AgentSpec struct {
    Name    string      `hcl:"name,label"`
    Adapter string      `hcl:"adapter"`
    OnCrash string      `hcl:"on_crash,optional"`
    Config  *ConfigSpec `hcl:"config,block"`
}

// AFTER
type AdapterDeclSpec struct {
    Type        string      `hcl:"type,label"`           // first label
    Name        string      `hcl:"name,label"`           // second label
    Environment string      `hcl:"environment,optional"` // "<env_type>.<env_name>" reference
    OnCrash     string      `hcl:"on_crash,optional"`
    Config      *ConfigSpec `hcl:"config,block"`
}
```

Why `AdapterDeclSpec` (not `AdapterSpec`)? Because [`AdapterInfo`](../../workflow/schema.go#L151) already exists for the adapter-side schema description. Using `AdapterSpec` for the HCL block would collide semantically. Use `AdapterDeclSpec` ("declaration spec") for the HCL block and `AdapterNode` for the compiled form. Document the naming choice in code comments.

```go
type AdapterNode struct {
    Type        string
    Name        string
    Environment string             // resolved to "<env_type>.<env_name>"; empty if not set; engine resolves to default at scope start
    OnCrash     string
    Config      map[string]string  // compile-folded config
}
```

In `Spec`, rename `Agents []AgentSpec` to `Adapters []AdapterDeclSpec` with the HCL tag `\`hcl:"adapter,block"\``.

In `FSMGraph`, rename `Agents map[string]*AgentNode` to `Adapters map[string]*AdapterNode`. **Key format:** `"<type>.<name>"` (matches the reference syntax used in steps and subworkflows). This is a different key shape than the legacy single-name keying — every consumer must update.

### Step 2 — Step-block field rename

`StepSpec.Agent string \`hcl:"agent,optional"\`` becomes:

```go
StepSpec.Adapter string `hcl:"adapter,optional"` // "<type>.<name>" reference to a declared adapter
```

Wait — there's a name collision. The legacy `StepSpec.Adapter string \`hcl:"adapter,optional"\`` field already exists at [workflow/schema.go:75](../../workflow/schema.go#L75) and meant "use this adapter type directly with default config (no named adapter binding)". After the rename:

- The legacy direct-adapter form (`adapter = "shell"` on a step) is **removed**. It conflated type and instance and made adapter lifecycle implicit.
- All steps that need adapter execution must reference a declared adapter via `adapter = <type>.<name>`.
- `StepSpec.Adapter string` keeps the field name but changes semantics — it is now ALWAYS a reference to a declared adapter, never a bare type.

This is a breaking change for HCL authors of workflows that use `adapter = "shell"` directly on a step. Migration: declare a named adapter at top level and reference it.

In [14-universal-step-target.md](14-universal-step-target.md), the universal `target` attribute replaces `adapter`/`agent` entirely. This workstream's intermediate step keeps `StepSpec.Adapter` as a stopgap until [14](14-universal-step-target.md) lands.

Delete `StepSpec.Agent` field — it merges into `StepSpec.Adapter`.

### Step 3 — Compile rename

Rename [workflow/compile_agents.go](../../workflow/compile_agents.go) → `workflow/compile_adapters.go`. Rename:

- `compileAgents` → `compileAdapters`.
- `agentConfigEvalContext` → `adapterConfigEvalContext`.
- All log/event/diagnostic strings: `"agent ..."` → `"adapter ..."`.
- The duplicate-detection key changes from `name` to `<type>.<name>`.

Add: validation that `Environment` (when set) matches a declared environment in `g.Environments`. Use [10-environment-block.md](10-environment-block.md)'s map keying: `<env_type>.<env_name>`. Missing environment is a compile error.

### Step 4 — Engine consumer rename

Find every reference in [internal/engine/](../../internal/engine/), [internal/plugin/](../../internal/plugin/), [internal/cli/](../../internal/cli/), [internal/run/](../../internal/run/), [cmd/](../../cmd/) to:

- `*AgentNode` → `*AdapterNode`.
- `g.Agents` → `g.Adapters`.
- Field accesses on the struct: `.Adapter` (which used to be the type) → `.Type` (now the type label).

Use `gopls`/IDE rename for type-level changes; for the runtime field access changes, do a careful manual sweep — there's no automated way to know that `node.Adapter` → `node.Type` until you read the rename intent.

### Step 5 — Hard parse error for legacy `agent` block

In the HCL decode path, before passing the body to `gohcl.DecodeBody`:

```go
// rejectLegacyBlocks emits a hard error for blocks whose names were renamed
// in v0.3.0. The error message names the new form and points at the
// migration note.
func rejectLegacyBlocks(body hcl.Body) hcl.Diagnostics {
    legacyBlockNames := map[string]string{
        "agent":  `the "agent" block was renamed to "adapter" in v0.3.0; declare adapter "<type>" "<name>" { ... } and remove the legacy agent block. See CHANGELOG.md migration note.`,
        // [15] adds: "branch": ...
        // [16] adds: "transition_to": (attribute, not block — handled separately)
    }
    var diags hcl.Diagnostics
    schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{}}
    for name := range legacyBlockNames {
        schema.Blocks = append(schema.Blocks, hcl.BlockHeaderSchema{Type: name, LabelNames: nil})
    }
    content, _, _ := body.PartialContent(schema)
    for _, block := range content.Blocks {
        if msg, ok := legacyBlockNames[block.Type]; ok {
            diags = append(diags, &hcl.Diagnostic{
                Severity: hcl.DiagError,
                Summary:  fmt.Sprintf("removed block %q", block.Type),
                Detail:   msg,
                Subject:  &block.DefRange,
            })
        }
    }
    return diags
}
```

Call `rejectLegacyBlocks` at the top of the `Spec` decode path, before `gohcl.DecodeBody`. The error must point at the source range of the legacy block.

The legacy `StepSpec.Agent` field similarly rejects: when an HCL `agent = "..."` attribute appears on a step block, emit a hard parse error with the same migration message.

### Step 6 — Migration note (deferred to E1, but seeded here)

Reviewer notes for this workstream record the **exact migration text** for [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md) to paste into [CHANGELOG.md](../../CHANGELOG.md):

```
### `agent` block → `adapter "<type>" "<name>"` block

v0.2.0 form:
    agent "reviewer" {
        adapter = "copilot"
        config { reasoning_effort = "high" }
    }

v0.3.0 form:
    adapter "copilot" "reviewer" {
        config = {
            reasoning_effort = "high"
        }
    }

Steps that referenced `agent = "reviewer"` now reference `adapter = copilot.reviewer`.
Steps that used `adapter = "shell"` (bare type) must declare a named adapter:
    adapter "shell" "default" { config = {} }
and reference it: `adapter = shell.default`.
```

### Step 7 — Update examples and docs

Rename every example HCL under [examples/](../../examples/) to use the new shape. Regenerate goldens.

Rewrite [docs/workflow.md](../../docs/workflow.md) sections that document the agent block. The doc file is editable in this workstream (it's not in the coordination set).

[docs/runtime/](../../docs/runtime/) and any adapter-author docs that reference "agents" rename to "adapters" consistently.

### Step 8 — Update events and proto

Search for event field names mentioning `agent`. The wire envelope in [proto/criteria/v1/](../../proto/criteria/v1/) likely has a `string agent_name` field somewhere; rename to `adapter_name` (or keep the proto field name and note the v0.2.0 → v0.3.0 mapping in the SDK CHANGELOG — proto wire compatibility prefers stable names). **Decision:** since the user mandated a clean break, rename the proto field.

- Bump the proto field name. The field number stays. Field-number stability + name change is wire-compatible (protobuf serializes by number); the rename only affects generated code. Update [sdk/CHANGELOG.md](../../sdk/CHANGELOG.md).
- Run `make proto` to regenerate. Confirm `make proto-check-drift` exits 0 after regeneration.

### Step 9 — Tests

- `workflow/compile_adapters_test.go`:
  - `TestCompileAdapters_BasicShape`.
  - `TestCompileAdapters_DuplicateTypeAndName`.
  - `TestCompileAdapters_EnvironmentReference_Resolves`.
  - `TestCompileAdapters_EnvironmentReference_Missing` — error.
  - `TestCompileAdapters_BareEnvironmentMissing_DefaultResolves` — when adapter has no `environment = ...` and the workflow has a default environment, the adapter binds to it.

- Decode rejection:
  - `TestDecode_LegacyAgentBlock_HardError` — HCL with `agent "x" { ... }` produces a parse error with the documented message and source range.
  - `TestDecode_LegacyStepAgentAttr_HardError` — HCL with `step "x" { agent = "y" }` produces a parse error.
  - `TestDecode_LegacyStepBareAdapter_HardError` — HCL with `step "x" { adapter = "shell" }` (no dot) produces an error directing the user to declare a named adapter.

- Engine:
  - `TestEngine_AdapterRoutingByDottedName` — step `adapter = copilot.reviewer` routes to the right declaration.

- Migration smoke: every example in [examples/](../../examples/) compiles and runs.

### Step 10 — Validation

```sh
go build ./...
go test -race -count=2 ./workflow/... ./internal/engine/... ./internal/cli/... ./internal/plugin/... ./cmd/... ./sdk/...
make validate
make proto-check-drift
make test-conformance
make lint-go
make lint-baseline-check
make ci
git grep -nE '\bAgentSpec\b|\bAgentNode\b|"agent,block"|hcl:"agent,optional"' -- ':!*_test.go' ':!CHANGELOG.md' ':!docs/'
```

The final `git grep` MUST return zero matches in production code. The CHANGELOG and docs reference the legacy term in migration context — those are allowed.

## Behavior change

**Behavior change: yes — breaking for HCL authors.**

Observable differences:

1. `agent "x" { ... }` is a hard parse error. Migration is to `adapter "<type>" "x" { ... }`.
2. `step "y" { agent = "x" }` is a hard parse error.
3. `step "y" { adapter = "shell" }` (bare type without dot) is a hard parse error — must reference a declared adapter.
4. The wire envelope's `agent_name` field renames to `adapter_name` (number stable, name new).
5. Logs, diagnostics, and CLI output use the term "adapter" everywhere; no occurrences of "agent" in user-facing strings.

Migration note enumerated for [21](21-phase3-cleanup-gate.md).

## Reuse

- Existing [`compile_agents.go`](../../workflow/compile_agents.go) compile flow — rename in place, do not rewrite.
- [`adapterConfigEvalContext`](../../workflow/compile_agents.go#L22) — rename only.
- Existing `AdapterInfo` (the schema description) — distinct from the new `AdapterNode`; document the distinction in code.
- `FoldExpr` from [07](07-local-block-and-fold-pass.md) — for adapter config attribute compile-folding.
- `gopls` / IDE rename for symbol-level changes.

## Out of scope

- Adapter lifecycle automation (auto-init at scope start, auto-teardown at terminal). Owned by [12](12-adapter-lifecycle-automation.md).
- Removing `lifecycle = "open"|"close"` from steps. Owned by [12](12-adapter-lifecycle-automation.md).
- The universal step `target` attribute. Owned by [14](14-universal-step-target.md). This workstream's `StepSpec.Adapter` keeps its current shape (a dotted `<type>.<name>` reference) until [14](14-universal-step-target.md) replaces it.
- `subworkflow` block. Owned by [13](13-subworkflow-block-and-resolver.md).

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — rename `AgentSpec` → `AdapterDeclSpec`, `AgentNode` → `AdapterNode`, `Spec.Agents` → `Spec.Adapters`, `FSMGraph.Agents` → `FSMGraph.Adapters`. Delete `StepSpec.Agent`. Reshape `StepSpec.Adapter` (semantic only).
- Rename: [`workflow/compile_agents.go`](../../workflow/compile_agents.go) → `workflow/compile_adapters.go`. Rename functions and identifiers within.
- New (or extracted) decode-time rejection helper file `workflow/parse_legacy_reject.go`.
- Every callsite under [`internal/engine/`](../../internal/engine/), [`internal/plugin/`](../../internal/plugin/), [`internal/cli/`](../../internal/cli/), [`internal/run/`](../../internal/run/), [`cmd/`](../../cmd/) that references the old types.
- [`proto/criteria/v1/`](../../proto/criteria/v1/) — rename `agent_name` → `adapter_name` in any envelope. Field numbers stable.
- [`sdk/CHANGELOG.md`](../../sdk/CHANGELOG.md) — additive entry per Step 8.
- Every example HCL under [`examples/`](../../examples/).
- Goldens under [`internal/cli/testdata/compile/`](../../internal/cli/testdata/compile/) and [`internal/cli/testdata/plan/`](../../internal/cli/testdata/plan/).
- [`docs/workflow.md`](../../docs/workflow.md) and any adapter-related docs in [`docs/`](../../docs/).
- All test files needing the rename (production-side type changes propagate to tests).
- New migration tests per Step 9.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — no new entries.

## Tasks

- [ ] Rename schema (Step 1).
- [ ] Reshape step-block adapter reference (Step 2).
- [ ] Rename compile flow (Step 3).
- [ ] Rename engine/plugin/cli consumers (Step 4).
- [ ] Add hard parse error for legacy blocks (Step 5).
- [ ] Record exact migration text in reviewer notes (Step 6).
- [ ] Update examples, docs, goldens (Step 7).
- [ ] Rename proto field; bump SDK changelog (Step 8).
- [ ] Author all tests in Step 9.
- [ ] `make ci`, `make proto-check-drift`, `make test-conformance` green; `git grep` for legacy types in production code returns zero (Step 10).

## Exit criteria

- `git grep -E '\bAgentSpec\b|\bAgentNode\b'` returns zero in production code.
- `git grep '"agent,block"'` returns zero in production code.
- `git grep 'hcl:"agent,optional"'` returns zero in production code.
- `agent "x"` HCL block produces a hard parse error.
- `step "x" { agent = ... }` produces a hard parse error.
- `adapter "<type>" "<name>"` block parses, compiles, and is referenced by `adapter = <type>.<name>` from steps.
- Adapter `environment = <env_type>.<env_name>` references resolve at compile.
- Wire envelopes use `adapter_name` field; SDK changelog bumped.
- Every example renamed; `make validate` green.
- Goldens regenerated.
- Migration text recorded in reviewer notes for [21](21-phase3-cleanup-gate.md).
- `make ci` exits 0.

## Tests

The Step 9 list is the deliverable. Coverage targets:

- `workflow/compile_adapters.go` ≥ 90%.
- The `rejectLegacyBlocks` path ≥ 95% (it's a small, decision-heavy function).

## Risks

| Risk | Mitigation |
|---|---|
| The rename touches ~30+ files and a missed callsite breaks at link or runtime | Run `git grep` for every legacy identifier (Step 10 has the list). Use `gopls` rename when possible; manually verify the result. |
| Renaming the proto field breaks orchestrators that read `agent_name` from the wire | Field numbers stable means the wire format is unchanged. Orchestrators reading by field name see a name change in their generated code (recompile). Document in SDK CHANGELOG. |
| `StepSpec.Adapter` semantic flip (now requires dot) breaks every existing example | This is the documented breaking change. Sweep examples in Step 7. |
| `rejectLegacyBlocks` has false positives if a workflow's `name` happens to be "agent" | Block-name matching is HCL block-type matching, not attribute-value matching. A `step "agent"` (instance named "agent") is fine; a top-level `agent "x" { ... }` block is the rejected case. Test `TestDecode_StepNamedAgent_NotRejected` to lock this in. |
| The unified key format `<type>.<name>` collides with other usages (e.g. `var.<name>`) | The dotted key is a runtime/storage convention, not an HCL syntax. The HCL `adapter = copilot.reviewer` reference parses as a traversal; the engine resolves it to the dotted key for storage. No collision. |
| Tests that use the legacy types fail to compile after the rename | Update the tests in this workstream — they are not in scope's "may not edit" set. The signal is that all renamed-tests still verify the same behavior, just under the new names. |
