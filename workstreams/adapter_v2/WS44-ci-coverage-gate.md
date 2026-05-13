# WS44 — CI coverage ratchet gate

**Phase:** Adapter v2 · **Track:** Post-release hardening · **Owner:** Workstream executor · **Depends on:** [WS40](WS40-v2-release-gate.md) (release gate must merge first so the captured floors reflect the post-rewrite package layout). · **Unblocks:** none.

> **Deferral note.** This workstream originated as the pre-Phase-4 `test-03-ci-coverage-gate.md`. It was deferred into adapter_v2 because applying a per-package coverage ratchet during a 43-workstream rewrite would create more friction than protection: WS37 deletes large amounts of v1 code (shifting package averages downward), WS30–WS36 add new code paths before tests catch up, and several new packages (sandbox, OCI cache, signing, lockfile, manifest) don't exist yet when the floors would be captured. Capturing the floor *after* WS40 means the contract reflects the steady-state codebase, not a transitional one.
>
> If interim regression protection is wanted during the rewrite, scope a much narrower variant: ratchet only on `workflow/` and any other package outside the adapter rework's blast radius. Track that separately — do not block this workstream on it.

## Context

`make test-cover` already produces coverage profiles ([Makefile:75-80](../../Makefile#L75-L80)) but **CI does not gate on them**. Coverage can silently regress on any merge. The adapter v2 rework refactors large amounts of code; this workstream lands *after* that work to lock in the new steady-state floor.

This workstream establishes a **per-package coverage ratchet**:

- Capture the current coverage percentage for each load-bearing package after WS40 lands.
- Store the per-package floors in `tools/coverage-floors.txt` (one line per package).
- Add a CI step that runs `go test -coverprofile`, parses the output, and fails if any package's coverage falls below its floor.
- The floor only ever rises: a workstream that pushes coverage up MUST update the floor in the same PR. A workstream that legitimately reduces coverage (e.g. by removing dead code) MUST drop the floor with a documented reason in reviewer notes.

This is not a "minimum percentage" gate. It is a **non-regression** gate. The current numbers become the new contract; future work can raise but not lower without justification.

## Prerequisites

- [WS40](WS40-v2-release-gate.md) merged — adapter v2 release-gate roll-up has shipped and the package layout is stable.
- [test-02-hcl-parsing-eval-coverage.md](../test-02-hcl-parsing-eval-coverage.md) merged (independent of adapter v2; raises `workflow/` coverage before the floor is captured).
- `make ci` green on `main`.
- `make test-cover` produces a usable `cover.out` (verify before starting):
  ```sh
  make test-cover && wc -l cover.out
  ```

## In scope

### Step 1 — Capture the per-package coverage floors

Run `make test-cover` against `main` (after WS40 and test-02 have landed). Collect per-package coverage:

```sh
go test -race -coverprofile=cover.out -covermode=atomic ./...
go tool cover -func=cover.out | awk '
  /\.go:/ {
    # Extract package: strip the file:line:func part, keep the dir
    n = split($1, parts, "/")
    pkg = parts[1]
    for (i=2; i<n; i++) pkg = pkg "/" parts[i]
    cov = $NF
    sub(/%/, "", cov)
    sum[pkg] += cov
    cnt[pkg]++
  }
  END {
    for (p in sum) printf "%s %.1f\n", p, sum[p]/cnt[p]
  }
' | sort > /tmp/coverage-floors.txt
```

(The exact awk is illustrative — pick whichever parser is robust against the actual `go tool cover -func` output format. The output of `go tool cover -func=cover.out` has lines like `github.com/brokenbots/criteria/workflow/eval.go:489:	SerializeVarScope	95.5%` — the goal is to aggregate per-package, not per-function.)

The captured `tools/coverage-floors.txt` has the format:

```
internal/adapter/conformance 87.3
internal/adapters/shell 81.2
internal/cli 72.4
internal/engine 79.1
internal/plugin 76.8
internal/run 84.0
internal/transport/server 70.5
sdk/conformance 88.1
workflow 85.7
```

Round each percentage **down** to the nearest 0.5 to leave a tiny buffer for measurement noise (e.g. 87.34 → 87.0, 87.55 → 87.5). This avoids per-CI-run flake from coverage tool jitter.

Selectivity: **only include packages with ≥ 100 statements measured**. Tiny packages are noisy and not load-bearing. Use `go tool cover -func=cover.out | grep -c <pkg>` to gauge; any package with < 20 entries is skipped.

Commit `tools/coverage-floors.txt` exactly as captured.

### Step 2 — Write the coverage-check script

New file: `tools/coverage-check.sh`. Posix-compliant bash, reads `tools/coverage-floors.txt`, runs `go test -coverprofile`, parses the output, asserts each listed package meets or exceeds its floor.

```bash
#!/usr/bin/env bash
set -euo pipefail

FLOORS_FILE="${FLOORS_FILE:-tools/coverage-floors.txt}"
COVER_FILE="${COVER_FILE:-cover.out}"

if [[ ! -f "$COVER_FILE" ]]; then
    echo "ERROR: $COVER_FILE not found. Run 'make test-cover' first."
    exit 2
fi

# Build per-package actual coverage map.
declare -A actual
while IFS= read -r line; do
    # Parse `go tool cover -func` output: <path>:<line>:<func> ... <pct>%
    # Extract the package (strip file basename and module prefix), aggregate.
    file=$(echo "$line" | awk '{print $1}' | cut -d: -f1)
    pct=$(echo "$line" | awk '{print $NF}' | tr -d '%')
    pkg=$(echo "$file" | sed 's|^github.com/brokenbots/criteria/||' | xargs dirname)
    # Skip the "total" line and any non-percentage line
    if [[ ! "$pct" =~ ^[0-9.]+$ ]]; then continue; fi
    actual[$pkg]+="$pct "
done < <(go tool cover -func="$COVER_FILE")

# Compute average per package
declare -A avg
for pkg in "${!actual[@]}"; do
    sum=0; n=0
    for v in ${actual[$pkg]}; do
        sum=$(echo "$sum + $v" | bc -l)
        n=$((n+1))
    done
    if [[ $n -gt 0 ]]; then
        avg[$pkg]=$(echo "scale=1; $sum / $n" | bc -l)
    fi
done

# Compare against floors.
fail=0
while IFS=' ' read -r pkg floor; do
    [[ -z "$pkg" || "$pkg" == \#* ]] && continue
    a="${avg[$pkg]:-}"
    if [[ -z "$a" ]]; then
        echo "FAIL: package $pkg has no coverage data (floor: $floor%)"
        fail=1
        continue
    fi
    # Use bc for comparison
    drop=$(echo "$a < $floor" | bc -l)
    if [[ "$drop" == "1" ]]; then
        echo "FAIL: package $pkg coverage $a% < floor $floor%"
        fail=1
    else
        echo "OK:   package $pkg coverage $a% >= floor $floor%"
    fi
done < "$FLOORS_FILE"

if [[ $fail -eq 1 ]]; then
    echo
    echo "Coverage regressed. Either:"
    echo "  1. Add tests so coverage rises again, OR"
    echo "  2. If the regression is intentional (e.g. removed dead code), edit"
    echo "     $FLOORS_FILE to lower the floor and document the reason in PR review."
    exit 1
fi
exit 0
```

The script is intentionally simple — bash + `bc` + `awk`. No new tool dependency. Document the bash + bc requirements in the script header.

If bash + bc is too fragile, port to a tiny Go program at `tools/coverage-check/main.go` instead — same logic, different language. Pick whichever the executor finds more robust; both are acceptable.

### Step 3 — Add Makefile target

Extend [Makefile](../../Makefile):

```make
.PHONY: coverage-check
coverage-check: test-cover
	bash tools/coverage-check.sh
```

This target runs `make test-cover` first (the dependency) so `cover.out` exists. Local invocation:

```sh
make coverage-check
```

### Step 4 — Add CI step

Extend [.github/workflows/ci.yml](../../.github/workflows/ci.yml). Add a new top-level job (the existing `unit-tests` job already runs tests; this job runs them again with coverage and gates on the floor):

```yaml
  coverage-check:
    name: Coverage ratchet
    runs-on: ubuntu-latest
    needs: unit-tests
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Cache Go build cache
        uses: actions/cache@v4
        with:
          path: ~/.cache/go-build
          key: go-build-cover-${{ runner.os }}-${{ hashFiles('**/go.sum') }}
          restore-keys: |
            go-build-cover-${{ runner.os }}-

      - name: Sync workspace
        run: go work sync

      - name: Run tests with coverage
        run: make test-cover

      - name: Enforce coverage floor
        run: bash tools/coverage-check.sh
```

The job runs after `unit-tests` (so a test failure surfaces first, not a coverage failure on a broken build). It is gated by the `needs: unit-tests` dependency.

If the existing CI structure prefers a single job, append coverage-check as a final step under `unit-tests` instead. Choose whichever fits the existing structure; document the choice in reviewer notes.

### Step 5 — Document the ratchet workflow

Append a new section to [docs/contributing/your-first-pr.md](../../docs/contributing/your-first-pr.md):

```markdown
## Coverage ratchet

CI enforces per-package coverage floors stored in [`tools/coverage-floors.txt`](../../tools/coverage-floors.txt). If your PR drops coverage for a listed package, CI fails.

Two options:

1. **Add tests.** Most regressions are accidental. Run `make coverage-check` locally, identify the regressed package, and add tests until the floor is met.
2. **Drop the floor.** If the regression is intentional (e.g. you removed a function that had high coverage and the package average shifts down), edit `tools/coverage-floors.txt` and lower the floor for that package. Justify in PR review.

The floor only ever ratchets up over time. PRs that raise coverage are encouraged to also raise the floor.
```

### Step 6 — Validation

```sh
make test-cover
make coverage-check       # exit 0 expected
# Manually break: temporarily comment out a few lines of test, re-run:
make coverage-check       # exit 1 expected with package listed
# Revert the temporary break.
make coverage-check       # exit 0 again
make ci                   # exit 0 expected
```

Document in reviewer notes:

- The exact contents of `tools/coverage-floors.txt` as committed.
- The output of `make coverage-check` on a clean tree (proves the floors are achievable on the workstream's HEAD).
- The output of `make coverage-check` after a temporary regression (proves the script catches it).

## Behavior change

**No behavior change.** This workstream adds a CI check, a Makefile target, a script, and a data file. No source code is modified. No test is added or removed. Coverage measurement is the only new artifact, and it does not affect runtime behavior.

The CI gate is **strict** — a regression below floor fails the build. This is a behavior change for **CI**, not for the product. PRs that drop coverage will fail CI starting the moment this workstream merges.

## Reuse

- Existing `make test-cover` target.
- `go tool cover -func` output format.
- Standard bash + bc OR small Go program for the check script — pick one.
- Existing CI job structure in [.github/workflows/ci.yml](../../.github/workflows/ci.yml) — extend.

## Out of scope

- A coverage badge on the README. Not in scope.
- A web-rendered coverage report (codecov, coveralls). Not in scope.
- Increasing coverage in any package. The floor is the **current** number; raising coverage is feature-workstream territory (test-02 raised the `workflow/` numbers; WS26 raised the adapter conformance surface).
- Per-file or per-function coverage gates. Per-package is the right granularity.
- Coverage gates on specific functions (e.g. `mergeSpecs` ≥ 90%). test-02 already locks those numbers in via its tests; the per-package gate inherits them.
- Including `cmd/criteria-adapter-*/` packages in the floor. External adapter binaries have low statement counts and high noise; rely on conformance tests instead.
- Excluding generated proto files from coverage measurement. They drag down package averages slightly; the floor accommodates.

## Files this workstream may modify

- New file: [`tools/coverage-floors.txt`](../../tools/) — the per-package floor data.
- New file: [`tools/coverage-check.sh`](../../tools/) — the gate script. (OR `tools/coverage-check/main.go` if Go is preferred.)
- [`Makefile`](../../Makefile) — add `coverage-check` target.
- [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) — add the coverage-check job (or step under `unit-tests`).
- [`docs/contributing/your-first-pr.md`](../../docs/contributing/your-first-pr.md) — append the ratchet workflow section per Step 5.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file under `workflow/`, `internal/`, `cmd/`, `sdk/`.
- Generated proto files.
- [`.golangci.yml`](../../.golangci.yml), [`.golangci.baseline.yml`](../../.golangci.baseline.yml).

## Tasks

- [ ] Run `make test-cover` on the post-WS40 + test-02 tree; capture per-package floors with rounding (Step 1).
- [ ] Commit `tools/coverage-floors.txt` (Step 1).
- [ ] Write `tools/coverage-check.sh` per Step 2 (or Go equivalent at `tools/coverage-check/main.go`).
- [ ] Add `coverage-check` Makefile target (Step 3).
- [ ] Add CI job/step (Step 4).
- [ ] Document the workflow in `docs/contributing/your-first-pr.md` (Step 5).
- [ ] Validation including the deliberate-regression demo (Step 6).

## Exit criteria

- `tools/coverage-floors.txt` exists with one line per qualifying package (≥ 100 statements measured), rounded down to nearest 0.5%.
- `make coverage-check` exits 0 on a clean tree.
- `make coverage-check` exits 1 if any package's coverage drops below its floor (demonstrated and reverted during validation).
- CI runs the coverage-check job and gates on it.
- `docs/contributing/your-first-pr.md` documents the ratchet workflow.
- `make ci` exits 0.

## Tests

This workstream is CI-infrastructure and a script. No unit tests added.

If the script is implemented in Go (`tools/coverage-check/main.go`), add unit tests for its parser logic:

- `TestParseCoverFunc_HappyPath` — parse a synthetic `go tool cover -func` output, assert per-package averages match.
- `TestParseCoverFunc_MissingPackage` — floor file lists a package not present in cover output; assert error.
- `TestParseCoverFunc_BelowFloor` — actual < floor; assert exit 1 and the package name in the output.
- `TestParseCoverFunc_AboveFloor` — actual > floor; assert exit 0.

If the script is bash, no unit tests — manual validation per Step 6 is the lock-in.

## Risks

| Risk | Mitigation |
|---|---|
| Coverage measurement varies across Go minor versions, causing floor flakes | Round floors down to 0.5%. Pin the Go version in CI (`go-version-file: go.mod`). If flakes appear, raise the rounding granularity to 1.0%. |
| Test parallelism (`-race -count=2`) causes coverage atom counters to undercount in rare interleavings | Use `-covermode=atomic` (already set in `make test-cover`). If undercount appears, bump rounding granularity. |
| The 0.5% rounding leaves no headroom and a one-statement test removal trips the floor | The 0.5% buffer is intentionally tight. If a routine refactor trips the floor, that is a signal to update the floor — that's the workflow. The Step 5 doc explains. |
| Bash script is brittle on macOS vs Linux (different `bc` / `awk` versions) | Test on both before commit. If brittleness shows, port to Go (`tools/coverage-check/main.go`). |
| The floor data file becomes a merge-conflict hotspot when multiple PRs raise coverage simultaneously | Conflicts in `tools/coverage-floors.txt` resolve by taking the higher floor for each package. Document this in the Step 5 doc as a one-line note. |
| Excluding `cmd/criteria-adapter-*/` packages misses regressions there | The conformance suite ([WS26](WS26-conformance-harness.md)) is the gate for adapters, not coverage. Coverage of `cmd/` packages is a weak signal — the conformance contract is the strong signal. |
| The new CI job adds 2-3 minutes to PR CI time | `make test-cover` was already runnable; only the coverage-check parsing is new (< 5s). The bulk is the test run, which is the same cost as the existing `unit-tests` job. Run the coverage check in parallel where possible (it can use the cover output from `unit-tests` if cached). |
