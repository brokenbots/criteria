# Workstream 2 — Lint CI gate

**Owner:** Workstream executor · **Depends on:** [W01](01-lint-baseline-mechanical-burn-down.md) · **Unblocks:** [W14](14-phase2-cleanup-gate.md) (cleanup gate verifies the cap is enforced).

## Context

`make lint-go` is wired into CI today
([.github/workflows/ci.yml:39-40](../.github/workflows/ci.yml)) but is
not a hard merge gate. Per the v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
section 6 item 9), there is no enforcement preventing
`.golangci.baseline.yml` from growing in a PR — the per-workstream
burn-down contract relies on the executor noticing the growth manually.
This workstream converts the contract into mechanical enforcement.

Two enforcement levers:

1. **Baseline-stays-flat cap.** A new `make lint-baseline-check` target
   compares the entry count in the PR's
   `.golangci.baseline.yml` against a committed cap (initially set
   from W01's final count) and fails CI if the count exceeds the cap.
2. **Branch protection.** GitHub branch protection on `main` requires
   the existing `Lint` job to pass before merge. This is configuration,
   not code; document the required setting so a project admin can apply
   it.

This workstream does not lower the cap below W01's final count. Future
phase cleanups (W03 finishing W04 residuals, future workstreams) lower
the cap as part of their exit criteria.

## Prerequisites

- [W01](01-lint-baseline-mechanical-burn-down.md) merged and tagged
  baseline count recorded.
- `make ci` green on `main`.

## In scope

### Step 1 — Add `tools/lint-baseline/cap.txt`

Create `tools/lint-baseline/cap.txt` containing the integer cap
(W01's final entry count, e.g. `120`). One number per line; allow a
trailing newline. The file is the source of truth — committing a new
cap is the explicit operator action that approves a baseline change.

### Step 2 — Add `make lint-baseline-check` target

Add the target to `Makefile`:

```make
lint-baseline-check: ## Fail if .golangci.baseline.yml exceeds the cap in tools/lint-baseline/cap.txt
	@cap=$$(cat tools/lint-baseline/cap.txt); \
	count=$$(grep -c '^\s*-' .golangci.baseline.yml); \
	if [ "$$count" -gt "$$cap" ]; then \
		echo "ERROR: .golangci.baseline.yml has $$count entries; cap is $$cap (tools/lint-baseline/cap.txt)."; \
		echo "       Either fix the new findings or, with explicit reviewer agreement, raise the cap."; \
		exit 1; \
	fi; \
	echo "Lint baseline within cap ($$count / $$cap)."
```

Add it to `.PHONY`. Update `make help` doc by ensuring the `##` comment
is present on the target line so the existing `awk` help target picks
it up.

The `grep -c '^\s*-'` counts list entries; if the baseline format
changes (it shouldn't) the script needs an update. Document this
assumption in `docs/contributing/lint-baseline.md`.

### Step 3 — Wire the cap check into the lint CI job

Update `.github/workflows/ci.yml` `lint` job. After `make lint-go`,
add:

```yaml
      - name: Lint baseline cap check
        run: make lint-baseline-check
```

The check runs only after `make lint-go` passes — it is a *secondary*
gate that prevents silent baseline growth even when lint itself is
green.

### Step 4 — Update `make ci` to include the cap check

The aggregate `ci` target (already in `Makefile`) should call
`lint-baseline-check`. Add it to the dependency list of `ci`:

```make
ci: lint-imports lint-go lint-baseline-check test test-conformance validate ## Run the same checks CI runs
```

### Step 5 — Document branch protection

Add a section to `docs/contributing/lint-baseline.md` (or the file the
project uses as the lint-baseline contract) titled "Branch protection".
It should:

- Name the required status check (the `Lint` job).
- State that direct pushes to `main` are not permitted; all changes
  go through PR.
- Note that raising the cap requires a separate commit that updates
  `tools/lint-baseline/cap.txt` and is reviewable on its own.

The branch protection itself is GitHub configuration applied by a
repo admin — this workstream produces the documentation; the admin
applies the setting separately. Mark this as a Phase 2 cleanup-gate
verification item ([W14](14-phase2-cleanup-gate.md) confirms the
setting is applied).

### Step 6 — Validate

Run from a feature branch:

1. `make lint-baseline-check` — green.
2. Add a fake suppression to `.golangci.baseline.yml` so the count
   exceeds the cap.
3. `make lint-baseline-check` — fails with the documented message.
4. Revert the fake suppression. Run `make ci` — green.

Document the manual validation steps in reviewer notes.

## Behavior change

**No engine behavior change. CI behavior changes only.**

- New CI status check `Lint` will fail PRs that grow
  `.golangci.baseline.yml` beyond the cap, even if `make lint-go`
  itself is green.
- `make ci` now includes `lint-baseline-check`.
- No CLI flag, HCL surface, log line, or runtime behavior is altered.

## Reuse

- The existing `make lint-go` target. Do not modify its config-merge
  logic.
- The existing `tools/lint-baseline/main.go` already exists; if it
  exposes a programmatic count it should be preferred over `grep -c`.
  Inspect the binary first; if it has a `--count` mode, call that from
  the Makefile target instead of grep.

## Out of scope

- Lowering the cap. The cap starts at W01's final count and stays put
  until a later workstream burns it down.
- Removing the baseline file entirely. That is a far-future workstream
  once the count reaches zero.
- Adding new linter rules. Belongs in a later phase.
- Re-running W01's mechanical burn-down. This workstream assumes W01
  is merged.
- Applying the branch-protection setting in the GitHub admin UI.
  Documented; applied by an admin out-of-band.

## Files this workstream may modify

- `Makefile` (new `lint-baseline-check` target; updated `ci` target).
- `.github/workflows/ci.yml` (new step in the `lint` job).
- `tools/lint-baseline/cap.txt` (new file).
- `tools/lint-baseline/main.go` (only if a `--count` mode is added to
  feed the Makefile target; do not change its existing behavior).
- `docs/contributing/lint-baseline.md` (new "Branch protection"
  section + cap mechanics doc).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [ ] Create `tools/lint-baseline/cap.txt` with W01's final count.
- [ ] Add `make lint-baseline-check` target.
- [ ] Add `.PHONY` entry; verify `make help` lists the target.
- [ ] Update `make ci` to include `lint-baseline-check`.
- [ ] Add the cap-check step to `.github/workflows/ci.yml` `lint` job.
- [ ] Update `docs/contributing/lint-baseline.md` with cap mechanics
      and branch-protection guidance.
- [ ] Manual validation: cap fails when baseline exceeds; cap passes
      when within. Document in reviewer notes.
- [ ] `make ci` green on the workstream branch.
- [ ] CI run on the PR shows the new step in the `lint` job.

## Exit criteria

- `make lint-baseline-check` exits 0 on `main`.
- `make lint-baseline-check` exits 1 with the documented message when
  `.golangci.baseline.yml` is artificially grown beyond the cap (then
  reverted).
- `.github/workflows/ci.yml` `lint` job runs the cap check.
- `make ci` includes `lint-baseline-check`.
- `tools/lint-baseline/cap.txt` exists with a sensible value.
- Branch-protection guidance documented in
  `docs/contributing/lint-baseline.md`.
- `make ci` green.

## Tests

This workstream does not add Go tests. Verification is the manual
validation in Step 6, captured in reviewer notes.

## Risks

| Risk | Mitigation |
|---|---|
| The `grep -c '^\s*-'` heuristic miscounts if the baseline file format changes | Pin the format expectation in `docs/contributing/lint-baseline.md`. If `tools/lint-baseline/main.go` exposes a programmatic count, use it. |
| A legitimate burn-down PR fails the gate because lowering the cap requires a separate commit | Document in the contributor guide that lowering the cap is a one-line commit; offer to bundle the cap-lower into the burn-down PR. |
| Branch protection is documented but never applied by an admin | [W14](14-phase2-cleanup-gate.md) verifies the setting is applied as part of the cleanup gate. If not applied by then, escalate. |
| The cap check fails before `make lint-go` runs (ordering issue) | The cap check runs *after* `make lint-go` in CI; in `make ci` it is a separate target so execution order is determined by the dependency list. |
