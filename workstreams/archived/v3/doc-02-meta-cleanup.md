# doc-02 — Documentation cleanup: meta / index files

**Owner:** Workstream Cleanup agent · **Depends on:** [doc-01](doc-01-docs-cleanup.md) (Step 5 of doc-01 renames `docs/roadmap/phase-3.md`; cross-links in this workstream must point to the new name) · **Blocks:** nothing

## Context

After the Phase 3 close (2026-05-06, `v0.3.0` tagged), several meta and index files were not updated to reflect the completed phase. Additionally, `README.md` still contains a "Workflow language" code example using v0.2.0 HCL syntax — the example is invalid in v0.3.0 and directly contradicts the correct quickstart example at the top of the same file. `CONTRIBUTING.md` contains a leftover `cd overseer` command from the legacy brand. `PLAN.md` has a duplicate stale Phase 3 bullet. `workstreams/README.md` still describes Phase 3 as upcoming.

This workstream fixes those five issues across four files. No source code is changed.

## Prerequisites

- `make test` green on `main`.
- `make validate` green on `main`.
- [doc-01](doc-01-docs-cleanup.md) merged (required so the `phase-3-summary.md` rename is already in place before this workstream updates cross-links to it).

## In scope — allowed files

Exactly these files may be modified:

- `README.md`
- `CONTRIBUTING.md`
- `PLAN.md`
- `workstreams/README.md`

No other file may be touched.

---

## Step 1 — `README.md` — Fix "Workflow language" example (I1)

The "Workflow language" section contains an HCL code block that uses v0.2.0 syntax in three ways:

1. Steps and states are nested **inside** the `workflow { }` block. Phase 3 W17 made the top-level-only layout the sole accepted form. Steps inside `workflow { }` are a parse error in v0.3.0.
2. `adapter = "shell"` — the v0.2.0 bare-adapter step attribute. Phase 3 W14 replaced this with `target = adapter.<type>.<name>`, requiring an `adapter "<type>" "<name>" { }` declaration.
3. `outcome "success" { transition_to = "test" }` — Phase 3 W15 renamed `transition_to` to `next`.

The corrected example should match the v0.3.0 top-level layout and mirror the style of `examples/hello/hello.hcl` (the canonical minimal example used in smoke tests).

**Find (exact text — the entire "Workflow language" section, from the heading through the closing ` ``` `):**
````
## Workflow language

```hcl
workflow "deploy" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "deployed"

  step "build" {
    adapter = "shell"
    input { command = "go build ./..." }
    outcome "success" { transition_to = "test" }
    outcome "failure" { transition_to = "failed" }
  }

  step "test" {
    adapter = "shell"
    input { command = "go test ./..." }
    outcome "success" { transition_to = "deployed" }
    outcome "failure" { transition_to = "failed" }
  }

  state "deployed" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
}
```
````

**Replace with:**
````
## Workflow language

```hcl
workflow "deploy" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "deployed"
}

adapter "shell" "default" {
  config {}
}

step "build" {
  target = adapter.shell.default
  input { command = "go build ./..." }
  outcome "success" { next = "test" }
  outcome "failure" { next = "failed" }
}

step "test" {
  target = adapter.shell.default
  input { command = "go test ./..." }
  outcome "success" { next = "deployed" }
  outcome "failure" { next = "failed" }
}

state "deployed" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
```
````

> **Rationale for the structure:** The `workflow { }` block is now header-only (version, initial_state, target_state). All steps, states, and adapter declarations live at the top level. This is the format enforced by the compiler and shown in every example under `examples/`.

After editing, verify that `criteria compile README.md` does **not** need to pass (the README example has no `# mode:` comment and is not picked up by `make validate`). Visual inspection is sufficient for this section.

---

## Step 2 — `CONTRIBUTING.md` — Fix `cd overseer` (I2)

In the Setup section, the bash clone-and-build block says `cd overseer` (legacy brand). This was the old repo name before Phase 0 W08 executed the brand rename.

**Find (exact text):**
```bash
git clone https://github.com/brokenbots/criteria.git
cd overseer
make bootstrap         # sync all three Go workspace modules
```

