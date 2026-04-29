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

- [x] Create `tools/lint-baseline/cap.txt` with W01's final count.
- [x] Add `make lint-baseline-check` target.
- [x] Add `.PHONY` entry; verify `make help` lists the target.
- [x] Update `make ci` to include `lint-baseline-check`.
- [x] Add the cap-check step to `.github/workflows/ci.yml` `lint` job.
- [x] Update `docs/contributing/lint-baseline.md` with cap mechanics
      and branch-protection guidance.
- [x] Manual validation: cap fails when baseline exceeds; cap passes
      when within. Document in reviewer notes.
- [x] `make ci` green on the workstream branch.
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

Unit coverage was added for `tools/lint-baseline` count mode
(`TestCountBaselineRules`, `TestCountBaselineRulesMissingFile`).
Behavioral verification for the Make/CI integration remains the manual
validation in Step 6, captured in reviewer notes.

## Risks

| Risk | Mitigation |
|---|---|
| The `grep -c '^\s*-'` heuristic miscounts if the baseline file format changes | Pin the format expectation in `docs/contributing/lint-baseline.md`. If `tools/lint-baseline/main.go` exposes a programmatic count, use it. |
| A legitimate burn-down PR fails the gate because lowering the cap requires a separate commit | Document in the contributor guide that lowering the cap is a one-line commit; offer to bundle the cap-lower into the burn-down PR. |
| Branch protection is documented but never applied by an admin | [W14](14-phase2-cleanup-gate.md) verifies the setting is applied as part of the cleanup gate. If not applied by then, escalate. |
| The cap check fails before `make lint-go` runs (ordering issue) | The cap check runs *after* `make lint-go` in CI; in `make ci` it is a separate target so execution order is determined by the dependency list. |

## Reviewer notes

### Batch 1 implementation

- Added `tools/lint-baseline/cap.txt` with cap `117` (W01 final count).
- Added `lint-baseline-check` Make target and `.PHONY` entry; `make help`
  now lists `lint-baseline-check`.
- Updated `ci` aggregate target dependencies to include
  `lint-baseline-check`.
- Added `Lint baseline cap check` step to `.github/workflows/ci.yml` `lint`
  job.
- Updated `docs/contributing/lint-baseline.md` with:
  - cap-check mechanics,
  - counting assumption (`- path:` entry counting via
    `tools/lint-baseline -count`),
  - branch-protection requirements.
- Extended `tools/lint-baseline/main.go` with `-count` mode so cap checks use
  a programmatic entry count instead of fragile grep heuristics.
- Added unit tests for count mode in `tools/lint-baseline/main_test.go`.

### Validation evidence

- `go test ./tools/lint-baseline` ✅
- `make lint-baseline-check` (baseline unchanged) ✅  
  Output: `Lint baseline within cap (117 / 117).`
- Synthetic growth test (temporary appended suppression, then reverted) ✅  
  `make lint-baseline-check` failed as expected with:
  `ERROR: .golangci.baseline.yml has 118 entries; cap is 117 (tools/lint-baseline/cap.txt).`
- `make ci` ✅

### Outstanding

- `CI run on the PR shows the new step in the lint job` remains pending until
  this branch is pushed and PR CI executes.

### Batch 2 remediation (review changes-requested)

- **[BLOCKER fixed]** `tools/lint-baseline/cap.txt` is now tracked in git.
  Evidence: `git ls-files tools/lint-baseline/cap.txt` returns
  `tools/lint-baseline/cap.txt`.
- **[NIT fixed]** Expanded `TestCountBaselineRules` with a `header only` case
  asserting a zero-entry baseline returns count `0`.
- **[NIT fixed]** Expanded `TestCountBaselineRules` with
  `text value starts with path token` case asserting a `text:` value of
  `'- path: internal/foo.go'` does not inflate entry count.
- **[NIT fixed]** Added numeric-cap validation in `make lint-baseline-check`.
  If `cap.txt` is non-numeric, the target now fails with:
  `ERROR: tools/lint-baseline/cap.txt must contain a single integer; got: <value>`.

### Batch 2 validation evidence

