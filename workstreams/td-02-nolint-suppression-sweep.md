# td-02 — Inline `nolint` suppression sweep

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** B (tech debt) · **Owner:** Workstream executor · **Depends on:** [td-01-lint-baseline-ratchet.md](td-01-lint-baseline-ratchet.md) (run after td-01 lands so the baseline is at the new lower count and this sweep doesn't conflict with the cap drop). · **Unblocks:** [td-03-staticcheck-deprecated-enum.md](td-03-staticcheck-deprecated-enum.md) (the 4 staticcheck suppressions in copilot_permission.go addressed there are also part of this audit; td-03 carves them out as a focused sub-workstream).

## Context

There are **66 inline `//nolint:` directives** scattered across the Go source tree. They were added during Phase-2/3 rework to keep CI green while broader cleanups were pending. Each directive is a small unpaid tax: it hides whatever the linter would otherwise say, and the cost is paid every time someone reads the surrounding code and has to ask "is this still needed?".

This workstream is a **systematic audit** of all 66. For each directive, the executor decides one of three outcomes:

1. **Fix the underlying issue** (preferred when cheap) — refactor or rewrite so the linter no longer fires; remove the directive.
2. **Move to baseline** with a documented `# kept:` reason — when the suppression is correct but inline noise is worse than baseline-file noise.
3. **Keep inline** with a tightened explanation — when the directive is the right place because the suppression is local and the reason is genuinely about a single line/expression (not a whole function).

Outcomes 1 and 2 are preferred. Outcome 3 is the exception, not the rule. The contract is: **every surviving inline directive has a one-sentence rationale that names the specific local reason.**

The 66 directives by rule (snapshot from the Phase-3 close — re-snapshot in Step 1 to confirm):

| Rule | Count | Notes |
|---|---:|---|
| `gocritic` | 23 | Mostly W15 (Options pass-by-value in conformance tests) |
| `funlen` | 16 | Mostly W03/W04 carryover |
| `funlen,gocognit,gocyclo` | 5 | Multi-rule deferrals on workflow compile functions |
| `staticcheck` | 4 | **Deprecated enum, owned by [td-03](td-03-staticcheck-deprecated-enum.md)** — exclude from this workstream |
| `gocognit` | 3 | Carryover |
| `funlen,gocyclo` | 3 | Carryover |
| `funlen,gocognit` | 3 | HCL eval / variable scope serialization |
| `nilerr` | 2 | Returns nil after timeout (intentional) |
| `revive` | 2 | Proto-generated wire-compatibility names |
| `gocognit,gocyclo` | 1 | Type switch covering all envelope types |
| `err113` | 1 | Fully contextual error message (no %w wrap needed) |
| `cyclop,gocognit,gocyclo,funlen` | 1 | Multi-field merge with conflict detection |
| **Total** | **66** | |

After excluding the 4 staticcheck (owned by td-03), this workstream audits **62 directives**.

**Target:** drop from 62 to **≤ 35 inline directives**, with every surviving directive carrying a one-sentence rationale that names the specific local reason. Removed directives either become baseline entries (with `# kept:` reasons) or are eliminated by fixing the underlying issue.

## Prerequisites

- [td-01-lint-baseline-ratchet.md](td-01-lint-baseline-ratchet.md) merged. `tools/lint-baseline/cap.txt` reads `16`. `make lint-baseline-check` is green.
- `make ci` green on `main`.
- `golangci-lint` installed at the version `make lint-go` invokes.

## In scope

### Step 1 — Snapshot the 62 directives

From repo root, generate the work-list:

```sh
grep -rn '//nolint' . --include='*.go' \
  | grep -v 'staticcheck' \
  | grep -v '^./vendor/' \
  | grep -v '/testdata/' \
  > /tmp/td-02-worklist.txt

wc -l /tmp/td-02-worklist.txt   # expect: 62
```

(The 4 `staticcheck` directives are owned by td-03 and excluded here. If the count is not exactly 62, re-snapshot and reconcile against the Context table — the count may have drifted up or down from the Phase 3 close.)

Commit `/tmp/td-02-worklist.txt` content into reviewer notes (paste the file:line:directive list verbatim) so the reviewer can see the starting state. The list does NOT go into the repo — it is a working artifact.

### Step 2 — Categorise each directive

For each line in the work-list, read the surrounding 20 lines of context. Categorise into one of these buckets:

- **A. Fixable now** (target: ≥ 20 directives). The underlying issue is a small refactor: extract a helper, add a doc-comment, rename a variable, use `errors.Is`/`errors.As` instead of swallowing. Example: a `funlen` directive on a 55-line function where ~10 lines are easily extractable into a clearly-named helper.
- **B. Move to baseline** (target: ≥ 7 directives). The suppression is correct, the underlying complexity is structural (e.g. a state machine that is genuinely a state machine), and inline noise is worse than baseline-file noise. The `# kept:` reason in the baseline file replaces the inline comment.
- **C. Keep inline, tighten rationale** (target: ≤ 35 directives). The suppression is local to a single statement (typical: `nilerr` on a deliberate `return nil`, `err113` on a fully-contextual `fmt.Errorf` that doesn't wrap). Tighten the inline comment so the reason is one sentence and names the specific local cause.
- **D. Owned by td-03** (4 directives). Skip — the staticcheck deprecated-enum suppressions in `cmd/criteria-adapter-copilot/copilot_permission.go` are td-03's territory.

Produce a categorisation table in reviewer notes:

```markdown
| File:line | Rule(s) | Category | Plan |
|---|---|---|---|
| internal/adapter/conformance/conformance.go:42 | gocritic | A | Convert Options pass-by-value to *Options. |
| internal/adapter/conformance/conformance_lifecycle.go:88 | gocritic | B | Pass-by-value of test Options is API-shaped; move to baseline with kept reason. |
| ... | | | |
```

The categorisation is the load-bearing artifact of this workstream. The reviewer signs off on the plan before any code changes.

### Step 3 — Execute Category A fixes (target ≥ 20)

For each Category A directive:

1. Fix the underlying issue. Common patterns:
   - **`funlen`**: extract a self-explanatory helper. Helper name should be a verb phrase that reads as a sentence at the call site.
   - **`gocritic` hugeParam**: convert pass-by-value to `*Options` (or whichever struct). Update all call sites.
   - **`gocritic` rangeValCopy**: convert `for _, v := range ...` to indexed iteration.
   - **`gocognit`/`gocyclo`**: extract a helper or replace nested ifs with a switch / early returns.
   - **`nilerr`**: rewrite the control flow so the deliberate-nil case is explicit (e.g. `return errTimeout` then handle `errors.Is(err, errTimeout) { return nil }` at the caller).
   - **`err113`**: wrap or define a sentinel error if the call site needs to distinguish; otherwise document why a contextual error is correct (Category C).
2. Remove the inline directive.
3. Run `make lint-go` and confirm the rule no longer fires for that file:line. If a different rule now fires, that is in scope: fix it or escalate to Category B/C.
4. Run any tests for the touched file: `go test ./<package>/...`. Add a test if the refactor exposes a regression.

Cap on file churn per Category A fix: ≤ 100 lines added/removed per directive (excluding test additions). If a fix would exceed that cap, escalate to Category B (move to baseline; the underlying refactor belongs in a dedicated workstream).

### Step 4 — Execute Category B moves (target ≥ 7)

For each Category B directive:

1. Identify the rule(s) being suppressed.
2. Add a baseline entry to `.golangci.baseline.yml` matching the file path, linter(s), and a regex tight enough to match only the intended occurrence (use the function name or a unique substring — never a wildcard that would silence future findings).
3. Add a single-line comment above the entry: `# kept: <one-sentence reason naming the structural cause and why inline suppression is worse>`.
4. Remove the inline directive.
5. Run `make lint-go` (still green) and `make lint-baseline-check`. The cap may need to rise from 16 to (16 + N moved entries). Update `tools/lint-baseline/cap.txt` accordingly. **The cap rise is the legitimate cost of this trade-off** — document it explicitly in reviewer notes and in the lint-baseline doc per Step 6.

The cap MUST stay at the actual count exactly (no slack).

### Step 5 — Execute Category C tightening (≤ 35 survivors)

For each Category C directive:

1. Read the existing inline comment. Confirm it explains the local reason.
2. If the comment is generic (`// W15`, `// deferred`, `// see workstream X`), rewrite it to name the specific local cause. Format:
   ```go
   //nolint:<rule> // <one-sentence reason: what the code is doing and why the linter is wrong here>
   ```
   Examples:
   - Bad: `//nolint:nilerr // expected`
   - Good: `//nolint:nilerr // returning nil because the context.DeadlineExceeded result is the documented success signal — see comment above`
   - Bad: `//nolint:err113 // W15`
   - Good: `//nolint:err113 // dynamic error message contains the user-facing field name; sentinel-error wrap would lose context`
3. If the comment cannot be tightened to one local sentence, the directive belongs in Category A (fix the issue) or Category B (move to baseline).

After Step 5, **every surviving inline directive carries a tightened rationale**. Verify with:

```sh
grep -rn '//nolint' . --include='*.go' \
  | grep -v 'staticcheck' \
  | grep -v '^./vendor/' \
  | grep -v '/testdata/' \
  | wc -l
# expected: ≤ 35
```

### Step 6 — Update `docs/contributing/lint-baseline.md`

Append a new section after the td-01 section (which td-01 added):

```markdown
## td-02 (pre-Phase-4) — 2026-MM-DD

- **Starting inline directives:** 62 (excluding 4 staticcheck owned by td-03).
- **Final inline directives:** ≤ 35.
- **Baseline cap before:** 16. **After:** 16 + N moved entries.

### Removed inline directives by category

| Category | Count | Disposition |
|---|---:|---|
| A — fixed underlying issue | ≥ 20 | Refactor / extraction / pass-by-pointer / control-flow rewrite. |
| B — moved to baseline | ≥ 7 | `# kept:` rationale in `.golangci.baseline.yml`. |
| C — tightened rationale | ≤ 35 | Inline directive retained with one-sentence local reason. |

### Surviving Category C directives

(One-line table per surviving directive: file:line, rule, one-sentence reason.)
```

### Step 7 — Validation

```sh
make lint-go
make lint-baseline-check
go test -race -count=1 ./...
(cd sdk && go test -race -count=1 ./...)
(cd workflow && go test -race -count=1 ./...)
make ci
```

All six must exit 0. Inspect:

- `grep -rc '//nolint' --include='*.go' . | awk -F: '{s+=$2} END{print s}'` returns ≤ 35 (excluding staticcheck and vendor/testdata).
- `tools/lint-baseline/cap.txt` matches the actual baseline entry count.
- No directive remains with a generic comment like `// expected`, `// W15`, `// deferred`. Verify with:
  ```sh
  grep -rE '//nolint:.*// (expected|deferred|W[0-9]+|legacy)$' --include='*.go' . | wc -l
  # expected: 0
  ```

## Behavior change

**No behavior change.** Every fix is a refactor, a comment tightening, or a baseline relocation. No HCL surface change. No CLI flag change. No event/log change. No new error messages.

If a Category A fix exposes a real bug (e.g. a swallowed error that masked a regression), that bug is in scope. Fix it and add a regression test. Document the bug in reviewer notes. Do not revert the fix.

## Reuse

- Existing [`make lint-go`](../Makefile) / `make lint-baseline-check` targets.
- Baseline tooling at [tools/lint-baseline/](../tools/lint-baseline/).
- The `# kept:` annotation convention from [archived/v3/01-lint-baseline-burndown.md](archived/v3/01-lint-baseline-burndown.md).
- The Category A/B/C triage pattern from [archived/v2/16-phase2-cleanup-gate.md](archived/v2/16-phase2-cleanup-gate.md).
- Existing burn-down doc structure in [docs/contributing/lint-baseline.md](../docs/contributing/lint-baseline.md).

## Out of scope

- The 4 `staticcheck` deprecated-enum directives in `cmd/criteria-adapter-copilot/copilot_permission.go`. Owned by [td-03-staticcheck-deprecated-enum.md](td-03-staticcheck-deprecated-enum.md).
- The W10 / W12 baseline entries that td-01 left intact. Same out-of-scope reasoning as td-01.
- Adding new linter rules to [.golangci.yml](../.golangci.yml).
- Changing the linter version pin in the `Makefile` `lint-go` target.
- Files under `vendor/`, `*/testdata/`, or generated proto files.
- Eliminating the `funlen,gocognit,gocyclo` cluster on `compileSteps` and similar deeply structural functions — those should land in Category B (moved to baseline with structural rationale), not Category A. The W04 split rework is closed; further extraction risk-reward is poor.

## Files this workstream may modify

- Any non-generated `*.go` file containing an inline `//nolint:` directive (other than staticcheck deferred to td-03), and any file that needs signature updates as a downstream consequence of a Category A fix.
- [`.golangci.baseline.yml`](../.golangci.baseline.yml) — add Category B entries; update the cap as the count grows.
- [`tools/lint-baseline/cap.txt`](../tools/lint-baseline/cap.txt) — update to the new exact count after Category B moves.
- [`docs/contributing/lint-baseline.md`](../docs/contributing/lint-baseline.md) — append the new td-02 section per Step 6.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Generated proto files.
- Files under `vendor/` or `*/testdata/`.
- The 4 staticcheck directives in `cmd/criteria-adapter-copilot/copilot_permission.go`.
- [`.golangci.yml`](../.golangci.yml) — rule configuration is immutable here.

## Tasks

- [ ] Snapshot the 62 directives and produce the categorisation table (Step 1, Step 2).
- [ ] Reviewer signs off on the categorisation plan before any code changes.
- [ ] Execute ≥ 20 Category A fixes (Step 3).
- [ ] Execute ≥ 7 Category B moves with `# kept:` reasons (Step 4).
- [ ] Tighten Category C rationales (Step 5).
- [ ] Update `docs/contributing/lint-baseline.md` (Step 6).
- [ ] Validation (Step 7).

## Exit criteria

- Inline `//nolint` count ≤ 35 (excluding staticcheck and vendor/testdata).
- Every surviving inline directive carries a one-sentence local rationale (no generic `// W15` / `// expected` / `// deferred` comments remain).
- `tools/lint-baseline/cap.txt` matches the actual baseline entry count exactly.
- `make lint-go` exits 0.
- `make lint-baseline-check` exits 0.
- `go test -race -count=1` exits 0 across root, `sdk/`, `workflow/`.
- `make ci` exits 0.
- `docs/contributing/lint-baseline.md` contains the new td-02 section.

## Tests

This workstream is "no behavior change." The existing test suite is the lock-in.

For each Category A fix, run the tests for the touched package and confirm green. If a refactor exposes a real regression, add a focused unit test that would have caught it.

For Category C, no tests are added — the change is comment-only.

For Category B, no tests are added — the directive moves but the suppression is the same.

## Risks

| Risk | Mitigation |
|---|---|
| The categorisation in Step 2 is wrong and a Category A fix is harder than the 100-line cap allows | The cap is the safety valve — escalate to Category B (move to baseline). The cleanup is incremental; not every directive must be fixed. |
| A Category A fix inadvertently changes behavior (e.g. a refactor reorders error returns) | Run package tests after each fix. If a test fails, the fix changed behavior and must be reverted or the test added. |
| Cap rises significantly because many directives go to Category B | The cap rise is documented explicitly in the lint-baseline doc with one-sentence rationale per moved entry. The reviewer judges acceptability. |
| A surviving Category C directive's tightened rationale is still too generic | The reviewer flags it; the executor either rewrites or moves to Category B. |
| The 62 starting count drifts by the time the workstream runs (someone adds a new directive) | Re-snapshot in Step 1 and adjust the targets proportionally. The contract is "≤ 35 survivors", not "exactly 27 removed". |
| A Category A fix breaks a downstream consumer of an unexported function the executor didn't realize was important | Search for cross-package references before changing exported-looking-but-unexported helpers. If unsure, escalate to Category B. |
