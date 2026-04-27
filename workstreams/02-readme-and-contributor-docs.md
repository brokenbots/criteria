# Workstream 2 — README and contributor docs

**Owner:** Doc agent (or human committer) · **Depends on:** [W01](01-naming-convention-review.md) · **Unblocks:** [W08](08-phase0-cleanup-gate.md).

## Context

The current `README.md` and `CONTRIBUTING.md` were authored as
"first drafts" during the v1.6 split (see W08 Step 7 in the overlord
repo's archived workstreams). The W08 reviewer notes called these
out as deferred work. Phase 0 is the explicit catch-up.

The audience is shifting from "Castle implementer" to
"general-purpose user installing a workflow CLI". The docs should
read that way: someone arriving from a search result for "Go
workflow engine" should understand within 30 seconds what overseer
is, why they would use it, what they get out of the box, and how to
run their first workflow.

[W01](01-naming-convention-review.md)'s ADR-0001 may change the
project name. This workstream consumes the ADR's conclusions; if a
rename is happening, this workstream also sweeps the user-visible
strings affected by it. If no rename, the ADR is referenced as
rationale and nothing else changes.

## Prerequisites

- [W01](01-naming-convention-review.md) merged with ADR-0001 in
  `Accepted` state.
- `make build`, `make test`, `make validate`, `make lint-imports`
  green on `main`.

## In scope

### Step 1 — README rewrite

Replace the existing README with a real one. Required sections, in
order:

1. **One-paragraph elevator pitch.** What overseer is, who it's for,
   what it competes with. Plain English. No internal jargon.
2. **Install.** `go install` path; pre-built binary expectation
   (link to W07/W08's release asset path if available, otherwise
   note it's coming with the first tag).
3. **Quickstart.** Two commands max: write a `hello.hcl`, run
   `overseer apply hello.hcl`. Show the output.
4. **What's in the box.** Bullet list of the standalone capabilities
   (HCL → FSM, local execution, plugin model, conformance suite for
   third-party orchestrators).
5. **Workflow language.** One short example, then a link to
   `docs/workflow.md`.
6. **Plugins.** One short example, then a link to `docs/plugins.md`.
7. **Talking to a Castle-compatible orchestrator.** One paragraph
   describing the SDK contract; link to the conformance suite and
   to the overlord repo as the reference orchestrator.
8. **Status.** Honest one-paragraph status: "v0.x, internal use,
   public release pending" (or whatever's true at the time of the
   rewrite).
9. **License.** Pointer to `LICENSE` (added in W07).

The current README has six sections (Packages, Quickstart,
Development, Adapter plugins, Workflow syntax, SDK conformance,
License). Some of those collapse, some expand; the rewrite is not
a structural copy.

### Step 2 — CONTRIBUTING rewrite

Replace the existing CONTRIBUTING with a real one. Required sections:

1. **Setup.** Prereqs (Go version), `make bootstrap`, where the
   workspace lives, how to run a build.
2. **Project layout.** One-paragraph orientation; link to AGENTS.md
   for the deeper map.
3. **Development workflow.** Branch, edit, test, PR — the obvious
   path, written so a first-time contributor can follow it.
4. **Test lanes.** `make test`, `make test-conformance`,
   `make validate`, `make lint-imports`. What each one is for and
   when to run it.
5. **Proto changes.** Edit, `make proto`, commit both. Drift check
   in CI.
6. **Workstream-driven workflow.** How agent-executed workstreams
   work in this repo: each PR is one workstream file; the executor
   and reviewer agents are scoped to that file; the cleanup gate
   handles the coordination set (README/PLAN/AGENTS).
7. **Published SDK contract.** What's stable, what's a breaking
   change, version-bump policy. (Carry over from current
   CONTRIBUTING; tighten the language.)
8. **Adapter plugins.** Short pointer to docs/plugins.md.
9. **Code style.** Slog logging, no CGO, etc.

### Step 3 — Doc-internal links

Scan `docs/workflow.md` and `docs/plugins.md` for any remaining
references to the overlord repo or to in-tree paths that no longer
exist. Fix in place. (Most of this was swept during the post-split
cleanup that opened Phase 0; this step is a final pass.)

### Step 4 — Apply ADR-0001 outcomes

If [W01](01-naming-convention-review.md)'s ADR recommends a rename,
sweep all user-visible strings affected by it within the scope of
this workstream:

- README, CONTRIBUTING, AGENTS.md prose.
- `docs/workflow.md`, `docs/plugins.md`.
- Example HCL comments.
- Help text in CLI commands (`internal/cli/*.go` `usage:` strings).

Do **not** rename Go identifiers, env vars, module paths, or
binary names in this workstream — those are larger and structural
and belong to a separate phase. If the ADR mandates those too,
flag in the workstream's reviewer notes and stop; the rename is a
separate phase.

If the ADR is "no rename", skip this step.

## Out of scope

- Renaming Go identifiers, module paths, binary names, env vars.
- Authoring `docs/workflow.md` or `docs/plugins.md` from scratch
  (those are intact from the split; this workstream only fixes
  links and stale strings).
- Marketing-site / external landing-page work.
- Architectural changes.

## Files this workstream may modify

- `README.md`
- `CONTRIBUTING.md`
- `docs/workflow.md`
- `docs/plugins.md`

This workstream may **not** edit `AGENTS.md`, `PLAN.md`, or any
other workstream file. If something must change in those, defer it
to [W08](08-phase0-cleanup-gate.md) with a forward-pointer note.

## Tasks

- [ ] Read ADR-0001 from [W01](01-naming-convention-review.md).
- [ ] Rewrite `README.md` per Step 1.
- [ ] Rewrite `CONTRIBUTING.md` per Step 2.
- [ ] Sweep `docs/workflow.md` and `docs/plugins.md` for stale
      references.
- [ ] Apply ADR-0001 prose-level renames if any.
- [ ] Run `make build && make test && make validate && make lint-imports`
      to confirm nothing wires through the doc files.

## Exit criteria

- `README.md` and `CONTRIBUTING.md` reflect the post-split,
  standalone-overseer reality and follow the section structure
  above.
- All in-doc links resolve.
- No `proto/overlord/v1/`, `shared/pb/overlord`, `shared/sdk/`,
  `OVERLORD_*` strings in any modified file.
- ADR-0001's prose-level conclusions are reflected.

## Tests

None directly. The validation is human readability + the existing
build/test/validate/lint-imports lanes (which gate against any
accidental code drift).

## Risks

| Risk | Mitigation |
|---|---|
| Doc-rewrite scope creep into structural code changes | Hard stop at user-visible prose. Anything code-level gets a forward-pointer; it's not this workstream's job. |
| ADR-0001 changes after this workstream lands | Acceptable; the next phase or W08 sweeps any divergence. |
| README quickstart breaks after a future code change | The CLI smoke step in CI guards the apply path; if the README's commands diverge, CI catches it the next time someone runs the smoke against the README's literal commands. (Optional: lift the README quickstart into an executable doctest in a follow-up.) |