**Replace with:**
```bash
git clone https://github.com/brokenbots/criteria.git
cd criteria
make bootstrap         # sync all three Go workspace modules
```

Verify:
```bash
grep -n "overseer" CONTRIBUTING.md
# expected: 0 matches
```

---

## Step 3 — `PLAN.md` — Remove stale duplicate Phase 3 status bullet (I15)

The Status snapshot section has **two** Phase 3 entries. The first is a stale "TBD" placeholder from before Phase 3 was scoped and started; the second is the correct closed-phase entry. The stale bullet must be deleted.

**Find (exact text — the stale TBD bullet and the blank line that follows it):**
```
- **Phase 3 — TBD.** Architecture-team direction: HCL/runtime rework before any
  feature work. See "Phase 3 forward-pointer" below for the candidate scope
  list. Originally-planned environments / plug architecture is deferred to
  Phase 4 with a new contributor.
```

**Replace with:** *(delete entirely — no replacement text)*

After deletion, the Status snapshot section should have exactly one Phase 3 entry:
```
- **Phase 3 — HCL/runtime rework** — **closed 2026-05-06**. All nineteen active
  workstreams merged (W20 skipped); ...
```

Verify with:
```bash
grep -c "Phase 3" PLAN.md
# The count will vary depending on the rest of the document,
# but there must be no line containing "Phase 3 — TBD"
grep "Phase 3 — TBD" PLAN.md
# expected: 0 matches
```

---

## Step 4 — `workstreams/README.md` — Update Phase 3 status and forward-pointer (I13, I14)

### Fix I13 — Phase 3 status still reads "TBD"

**Find (exact text):**
```
- **Phase 3** — TBD. Architecture-team direction is an HCL/runtime rework;
  see [PLAN.md](../PLAN.md) for the candidate scope and the "Phase 3
  forward-pointer" section below.
```

**Replace with:**
```
- **Phase 3** — HCL/runtime rework — **closed 2026-05-06**. All nineteen active
  workstreams merged (W20 skipped); `v0.3.0` tagged. Archived under
  [`archived/v3/`](archived/v3/). See [docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md)
  for full outcomes.
```

### Fix I14 — "Phase 3 forward-pointer" section describes Phase 3 as upcoming

The entire "Phase 3 forward-pointer" section (from the `## Phase 3 forward-pointer` heading to the end of the file) was written when Phase 3 had not started. It describes the phase as future work and lists candidate scopes that are now shipped. Replace the section with a brief closed-phase note.

**Find (exact text — from the heading to the end of the file):**
```
## Phase 3 forward-pointer

Phase 3 is sketched in [PLAN.md](../PLAN.md) but not yet active here. Targeted
theme (per architecture_notes.md and proposed_hcl.hcl): **HCL/runtime rework
with a clean break from v0.2.0**. Twenty-one workstreams are scoped; the
detailed per-workstream files have been drafted locally and will be moved into
this directory when Phase 3 begins. The originally-planned Phase 3 environments
/ plug architecture theme is deferred to Phase 4 with a new contributor.

Headline scope:

- **Pre-rework cleanup.** Lint baseline burn-down to ≤ 50; split
  [internal/cli/apply.go](../internal/cli/apply.go) and
  [workflow/compile_steps.go](../workflow/compile_steps.go); server-mode apply
  test coverage; tracked roadmap artifact; release-process integrity.
- **Compile-time / runtime semantics.** `local "<name>"` block + constant-fold
  pass; schema unification (drop `WorkflowBodySpec`, sub-workflow IS a `Spec`,
  drop cross-scope `Vars` aliasing); top-level `output` block; `environment`
  declaration surface.
- **Language surface — clean break.** `agent` → `adapter "<type>" "<name>"`
  hard rename; adapter lifecycle automation; first-class `subworkflow` block
  with CLI resolver wiring; universal step `target` attribute; `outcome.next`
  + reserved `return` outcome; `branch` → `switch` rename; directory-level
  multi-file module compilation as the only entry shape.
- **Runtime additions.** `shared_variable` block; `parallel` step modifier;
  implicit input chaining (skipped — Phase 4).
- **Release process.** `tag-claim-check` CI guard; real release workflow;
  per-os/arch tarballs; runtime image; cosigned `SHA256SUMS`.
```