- `go test ./tools/lint-baseline/...` ✅
- `make lint-baseline-check` (valid cap) ✅
- `make lint-baseline-check` with temporary invalid cap (`not-a-number`) ✅  
  Fails with clear integer-validation error.
- `make ci` ✅

## Reviewer Notes

### Review 2026-04-29 — changes-requested

#### Summary

The implementation correctly covers every W02 plan item — the Makefile target,
`.PHONY` entry, `make help` listing, `ci` aggregate update, CI YAML step,
`-count` mode in `tools/lint-baseline/main.go`, unit tests, and branch-protection
documentation. All exit criteria pass when verified locally. One blocker prevents
approval: `tools/lint-baseline/cap.txt` is present in the working tree but
**untracked** (not committed to git). Without this file in the repository,
`make lint-baseline-check` fails in CI with "No such file or directory",
defeating the entire enforcement mechanism. Three nits must also be resolved
before the next review pass.

#### Plan Adherence

- **Step 1** (cap.txt): File exists with value `117`; passes `make lint-baseline-check`
  locally. **NOT committed to git** — `git status` shows `?? tools/lint-baseline/cap.txt`.
  This is a blocker.
- **Step 2** (lint-baseline-check target): Implemented correctly. Uses
  `go run ./tools/lint-baseline -count` rather than the plan's fallback
  `grep -c` heuristic, which the plan explicitly preferred. `##` comment present;
  `make help` lists the target. `.PHONY` updated. ✓
- **Step 3** (CI YAML step): `Lint baseline cap check` step added after `make lint-go`
  in the `lint` job. ✓
- **Step 4** (`make ci` dependency): `lint-baseline-check` added to the `ci` target
  after `lint-go`. Comment updated. ✓
- **Step 5** (branch-protection docs): `docs/contributing/lint-baseline.md` updated
  with cap-check mechanics, counting assumption, and "Branch protection" section. ✓
- **Step 6** (validation): `make lint-baseline-check` exits 0 at 117/117; exits 1
  with the documented error message when synthetically grown to 118. `make ci` green.
  Reviewer independently verified all three checks. ✓
- **Reuse requirement**: Inspected `tools/lint-baseline/main.go` for `--count` mode;
  executor added it and used it in the Makefile target instead of `grep -c`. ✓
- **Tests in workstream plan**: `TestCountBaselineRules` and
  `TestCountBaselineRulesMissingFile` present and passing. ✓ (see test gap nits below)

#### Required Remediations

- **[BLOCKER] `tools/lint-baseline/cap.txt` must be committed to git.**
  `git status` reports `?? tools/lint-baseline/cap.txt`. Without this file in the
  repository, `make lint-baseline-check` (and therefore the CI `Lint` job) will fail
  with "cat: tools/lint-baseline/cap.txt: No such file or directory" on every checkout.
  The enforcement mechanism does not exist until this file is tracked.
  *Acceptance criteria*: `git ls-files tools/lint-baseline/cap.txt` returns
  `tools/lint-baseline/cap.txt`; `make lint-baseline-check` exits 0 immediately after
  a clean checkout on a fresh machine.

- **[NIT] `TestCountBaselineRules` is missing a count=0 subtest.**
  The test only validates counting 2 entries. Add a subtest (or table-driven case)
  that writes only the YAML header (`issues:\n  exclude-rules:\n`) and asserts the
  count is `0`. This guards against an off-by-one regression where every parse
  returns at least 1.
  *Acceptance criteria*: `go test ./tools/lint-baseline/...` includes a passing case
  that calls `countBaselineRules` on a header-only file and asserts `count == 0`.

- **[NIT] `TestCountBaselineRules` does not verify resistance to `- path:` in text values.**
  The `text:` field is regexp-quoted arbitrary content. A synthetic entry whose text
  starts with `- path:` (e.g., manually edited baseline) would inflate the count.
  Add one table-driven case: a single rule entry whose `text:` value is
  `'- path: internal/foo.go'`, and assert the count is `1`, not `2`.
  *Acceptance criteria*: test case present and passing; `countBaselineRules` returns
  the correct count when a `text:` field value begins with `- path:`.

