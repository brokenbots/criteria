# Phase 3 Roadmap Summary

**Phase 3 — HCL/runtime rework** closed **2026-05-06**, delivering `v0.3.0`.

## Outcomes

A clean break from v0.2.0 with comprehensive HCL language rework and runtime architecture improvements. All 19 active workstreams merged; workstream 20 (implicit input chaining) deferred to Phase 4 due to failed-plan risk concerns. Lint baseline burn-down to 21 entries (zero `errcheck`/`contextcheck`). Maintainability and Tech Debt both lifted to **B**. Release process integrity (`tag-claim-check` CI guard) shipping.

## Workstreams

### Pre-rework cleanup (W01–W06)

| WS | Title | Outcome |
|----|-------|---------|
| 01 | Lint baseline burn-down | Reduced from 50+ to 21 entries; no `errcheck`/`contextcheck` per architectural contract. Coverage floors raised. Maintainability and Tech Debt lifted to B. |
| 02 | Split CLI apply | `internal/cli/apply.go` split into focused files: `apply_compile.go`, `apply_execute.go`. No behavior change. |
| 03 | Split compile steps | `workflow/compile_steps.go` split by step-kind lines into `compile_step_foreach.go`, `compile_step_workflow.go`, etc. No behavior change. |
| 04 | Server-mode apply test coverage | Transport `server/` coverage raised from 63.4% to 70%; previously 0% functions now at ≥60% each. |
| 05 | Tracked roadmap artifact | `docs/roadmap/phase-2-summary.md` replaces local `~/.claude/...` plan reference. Permanent summaries for all prior phases. |
| 06 | Release process integrity | Added `tag-claim-check` CI job validating claimed tags match remote; real release workflow on tag push (not RC-only); per-os/arch tarballs, runtime image, cosigned SHA256SUMS. |

### Semantics and schema (W07–W10)

| WS | Title | Outcome |
|----|-------|---------|
| 07 | Local variables and fold | New `local "<name>" { value = ... }` block for compile-time constants. Compile pass folds `local.*` and rejects undeclared `var.*` (no runtime inference). `file()` function broadened. |
| 08 | Schema unification | Removed `WorkflowBodySpec` complexity; subworkflows ARE Specs. Implicit cross-scope `Vars` aliasing removed. Undeclared variable references now compile errors. |
| 09 | Top-level output block | New `output "<name>" { type = ..., value = ... }` block at workflow root. Emitted as `run.outputs` event. Full type system (number, string, list(string), etc.). |
| 10 | Environment block | New `environment "<type>" "<name>" { variables = { ... } }` declaration; injected into adapter subprocess via env vars. |

### Language clean break (W11–W17)

| WS | Title | Outcome | Breaking |
|----|-------|---------|----------|
| 11 | Agent → adapter rename | `agent "name"` → `adapter "<type>" "<name>"` block. Proto field rename `agent_name` → `adapter` (field number stable). | **YES** |
| 12 | Adapter lifecycle automation | `lifecycle = "open"\|"close"` removed. Adapters auto-open on scope entry, auto-close on exit (LIFO). | **YES** |
| 13 | Subworkflow first-class | New `subworkflow "<name>" { source = "path" }` top-level block. Inline `step.workflow { ... }` and `step.workflow_file` removed. | **YES** |
| 14 | Universal step target | Unified `step.target = adapter.<type>.<name> \| subworkflow.<name>` (replaces `step.adapter`, `step.agent`, `step.workflow*`). | **YES** |
| 15 | Outcome and return | `outcome.next` replaces `transition_to`. Reserved `return` outcome. `outcome.output` projection. `default_outcome` attribute. | **YES** |
| 16 | Switch and if flow | `branch { arm { ... } }` → `switch { condition { match = ..., next = ... } }`. `if` deferred to Phase 4. | **YES** |
| 17 | Directory-mode modules | Single-file entry point removed; directory-only. Workflow attributes wrap in `workflow "<name>" { ... }` block. | **YES** |

### Runtime (W18–W20)

| WS | Title | Outcome | Status |
|----|-------|---------|--------|
| 18 | Shared variable block | New `shared_variable "<name>" { type = ..., initial = ... }` block. Engine-locked during concurrent iterations. | ✅ Merged |
| 19 | Parallel step modifier | New `parallel = [list]` attribute on steps. Per-iteration adapter sessions. Full concurrency, race-clean. | ✅ Merged |
| 20 | Implicit input chaining | Default `step.input` to previous step output. | ⏭️ Skipped (Phase 4) |

### Close gate (W21)

| WS | Title | Outcome |
|----|-------|---------|
| 21 | Phase 3 cleanup gate | Validation: build/lint/test gates, smoke test, baseline cap, determinism, security, coverage. Legacy-removal grep gate. Tech evaluation re-run. Archive to `archived/v3/`. Tag `v0.3.0`. |

## Key achievements

- **Clean HCL break**: `agent` → `adapter`, `transition_to` → `next`, `branch` → `switch`.
- **Subworkflows first-class**: No more inline/attribute models.
- **Automatic lifecycle**: Adapters open/close on scope entry/exit.
- **Parallel execution**: `parallel = [list]` step modifier with full concurrency.
- **Shared variables**: Engine-locked mutable state across iterations.
- **Top-level outputs**: Full type system; `run.outputs` event.
- **Compile-time constants**: `local` block; undeclared `var.*` compile errors.
- **Environment injection**: `environment` blocks for subprocess env vars.
- **Directory-only modules**: Single .hcl file entry removed.
- **Lint baseline**: 21 entries (down from 50+); zero `errcheck`/`contextcheck`.
- **Tech grades**: Maintainability B, Tech Debt B (from C+).
- **Release integrity**: `tag-claim-check` CI guard; signed artifacts.

## Tech evaluation scores

All targets met:

- **Maintainability**: B ✅ (was C+)
- **Tech Debt**: B ✅ (was C+)
- **Architecture**: B+ ✅ (was B)
- **Release/Operations**: B- ✅ (was C)

## Breaking changes reference

Every item below is a hard error on v0.3.0+ if used:

- `agent` block and `step.agent` attribute
- `step.adapter` (both forms)
- `step.lifecycle` attribute
- Inline `step.workflow { ... }` and `step.workflow_file` attribute
- `type = "workflow"` on steps
- `branch` block and `arm` sub-block
- `transition_to` attribute (everywhere)
- Top-level workflow attributes outside `workflow` block
- Implicit cross-scope `Vars` aliasing
- Single-file workflow entry point

## Source plan

This summary was generated from the Phase 3 cleanup gate workstream ([21-phase3-cleanup-gate.md](../../workstreams/archived/v3/21-phase3-cleanup-gate.md)) and reflects the current state of the codebase after all merged workstreams. Phase 3 is a permanent archive; forward work is tracked in PLAN.md under Phase 4 and beyond.

---

*Phase 3 closed 2026-05-06. Archived under [workstreams/archived/v3/](../../workstreams/archived/v3/).*