**Replace with:**
```
## Phase 3 workstreams (archived)

Phase 3 closed 2026-05-06 with `v0.3.0` tagged. All workstream files have been
moved to [`archived/v3/`](archived/v3/). See
[docs/roadmap/phase-3-summary.md](../docs/roadmap/phase-3-summary.md) for the
full per-workstream outcome summary.
```

---

## Verification checklist

After all steps are complete, run these checks before marking the workstream done:

```bash
# No transition_to or agent blocks in README.md
grep -n "transition_to\|adapter = \"shell\"\| agent \"" README.md   # must be 0 matches

# No "cd overseer" in CONTRIBUTING.md
grep -n "overseer" CONTRIBUTING.md   # must be 0 matches

# No stale TBD Phase 3 entry in PLAN.md
grep -n "Phase 3 — TBD" PLAN.md    # must be 0 matches

# No TBD Phase 3 entry in workstreams/README.md
grep -n "TBD" workstreams/README.md    # must be 0 matches

# Phase 3 forward-pointer section removed
grep -n "Phase 3 forward-pointer" workstreams/README.md    # must be 0 matches

# Roadmap link is to phase-3-summary.md
grep -n "phase-3" workstreams/README.md    # all matches must be "phase-3-summary"

# Examples still compile
make validate
```

---

## Exit criteria — reviewer checklist

The reviewer must verify each item independently.

| # | File | Check | Pass / Fail |
|---|------|-------|-------------|
| I1a | `README.md` | `## Workflow language` code example: no steps or states nested inside `workflow { }`. | |
| I1b | `README.md` | `## Workflow language` code example: top-level `adapter "shell" "default" { config {} }` block is present. | |
| I1c | `README.md` | `## Workflow language` code example: steps use `target = adapter.shell.default`; no `adapter = "shell"`. | |
| I1d | `README.md` | `## Workflow language` code example: outcomes use `next = ...`; zero occurrences of `transition_to`. | |
| I2 | `CONTRIBUTING.md` | Setup bash block says `cd criteria`; zero occurrences of `cd overseer` or any `overseer` string. | |
| I15 | `PLAN.md` | Status snapshot contains exactly one Phase 3 entry; no bullet containing `Phase 3 — TBD`. | |
| I13 | `workstreams/README.md` | Status section Phase 3 bullet reads "closed 2026-05-06" and references `phase-3-summary.md`. | |
| I14 | `workstreams/README.md` | `## Phase 3 forward-pointer` heading does not exist; replaced by `## Phase 3 workstreams (archived)`. | |
| V1 | repo | `make validate` passes (examples unchanged). | |

All 9 checks must pass before reviewer approval.

---

## Executor notes

All five changes implemented on branch `cleanup/doc-02-meta-cleanup` (2026-05-09):

- **I1 (README.md):** Replaced the v0.2.0 `workflow { }` nested code block with the
  v0.3.0 top-level layout matching `examples/hello/hello.hcl`. Uses
  `adapter "shell" "default" { config {} }`, `target = adapter.shell.default`, and
  `next = ...` throughout.
- **I2 (CONTRIBUTING.md):** `cd overseer` → `cd criteria`.
- **I15 (PLAN.md):** Removed stale `- **Phase 3 — TBD.**` bullet from Status snapshot.
  Also updated the stale `## Phase 3 — TBD` section heading and its forward-looking
  candidate scope body to a proper closed-phase section with full W01–W21 workstream
  links (matching the Phase 0/1/2 section style).
- **I13/I14 (workstreams/README.md):** Phase 3 status bullet updated to "closed
  2026-05-06"; `## Phase 3 forward-pointer` section replaced with
  `## Phase 3 workstreams (archived)` brief note. Post-phase doc workstreams
  doc-01 and doc-02 also listed in that section after archival.

**Validation:** `make validate` failed on `examples/phase3-parallel` (pre-existing
failure on `main` before this branch; confirmed by `git stash` + re-run). Tracked by
active workstream `parallel-02-adapter-parallel-safe-capability.md`. All other
examples passed. Doc-only changes do not affect the failure.

## Reviewer notes

*(To be filled in by the reviewer agent.)*