- **[NIT] No validation that `cap.txt` contains a valid integer.**
  If `cap.txt` is accidentally set to a non-numeric value (e.g., whitespace, a comment),
  the shell arithmetic comparison `[ "$$count" -gt "$$cap" ]` fails with
  "integer expression expected" — a confusing error for contributors. Add a guard in
  the Makefile target after reading the cap:
  ```make
  if ! echo "$$cap" | grep -qE '^[0-9]+$$'; then \
      echo "ERROR: tools/lint-baseline/cap.txt must contain a single integer; got: $$cap"; \
      exit 1; \
  fi; \
  ```
  *Acceptance criteria*: `make lint-baseline-check` prints a clear error and exits 1
  when `cap.txt` contains non-numeric content.

#### Test Intent Assessment

**Strong**: `TestCountBaselineRules` (temp file, exact count), `TestCountBaselineRulesMissingFile`
(error on absent file). Existing pre-W02 tests (`TestGoldenRoundTrip`, `TestDeduplication`,
`TestEmptyInput`, `TestStableText`, `TestYAMLScalar`) remain solid.

**Weak**: No zero-entry baseline test; no text-field false-positive guard (see nits above).
The `make lint-baseline-check` integration is validated by manual steps in the workstream notes,
which is acceptable per the workstream's stated behavioral-verification approach.

#### Validation Performed

```
go test ./tools/lint-baseline/... -v -count=1   → PASS (8 tests)
make lint-baseline-check                         → "Lint baseline within cap (117 / 117)." (exit 0)
make lint-baseline-check (after synthetic +1)   → documented ERROR message (exit 1)
git checkout .golangci.baseline.yml; make lint-baseline-check → exit 0
make ci                                          → all gates green
make help | grep lint                            → lint-baseline-check listed correctly
git status tools/lint-baseline/cap.txt          → ?? tools/lint-baseline/cap.txt (UNTRACKED — blocker)
```

### Review 2026-04-29-02 — approved

#### Summary

All three nits and the blocker from the previous pass are fully resolved.
`tools/lint-baseline/cap.txt` is now staged (`A` in `git status`);
`git ls-files` confirms it is tracked. `TestCountBaselineRules` is now
table-driven with three cases: `multiple entries` (count=2), `header only`
(count=0), and `text value starts with path token` (count=1, proving no
false-positive inflation). The Makefile integer-validation guard produces the
expected clear error on non-numeric cap content. All exit criteria are met.
`make ci` is green. Approved for merge.

#### Plan Adherence

- **Step 1** (cap.txt): `git ls-files tools/lint-baseline/cap.txt` → `tools/lint-baseline/cap.txt`. ✓
- **Step 2** (lint-baseline-check target): Makefile target correct, `.PHONY` updated, `make help` lists target, integer-validation guard added. ✓
- **Step 3** (CI YAML step): `Lint baseline cap check` step present after `make lint-go`. ✓
- **Step 4** (`make ci` dependency): `lint-baseline-check` in dependency list after `lint-go`. ✓
- **Step 5** (branch-protection docs): Cap mechanics, counting assumption, branch-protection section all present. ✓
- **Step 6** (validation): Independently re-verified in this pass. ✓
- **Tests**: Table-driven `TestCountBaselineRules` (3 subtests), `TestCountBaselineRulesMissingFile`. All pass with `-race`. ✓

#### Test Intent Assessment

**Strong**: All three `TestCountBaselineRules` subtests map to distinct behavioral
invariants (normal count, zero count, no false-positive on text-field content).
`TestCountBaselineRulesMissingFile` confirms the error path. Pre-existing tests
unchanged and passing. Test suite is now regression-resistant against realistic
faults in `countBaselineRules`.

#### Validation Performed

```
git ls-files tools/lint-baseline/cap.txt          → tools/lint-baseline/cap.txt (tracked ✓)
go test ./tools/lint-baseline/... -v -race -count=1 → PASS (10 tests: 3 subtests in TestCountBaselineRules) ✓
make lint-baseline-check (cap=117, count=117)       → "Lint baseline within cap (117 / 117)." (exit 0) ✓
make lint-baseline-check (cap=not-a-number)         → clear integer-validation error (exit 1) ✓
make ci                                             → all gates green ✓
```
