# Workstream 11 — Hard rename `agent` → `adapter "<type>" "<name>"`

**Phase:** 3 · **Track:** C (language surface) · **Owner:** Workstream executor · **Depends on:** [07-local-block-and-fold-pass.md](07-local-block-and-fold-pass.md), [08-schema-unification.md](08-schema-unification.md), [10-environment-block.md](10-environment-block.md). · **Unblocks:** [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md), [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md), [14-universal-step-target.md](14-universal-step-target.md).

## Status

**Completed:**
- Schema rename: `AgentSpec` → `AdapterDeclSpec`, `AgentNode` → `AdapterNode`, `Spec.Agents` → `Spec.Adapters`, `FSMGraph.Adapters` keyed by `"<type>.<name>"`. `StepSpec.Agent` deleted.
- Compile rename: [`workflow/compile_agents.go`](../../workflow/compile_agents.go) → [`workflow/compile_adapters.go`](../../workflow/compile_adapters.go). Environment-reference validation added.
- Engine consumer rename: every callsite under [`internal/engine/`](../../internal/engine/), [`internal/plugin/`](../../internal/plugin/), [`internal/cli/`](../../internal/cli/), [`internal/run/`](../../internal/run/), [`cmd/`](../../cmd/) updated to use `AdapterNode` / `g.Adapters`.
- Hard parse errors: legacy `agent` block, legacy `step { agent = "..." }` attribute, and bare `step { adapter = "<type>" }` (single-segment) all rejected with migration messages.
- CLI output: `criteria plan` and `criteria compile --format json` both use `adapter` / `adapters` terminology end-to-end.
- Examples and goldens migrated; `make validate` green.
- Test suite green (`make ci` exits 0).

