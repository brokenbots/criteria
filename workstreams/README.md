# Criteria workstreams

The active phase's workstream files live at the top of this directory;
prior phases are in [`archived/`](archived/).

## Status

- **Phase 0** — post-separation cleanup — **closed 2026-04-27**. All nine
  workstreams merged; `v0.1.0` tagged. Archived under [`archived/v0/`](archived/v0/).
- **Phase 1** — stabilization + critical user fixes — **closed 2026-04-29**.
  All eleven workstreams merged; lint baseline burn-down gate clean.
  Archived under [`archived/v1/`](archived/v1/). The `v0.2.0` tag was
  documented but not pushed at this close; it ships at HEAD with the
  combined Phase 1 + Phase 2 work below.
- **Phase 2** — maintainability + unattended MVP + Docker runtime + Copilot
  tool-call finalization — **closed 2026-05-02**. Sixteen workstreams scoped,
  two cancelled (W05, W11). `v0.2.0` tagged at HEAD covering combined Phase 1
  + Phase 2 work. Archived under [`archived/v2/`](archived/v2/).
- **Phase 3** — HCL/runtime rework — **closed 2026-05-06**. All nineteen active
  workstreams merged (W20 skipped); `v0.3.0` tagged. Archived under
  [`archived/v3/`](archived/v3/). See [docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md)
  for full outcomes.
- **v0.3.1** — post-Phase-3 bugfixes + parallel correctness — **closed
  2026-05-xx**. Eleven workstreams (6 bugfix, 4 parallel, 1 QoL). Archived
  under [`archived/v3.1/`](archived/v3.1/).
- **v0.3.2** — pre-Phase-4 feature + tech-debt prep — **closed 2026-05-13**.
  Twelve workstreams (2 doc, 5 feat, 4 tech debt, 1 test). All merged; `v0.3.2`
  tag pending. Archived under [`archived/v3.2/`](archived/v3.2/).

## Phase 2 workstreams (archived)

All Phase 2 workstream files have been moved to [`archived/v2/`](archived/v2/).
See [PLAN.md](../PLAN.md) for the project-level roadmap with per-workstream
links and outcomes.

## Phase 1 workstreams (archived)

All Phase 1 workstream files have been moved to [`archived/v1/`](archived/v1/).

## Phase 0 workstreams (archived)

All Phase 0 workstream files have been moved to [`archived/v0/`](archived/v0/).

## Phase 3 workstreams (archived)

Phase 3 closed 2026-05-06 with `v0.3.0` tagged. All workstream files have been
moved to [`archived/v3/`](archived/v3/). See
[docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md) for the
full per-workstream outcome summary.

Post-phase documentation cleanup workstreams (also archived to `archived/v3/`):

- [doc-01](archived/v3/doc-01-docs-cleanup.md) ✅ — Docs cleanup: runtime/compiler reference and roadmap files.
- [doc-02](archived/v3/doc-02-meta-cleanup.md) ✅ — Docs cleanup: meta/index files (`README.md`, `CONTRIBUTING.md`, `PLAN.md`, `workstreams/README.md`).

## v0.3.1 workstreams (archived)

Post-Phase-3 bugfix and parallel correctness workstreams. All files moved to
[`archived/v3.1/`](archived/v3.1/).

## v0.3.2 workstreams (archived)

Pre-Phase-4 feature and tech-debt prep workstreams, closed 2026-05-13. All files
moved to [`archived/v3.2/`](archived/v3.2/).

- [doc-03](archived/v3.2/doc-03-llm-language-spec.md) ✅ — `docs/LANGUAGE-SPEC.md` + `spec-gen` tool.
- [doc-04](archived/v3.2/doc-04-llm-prompt-pack.md) ✅ — LLM prompt pack (8 curated HCL examples).
- [feat-01](archived/v3.2/feat-01-templatefile-function.md) ✅ — `templatefile(path, vars)` HCL function.
- [feat-02](archived/v3.2/feat-02-fileset-function.md) ✅ — `fileset(path, pattern)` HCL function.
- [feat-03](archived/v3.2/feat-03-hash-crypto-encoding-functions.md) ✅ — 13 hash, encoding, and dynamic HCL functions.
- [feat-04](archived/v3.2/feat-04-while-step-modifier.md) ✅ — `while` step iteration modifier.
- [feat-05](archived/v3.2/feat-05-per-line-console-output.md) ✅ — Per-line console output.
- [td-01](archived/v3.2/td-01-lint-baseline-ratchet.md) ✅ — Lint baseline ratchet 24 → 16.
- [td-02](archived/v3.2/td-02-nolint-suppression-sweep.md) ✅ — `//nolint` suppression sweep (62 → 31).
- [td-03](archived/v3.2/td-03-staticcheck-deprecated-enum.md) ✅ — Staticcheck deprecated-enum cleanup.
- [td-04](archived/v3.2/td-04-todo-closure.md) ✅ — TODO marker closure + lint-no-todos guard.
- [test-02](archived/v3.2/test-02-hcl-parsing-eval-coverage.md) ✅ — HCL parsing and eval coverage gaps.

## Phase 4 — Adapter system v2 (active)

Phase 4 opens the full adapter-system rewrite. Workstream files are in
[`adapter_v2/`](adapter_v2/). See [`adapter_v2/README.md`](adapter_v2/README.md)
for scope, goals, and workstream index.

## Workstream conventions

Every workstream file declares:

- **Goal**, **Prerequisites**, **In scope** (with file paths and line ranges),
  **Out of scope** (explicit "do not touch" list), **Reuse pointers** (existing
  functions/interfaces to use), **Behavior change** disclosure ("yes" or "no";
  if yes, every observable difference enumerated for the reviewer), **Tests
  required**, **Exit criteria**, and a **Files this workstream may modify**
  list.
- The "may not edit" set is restated in every workstream: `README.md`,
  `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`,
  `workstreams/README.md`, and any other workstream file. Those are the
  cleanup-gate's territory.

See [PLAN.md](../PLAN.md) for the project-level roadmap.

## Files NOT editable by workstream-executor or workstream-reviewer

The executor and reviewer agents are scoped to **the single workstream
file they are executing**. They may not edit:

- `README.md`
- `PLAN.md`
- `AGENTS.md`
- `CHANGELOG.md`
- `CONTRIBUTING.md`
- `workstreams/README.md`
- Any other workstream file in this directory

A workstream that needs changes to those files declares them in its
"Files this workstream may modify" list and must be the cleanup gate
for that phase, or it defers the edit to the cleanup gate with a
forward-pointer note in its reviewer log.

## Archived

- Phase 0 — [`archived/v0/`](archived/v0/) (closed 2026-04-27, `v0.1.0`).
- Phase 1 — [`archived/v1/`](archived/v1/) (closed 2026-04-29).
- Phase 2 — [`archived/v2/`](archived/v2/) (closed 2026-05-02, `v0.2.0`
  combined-phase tag).
- Phase 3 — [`archived/v3/`](archived/v3/) (closed 2026-05-06, `v0.3.0`).
- v0.3.1 — [`archived/v3.1/`](archived/v3.1/) (post-Phase-3 bugfixes + parallel).
- v0.3.2 — [`archived/v3.2/`](archived/v3.2/) (pre-Phase-4 feature + tech-debt prep, closed 2026-05-13).

The pre-separation v1.x phases live in the orchestrator repo's
`workstreams/archived/`; they are not copied here.