**Outstanding (must be completed before merge):**
- **`step.adapter` reference must be an HCL traversal expression**, not a quoted string. See [Step 2](#step-2--step-block-adapter-reference-is-an-hcl-traversal). All examples, testdata, and goldens currently encode the reference as a quoted string and must be re-authored to bareword traversal form.
- `WithAutoBootstrapAdapters()` and the `autoBootstrapAdapters: true` default in [`internal/engine/engine.go`](../../internal/engine/engine.go) must be removed or relocated. Adapter session lifecycle automation is owned by [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md); this workstream must not ship runtime auto-init in production paths. Test-only opt-in is acceptable if it lives in a test helper, not in the production constructor.

## Context

[proposed_hcl.hcl](../../proposed_hcl.hcl) renames the top-level `agent "name" {}` block to `adapter "<type>" "<name>" {}`. The new shape:

```hcl
adapter "copilot" "reviewer" {
    environment = "shell.ci"
    config = {
        reasoning_effort = "high"
    }
}

adapter "shell" "default" {
    config = {}
}

step "review" {
    adapter = adapter.copilot.reviewer       # bareword traversal — see Step 2
    input { task_id = each.value }
    outcome "success" { transition_to = "done" }
}
```

Two structural changes versus the legacy `agent` form:

1. **Two labels instead of one.** First label is the **adapter type** (matching the `--adapter <type>` registration the engine uses internally). Second is the **instance name** referenced from steps. A workflow can declare multiple instances of the same adapter type with different configs.
2. **`environment` attribute** binds the adapter to a declared environment from [10-environment-block.md](10-environment-block.md).

This is a hard rename. No alias. No deprecation cycle. The legacy `agent` block, the legacy `step { agent = "..." }` attribute, and the legacy single-segment `step { adapter = "<type>" }` form all become hard parse errors with migration messages. All internal types, examples, docs, error messages, event payloads, and SDK references rename together.

Adapter session lifecycle automation (auto-init at scope start, auto-teardown at terminal) is owned by [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md). This workstream is the **rename + the new shape only**.

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

The new type is `AdapterDeclSpec` — not `AdapterSpec` — because [`AdapterInfo`](../../workflow/schema.go#L151) already exists for the adapter-side schema description. `AdapterDeclSpec` ("declaration spec") is the HCL block; `AdapterNode` is the compiled form. Document the naming choice in code comments.

```go
type AdapterNode struct {
    Type        string
    Name        string
    Environment string             // resolved to "<env_type>.<env_name>"; empty if not set; engine resolves to default at scope start
    OnCrash     string
    Config      map[string]string  // compile-folded config
}
```

In `Spec`, rename `Agents []AgentSpec` to `Adapters []AdapterDeclSpec` with HCL tag `` `hcl:"adapter,block"` ``.

In `FSMGraph`, rename `Agents map[string]*AgentNode` to `Adapters map[string]*AdapterNode`. **Key format:** `"<type>.<name>"` (matches the runtime storage convention and what the dotted reference resolves to). Every consumer must update.

### Step 2 — Step-block adapter reference is an HCL traversal

`StepSpec.Adapter` must capture an **HCL traversal expression**, not a string. The author writes:

```hcl
step "review" {
    adapter = adapter.copilot.reviewer
}
```

— a bareword three-segment traversal (`adapter` namespace, type label, instance label). The compiler resolves the traversal to the dotted runtime key `"copilot.reviewer"` and stores it on `StepNode.Adapter`.

**Requirements (non-negotiable):**

1. **Author syntax is a traversal.** `adapter = "copilot.reviewer"` (quoted string) is a hard parse error. The error message must direct the user to the traversal form.
2. **Three segments, in order:** `adapter` (literal namespace), `<type>`, `<name>`. Anything else (single segment, two segments, four segments, function call, indexing) is a hard parse error.
3. **Unknown adapter** (a syntactically valid traversal that does not resolve against `g.Adapters`) is a compile error pointing at the source range.
4. **Storage shape unchanged.** `StepNode.Adapter` remains `string` holding `"<type>.<name>"`. Engine code that consumes `StepNode.Adapter` does not change.

**Why a traversal, not a string:**

- [14-universal-step-target.md](14-universal-step-target.md) replaces `adapter = ...` with the universal `target = ...` attribute; that workstream's design uses `target = adapter.copilot.reviewer` (traversal) and explicitly reuses the traversal-resolution helper this workstream delivers. Shipping `step.adapter` as a string forces a second breaking migration on every workflow author when [14](14-universal-step-target.md) lands.
- Bareword traversals are how every other named reference in the surface works: `var.<name>`, `local.<name>`, `each.value`, and the future `step.<name>.output.<key>`. The dotted adapter reference must follow the same rule for the surface to stay consistent.
- The compiler can produce precise traversal-resolution diagnostics (typo detection, source-range pointers) automatically. The string form requires reimplementing all of that.

**Schema implementation:**

`gohcl` does not decode `hcl.Expression` into a struct field directly. Use the `Remain hcl.Body` pattern that [`StepSpec`](../../workflow/schema.go) already uses for `for_each` and `count`: pull `adapter` out of `Remain.JustAttributes()` after the gohcl decode, then resolve the expression's traversal.

```go
// StepSpec — note: Adapter is no longer a hcl-decoded field; it is pulled
// from Remain by the compiler. The Agent field is deleted.
type StepSpec struct {
    Name      string `hcl:"name,label"`
    Lifecycle string `hcl:"lifecycle,optional"`
    OnCrash   string `hcl:"on_crash,optional"`
    Type      string `hcl:"type,optional"`
    // ... remaining gohcl-decoded fields ...
    Remain    hcl.Body `hcl:",remain"` // captures `adapter`, `for_each`, `count`, ...
}
```

The compile path:

```go
func resolveStepAdapterRef(body hcl.Body) (typeName, instanceName string, present bool, diags hcl.Diagnostics) {
    attrs, _ := body.JustAttributes()
    attr, ok := attrs["adapter"]
    if !ok {
        return "", "", false, nil
    }
    trav, traversalDiags := hcl.AbsTraversalForExpr(attr.Expr)
    diags = append(diags, traversalDiags...)
    if traversalDiags.HasErrors() {
        return "", "", true, diags
    }
    // Validate shape: adapter.<type>.<name>
    if len(trav) != 3 || trav.RootName() != "adapter" {
        diags = append(diags, &hcl.Diagnostic{
            Severity: hcl.DiagError,
            Summary:  "invalid adapter reference",
            Detail:   `adapter reference must take the form adapter.<type>.<name>`,
            Subject:  attr.Expr.Range().Ptr(),
        })
        return "", "", true, diags
    }
    typeAttr, typeOK := trav[1].(hcl.TraverseAttr)
    nameAttr, nameOK := trav[2].(hcl.TraverseAttr)
    if !typeOK || !nameOK {
        diags = append(diags, &hcl.Diagnostic{
            Severity: hcl.DiagError,
            Summary:  "invalid adapter reference",
            Detail:   `adapter reference segments must be bareword identifiers`,
            Subject:  attr.Expr.Range().Ptr(),
        })
        return "", "", true, diags
    }
    return typeAttr.Name, nameAttr.Name, true, diags
}
```

The helper is reused by [14-universal-step-target.md](14-universal-step-target.md) for the universal `target` attribute.

Delete the `StepSpec.Agent` field.

### Step 3 — Compile rename

Rename [workflow/compile_agents.go](../../workflow/compile_agents.go) → [workflow/compile_adapters.go](../../workflow/compile_adapters.go). Rename:

- `compileAgents` → `compileAdapters`.
- `agentConfigEvalContext` → `adapterConfigEvalContext`.
- All log/event/diagnostic strings: `"agent ..."` → `"adapter ..."`.
- The duplicate-detection key changes from `name` to `<type>.<name>`.

Add: validation that `Environment` (when set) matches a declared environment in `g.Environments` (keyed `<env_type>.<env_name>`). Missing environment is a compile error.

### Step 4 — Engine consumer rename

In [internal/engine/](../../internal/engine/), [internal/plugin/](../../internal/plugin/), [internal/cli/](../../internal/cli/), [internal/run/](../../internal/run/), [cmd/](../../cmd/):

- `*AgentNode` → `*AdapterNode`.
- `g.Agents` → `g.Adapters`.
- Field accesses on the struct: `.Adapter` (which used to be the type) → `.Type` (now the type label).

Use `gopls`/IDE rename for type-level changes; for the runtime field-access changes, do a manual sweep — the renamer cannot infer that `node.Adapter` should become `node.Type`.

**Constraint:** this workstream does not introduce auto-init or auto-teardown of adapter sessions in production code paths. Adapter session lifecycle automation is owned by [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md). If existing tests required implicit session opening, the test helper opens sessions explicitly (or uses a test-only engine option that lives in `_test.go`). Production constructors must not default to auto-bootstrap.

### Step 5 — Hard parse errors for legacy syntax

In the HCL decode path, before `gohcl.DecodeBody`:

```go
func rejectLegacyBlocks(body hcl.Body) hcl.Diagnostics {
    legacyBlockNames := map[string]string{
        "agent": `the "agent" block was renamed to "adapter" in v0.3.0; declare adapter "<type>" "<name>" { ... } and remove the legacy agent block. See CHANGELOG.md migration note.`,
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

The legacy `step { agent = "..." }` attribute rejects in the same pass with a parallel migration message.

The legacy bare `step { adapter = "<type>" }` (string, single segment) and the legacy quoted dotted `step { adapter = "<type>.<name>" }` (string, two segments) both reject during step compilation with messages directing the user to the traversal form `adapter = adapter.<type>.<name>`.

### Step 6 — Migration text

Recorded for [21-phase3-cleanup-gate.md](21-phase3-cleanup-gate.md) to paste into [CHANGELOG.md](../../CHANGELOG.md):

```
### `agent` block → `adapter "<type>" "<name>"` block

v0.2.0 form:
    agent "reviewer" {
        adapter = "copilot"
        config { reasoning_effort = "high" }
    }
    step "review" { agent = "reviewer" }

v0.3.0 form:
    adapter "copilot" "reviewer" {
        config = {
            reasoning_effort = "high"
        }
    }
    step "review" { adapter = adapter.copilot.reviewer }

Steps that used `adapter = "shell"` (bare type) must declare a named adapter:
    adapter "shell" "default" { config = {} }
and reference it: `adapter = adapter.shell.default`.
```

### Step 7 — Examples, testdata, goldens, docs

- Sweep every HCL file under [examples/](../../examples/), [workflow/testdata/](../../workflow/testdata/), [internal/cli/testdata/](../../internal/cli/testdata/), and [internal/engine/testdata/](../../internal/engine/testdata/) to use the new shape: `adapter "<type>" "<name>" { ... }` declarations and `adapter = adapter.<type>.<name>` traversal references.
- Regenerate compile and plan goldens under [internal/cli/testdata/compile/](../../internal/cli/testdata/compile/) and [internal/cli/testdata/plan/](../../internal/cli/testdata/plan/).
- Rewrite [docs/workflow.md](../../docs/workflow.md) sections that document the agent block.
- Update [docs/runtime/](../../docs/runtime/) and adapter-author docs that reference "agents" to "adapters".

### Step 8 — Events and proto

Search for event field names mentioning `agent`. In [proto/criteria/v1/](../../proto/criteria/v1/), rename any `agent_name` field to `adapter_name`. Field numbers stay stable (proto wire format is unchanged). Update [sdk/CHANGELOG.md](../../sdk/CHANGELOG.md) with the rename note. Run `make proto` to regenerate; confirm `make proto-check-drift` exits 0.

### Step 9 — Tests

- `workflow/compile_adapters_test.go`:
  - `TestCompileAdapters_BasicShape`.
  - `TestCompileAdapters_DuplicateTypeAndName`.
  - `TestCompileAdapters_EnvironmentReference_Resolves`.
  - `TestCompileAdapters_EnvironmentReference_Missing` — error.
  - `TestCompileAdapters_DefaultEnvironmentBinds` — adapter with no `environment = ...` and a workflow default environment binds to the default.

- Step adapter traversal:
  - `TestCompileStep_AdapterTraversal_Resolves` — `adapter = adapter.copilot.reviewer` resolves to the declaration.
  - `TestCompileStep_AdapterStringLiteral_HardError` — `adapter = "copilot.reviewer"` produces a parse error pointing at the traversal form.
  - `TestCompileStep_AdapterBareType_HardError` — `adapter = adapter.shell` (two segments) produces a parse error.
  - `TestCompileStep_AdapterUnresolvedTraversal_Error` — `adapter = adapter.copilot.missing` where no such instance is declared produces a compile error with source range.

- Decode rejection:
  - `TestDecode_LegacyAgentBlock_HardError` — HCL with `agent "x" { ... }` produces a parse error with the documented message and source range.
  - `TestDecode_LegacyStepAgentAttr_HardError` — HCL with `step "x" { agent = "y" }` produces a parse error.
  - `TestDecode_LegacyStepBareAdapter_HardError` — HCL with `step "x" { adapter = "shell" }` (bare string) produces an error directing the user to declare a named adapter and reference it via traversal.

- Engine:
  - `TestEngine_AdapterRoutingByDottedName` — step `adapter = adapter.copilot.reviewer` routes to the right declaration.

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
git grep -nE 'adapter\s*=\s*"[^"]*\.[^"]*"' -- 'examples/' 'workflow/testdata/' 'internal/cli/testdata/' 'internal/engine/testdata/'
```

Both final greps MUST return zero matches. The first catches missed legacy identifiers in production code. The second catches any remaining quoted dotted adapter references in HCL fixtures.

## Behavior change

Breaking for HCL authors:

1. `agent "x" { ... }` is a hard parse error.
2. `step "y" { agent = "x" }` is a hard parse error.
3. `step "y" { adapter = "shell" }` (bare string) is a hard parse error.
4. `step "y" { adapter = "shell.default" }` (quoted dotted string) is a hard parse error.
5. `step "y" { adapter = adapter.shell.default }` (bareword three-segment traversal) is the only accepted form.
6. The wire envelope's `agent_name` field renames to `adapter_name`. Field numbers stable.
7. Logs, diagnostics, and CLI output use the term "adapter" everywhere; no occurrences of "agent" in user-facing strings.

## Reuse

- Existing [`compile_agents.go`](../../workflow/compile_agents.go) compile flow — rename in place, do not rewrite.
- [`adapterConfigEvalContext`](../../workflow/compile_agents.go#L22) — rename only.
- Existing `AdapterInfo` (the schema description) — distinct from the new `AdapterNode`; document the distinction in code.
- `FoldExpr` from [07](07-local-block-and-fold-pass.md) — for adapter config attribute compile-folding.
- `hcl.AbsTraversalForExpr` — the HCL helper that turns a traversal expression into `hcl.Traversal`. The `resolveStepAdapterRef` helper in Step 2 wraps it with shape validation; [14-universal-step-target.md](14-universal-step-target.md) reuses the same wrapper.
- `gopls` / IDE rename for symbol-level changes.

## Out of scope

- Adapter session lifecycle automation (auto-init at scope start, auto-teardown at terminal). Owned by [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md). This workstream's engine and CLI must not ship runtime auto-bootstrap of adapter sessions in production paths.
- Removing `lifecycle = "open"|"close"` from steps. Owned by [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md).
- The universal step `target` attribute. Owned by [14-universal-step-target.md](14-universal-step-target.md). This workstream delivers the traversal-resolution helper that [14](14-universal-step-target.md) reuses; the attribute name `adapter` stays until [14](14-universal-step-target.md) replaces it with `target`.
- `subworkflow` block. Owned by [13-subworkflow-block-and-resolver.md](13-subworkflow-block-and-resolver.md).

## Files this workstream may modify

- [`workflow/schema.go`](../../workflow/schema.go) — rename `AgentSpec` → `AdapterDeclSpec`, `AgentNode` → `AdapterNode`, `Spec.Agents` → `Spec.Adapters`, `FSMGraph.Agents` → `FSMGraph.Adapters`. Delete `StepSpec.Agent`. Remove `Adapter` from gohcl-decoded `StepSpec` fields and pull it from `Remain` as a traversal expression.
- Rename: [`workflow/compile_agents.go`](../../workflow/compile_agents.go) → `workflow/compile_adapters.go`. Rename functions and identifiers within.
- New: traversal-resolution helper for `step.adapter` (Step 2). May live in `workflow/compile_steps_adapter.go` or a dedicated file; reused by [14](14-universal-step-target.md).
- New decode-time rejection helper file `workflow/parse_legacy_reject.go`.
- Every callsite under [`internal/engine/`](../../internal/engine/), [`internal/plugin/`](../../internal/plugin/), [`internal/cli/`](../../internal/cli/), [`internal/run/`](../../internal/run/), [`cmd/`](../../cmd/) that references the old types.
- [`proto/criteria/v1/`](../../proto/criteria/v1/) — rename `agent_name` → `adapter_name` in any envelope. Field numbers stable.
- [`sdk/CHANGELOG.md`](../../sdk/CHANGELOG.md) — additive entry per Step 8.
- Every HCL file under [`examples/`](../../examples/), [`workflow/testdata/`](../../workflow/testdata/), [`internal/cli/testdata/`](../../internal/cli/testdata/), [`internal/engine/testdata/`](../../internal/engine/testdata/).
- Goldens under [`internal/cli/testdata/compile/`](../../internal/cli/testdata/compile/) and [`internal/cli/testdata/plan/`](../../internal/cli/testdata/plan/).
- [`docs/workflow.md`](../../docs/workflow.md) and any adapter-related docs in [`docs/`](../../docs/).
- All test files needing the rename.
- Migration tests per Step 9.

This workstream may **not** edit:

- `PLAN.md`, `README.md`, `AGENTS.md`, `CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
- [`.golangci.baseline.yml`](../../.golangci.baseline.yml) — no new entries.

## Tasks

- [ ] Rename schema (Step 1).
- [ ] Make `step.adapter` a traversal expression; add the resolution helper; reject string forms (Step 2).
- [ ] Rename compile flow (Step 3).
- [ ] Rename engine/plugin/cli consumers without introducing runtime auto-bootstrap of adapter sessions (Step 4).
- [ ] Add hard parse errors for legacy syntax (Step 5).
- [ ] Record migration text (Step 6).
- [ ] Update examples, testdata, docs, and goldens to the new traversal form (Step 7).
- [ ] Rename proto field; bump SDK changelog (Step 8).
- [ ] Author all tests in Step 9.
- [ ] Validation greens per Step 10, including both `git grep` checks.

## Exit criteria

- `git grep -E '\bAgentSpec\b|\bAgentNode\b'` returns zero in production code.
- `git grep '"agent,block"'` returns zero in production code.
- `git grep 'hcl:"agent,optional"'` returns zero in production code.
- `git grep -nE 'adapter\s*=\s*"[^"]*\.[^"]*"'` returns zero matches under [examples/](../../examples/), [workflow/testdata/](../../workflow/testdata/), [internal/cli/testdata/](../../internal/cli/testdata/), [internal/engine/testdata/](../../internal/engine/testdata/) — i.e. no quoted dotted adapter references remain in HCL fixtures.
- `agent "x"` HCL block produces a hard parse error.
- `step "x" { agent = "..." }` produces a hard parse error.
- `step "x" { adapter = "shell" }` (bare string) produces a hard parse error.
- `step "x" { adapter = "shell.default" }` (quoted dotted string) produces a hard parse error.
- `adapter "<type>" "<name>"` block parses, compiles, and is referenced by `adapter = adapter.<type>.<name>` (bareword traversal) from steps.
- Adapter `environment = "<env_type>.<env_name>"` references resolve at compile.
- The traversal-resolution helper from Step 2 is exported (or reachable) for reuse by [14-universal-step-target.md](14-universal-step-target.md).
- The engine and CLI do not auto-open adapter sessions in production code paths. Auto-bootstrap belongs to [12-adapter-lifecycle-automation.md](12-adapter-lifecycle-automation.md); any test-only opt-in lives in `_test.go`.
- Wire envelopes use `adapter_name` field; SDK changelog bumped.
- Every example renamed; `make validate` green.
- Goldens regenerated.
- `make ci` exits 0.

## Tests

The Step 9 list is the deliverable. Coverage targets:

- `workflow/compile_adapters.go` ≥ 90%.
- The `rejectLegacyBlocks` path ≥ 95%.
- The `resolveStepAdapterRef` traversal helper ≥ 95%, including each rejection branch (string literal, wrong root, wrong segment count, non-attribute traversal segment).

## Risks

| Risk | Mitigation |
|---|---|
| The rename touches ~30+ files and a missed callsite breaks at link or runtime. | Run `git grep` for every legacy identifier (Step 10 has the list). Use `gopls` rename when possible; manually verify the result. |
| Renaming the proto field breaks orchestrators that read `agent_name` from the wire. | Field numbers stable means the wire format is unchanged. Orchestrators reading by field name see a name change in their generated code (recompile). Document in SDK CHANGELOG. |
| `StepSpec.Adapter` semantic flip (now requires the traversal form) breaks every existing HCL fixture. | This is the documented breaking change. Sweep all four fixture trees in Step 7 and lock the constraint with the second `git grep` in Step 10. |
| `rejectLegacyBlocks` has false positives if a workflow's `name` happens to be "agent". | Block-name matching is HCL block-type matching, not attribute-value matching. A `step "agent"` (instance named "agent") is fine; a top-level `agent "x" { ... }` block is the rejected case. Test `TestDecode_StepNamedAgent_NotRejected` to lock this in. |
| The bareword traversal `adapter.<type>.<name>` collides with a function call or other expression in HCL. | The compiler rejects anything that is not a three-segment `hcl.Traversal` rooted at the `adapter` identifier. Function calls, string literals, and indexing all fail `hcl.AbsTraversalForExpr` with a precise diagnostic. |
| Tests that use the legacy types fail to compile after the rename. | Update the tests in this workstream — they are not in scope's "may not edit" set. The signal is that all renamed-tests still verify the same behavior, just under the new names. |
