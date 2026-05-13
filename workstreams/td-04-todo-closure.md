# td-04 — Close the 5 outstanding TODO comments

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** B (tech debt) · **Owner:** Workstream executor · **Depends on:** none. · **Unblocks:** none.

## Context

`grep -rn 'TODO\|FIXME\|XXX' --include='*.go'` (excluding `vendor/` and `testdata/`) finds exactly **5 TODO comments** in the tree:

| # | Location | Comment | Original workstream |
|---|---|---|---|
| 1 | [internal/transport/server/client_test.go:866](../internal/transport/server/client_test.go#L866) | `// TODO: reject http:// at construction time in a follow-up workstream.` | (none cited) |
| 2 | [internal/transport/server/client_test.go:876](../internal/transport/server/client_test.go#L876) | `// TODO: reject http:// at construction time in a follow-up workstream.` | (none cited) |
| 3 | [internal/cli/plan.go:122](../internal/cli/plan.go#L122) | `// TODO(W10): render branch nodes in plan output for human review.` | W10 (Phase 1) |
| 4 | [internal/engine/node.go:48](../internal/engine/node.go#L48) | `// TODO(1.6): parallelNode would call deps.BranchScheduler.Run(...).` | v1.6 (legacy version reference) |
| 5 | [workflow/schema.go:133](../workflow/schema.go#L133) | `// TODO(W04): replace Remain decode with hcl.EvalContext for expression interpolation.` | W04 (Phase 3) |

These are all the outstanding TODO markers in the codebase (apart from the test data and vendor directories which are excluded). This workstream closes each one with a concrete disposition: implement, delete, or replace with a documenting comment that does not contain the word "TODO".

Each TODO is small but has accumulated for a different reason. The pattern matters: phases close cleanly when their TODOs are also closed, and phase-4 (adapter rework) should not inherit any of these. This workstream is the final pre-rework cleanup of the TODO surface.

## Prerequisites

- `make ci` green on `main`.
- The 5 TODO markers are still present. Verify:
  ```sh
  grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v vendor | grep -v testdata
  ```
  Expected: exactly 5 lines matching the table above. If the count differs, re-snapshot in reviewer notes and adjust the workstream's targets — but the goal is the same: zero TODO markers remain at exit.

## In scope

### Step 1 — Close TODOs #1 and #2: `http://` rejection in `NewClient`

**Decision: implement the rejection.**

The two paired TODOs in `internal/transport/server/client_test.go` document that `NewClient("http://...", log, Options{TLSMode: TLSEnable})` and the `TLSMutual` variant accept an http URL at construction even though the configured TLS mode is inconsistent. The mismatch surfaces later when RPCs are attempted, which is hostile to debuggability.

1. Locate `NewClient` in [internal/transport/server/client.go](../internal/transport/server/client.go).
2. Add an early validation:
   ```go
   func NewClient(target string, log *slog.Logger, opts Options) (*Client, error) {
       u, err := url.Parse(target)
       if err != nil {
           return nil, fmt.Errorf("server: invalid URL %q: %w", target, err)
       }
       if (opts.TLSMode == TLSEnable || opts.TLSMode == TLSMutual) && u.Scheme == "http" {
           return nil, fmt.Errorf("server: TLS mode %q requires an https:// URL; got %q", opts.TLSMode, target)
       }
       // ... existing body
   }
   ```
   Use the actual constant names and signature from the current code (verify before editing). The error message MUST name both the TLS mode and the offending URL — debuggability is the point.
3. Replace the two TODO comments with positive-assertion subtests:
   ```go
   t.Run("tls_enable_with_http_url_rejected", func(t *testing.T) {
       if _, err := NewClient("http://example.com", log, Options{TLSMode: TLSEnable}); err == nil {
           t.Fatal("expected error for TLSEnable + http URL; got nil")
       }
   })
   t.Run("tls_mutual_with_http_url_rejected", func(t *testing.T) {
       certFile, keyFile := writeTempCertKey(t)
       if _, err := NewClient("http://example.com", log, Options{TLSMode: TLSMutual, CertFile: certFile, KeyFile: keyFile}); err == nil {
           t.Fatal("expected error for TLSMutual + http URL; got nil")
       }
   })
   ```
   These tests replace the existing accepting tests (lines 862–887). The old behavior was documented; the new behavior is enforced.
4. Search the rest of the repo for callers that pass `http://` with `TLSEnable` or `TLSMutual`. There should be none — if any are found, fix them or document them as out of scope and revert this step.

This is a **behavior change**: a previously-accepting construction now errors. Document it explicitly in this workstream's Behavior-change section, in the CHANGELOG (no — `CHANGELOG.md` is off-limits; the project's release process will pick this up via PR title/labels), and in the reviewer notes.

### Step 2 — Close TODO #3: branch node rendering in `criteria plan`

**Decision: delete the TODO and update the surrounding documentation.**

The W10-era TODO at `internal/cli/plan.go:122` predates the W16 switch-and-if-flow-control workstream. Phase 3 W16 closed the `branch` block entirely (replaced by `switch`); there are no longer "branch nodes" to render.

`switch` nodes ARE already rendered by the `criteria plan` output (verify by reading the surrounding code at lines 100–135 — if `switches` are not rendered, that is a real omission and the in-scope fix is to add a `switches:` section to the plan output matching the existing `states:` section format).

1. Read `internal/cli/plan.go` from line 90 to line 140 to confirm the current shape of the plan output.
2. If `switch` nodes are already rendered: delete line 122 (and the surrounding blank line if it becomes a double-blank). No replacement comment.
3. If `switch` nodes are NOT yet rendered: add a `switches:` block to the plan output between `states:` and `plugins required:`. Format mirrors `states:`:
   ```
   switches:
     <name>    conditions=<N>   default=<target>
   ```
   Then delete line 122.
4. Run `criteria plan examples/phase3-marquee/` (or any example that contains a `switch`) and confirm the output renders the switch.
5. If a test asserts the plan output (likely in `internal/cli/plan_test.go` or a golden-file test), update the golden file to include the new `switches:` block.

### Step 3 — Close TODO #4: stale `parallelNode` comment in `node.go:48`

**Decision: delete the TODO.**

The comment at `internal/engine/node.go:48` references a `1.6` version (legacy schema) and a `BranchScheduler.Run` design that was never adopted. Phase 3 W19 (parallel step modifier) shipped parallelism via a different mechanism (`runParallelIteration` in `internal/engine/parallel_iteration.go`, with bounded fan-out via a semaphore — see [archived/v3/19-parallel-step-modifier.md](archived/v3/19-parallel-step-modifier.md)). The TODO is obsolete.

1. Delete line 48 of `internal/engine/node.go`.
2. If the surrounding control flow (lines 39–53) becomes hard to read after the deletion (e.g. a now-orphaned blank line between two `if` clauses), reformat for readability — but do not change behavior.
3. No replacement comment.

### Step 4 — Close TODO #5: `InputSpec.Remain` decode rework

**Decision: delete the TODO and update the type's doc-comment to describe current behavior.**

The TODO at `workflow/schema.go:133` references W04 (Phase 3). W04 (`split compile-steps`) closed; the Phase-3 closure shipped expression-aware decoding for `step.input { ... }` via `ResolveInputExprs` and `ResolveInputExprsAsCty` in [workflow/eval.go](../workflow/eval.go). The TODO is stale — the work it describes was completed by a different mechanism.

1. Read `workflow/schema.go` from line 128 to line 145 for context.
2. Replace lines 130–135 (the `InputSpec` block's leading comment and TODO line) with a comment describing **current** behavior:
   ```go
   // InputSpec holds the raw HCL body of a `step.input { ... }` block.
   // Attribute expressions are decoded by the compiler into a string map
   // (compile-time) and parallel hcl.Expression map (runtime).
   // Runtime evaluation uses ResolveInputExprs / ResolveInputExprsAsCty
   // in workflow/eval.go, which builds an hcl.EvalContext with var.*,
   // steps.*, local.*, shared.*, and each.* namespaces.
   type InputSpec struct {
       Remain hcl.Body `hcl:",remain"`
   }
   ```
3. The same stale "W04 will upgrade to expression-aware decoding" comment also appears on `ConfigSpec` at line 125 — update it to describe current behavior the same way. Verify before editing whether the same upgrade has shipped for `ConfigSpec` (look for `ResolveConfigExprs` or similar). If yes, update the comment. If no, leave the `ConfigSpec` comment alone (this workstream's scope is only TODO #5, which is `InputSpec`).

### Step 5 — Add a `grep` guard to CI

To prevent future TODO accumulation, add a CI step that fails the build if any `TODO` / `FIXME` / `XXX` marker appears in non-test, non-vendor Go source. Test files are allowed (test scaffolding occasionally needs them).

Add to [.github/workflows/ci.yml](../.github/workflows/ci.yml) under the existing `lint` job:

```yaml
- name: no-todo-markers-in-production-code
  run: |
    set -e
    if grep -rn 'TODO\|FIXME\|XXX' --include='*.go' \
        --exclude-dir=vendor --exclude-dir=testdata \
        cmd/ internal/ workflow/ sdk/ 2>&1 \
        | grep -v '_test\.go' \
        | grep -E .; then
      echo "::error::TODO/FIXME/XXX markers found in production code; close them or move to a workstream file."
      exit 1
    fi
```

The guard:
- Excludes `_test.go` files (test TODOs are tolerated; the previous Step 1 case is special because the TODOs documented production behavior).
- Excludes `vendor/` and `testdata/` directories.
- Searches only the four production-source top-level dirs (`cmd/`, `internal/`, `workflow/`, `sdk/`).
- Exits non-zero with a GitHub Actions-formatted error if any marker is found.

Add a corresponding `make` target for local use:

```make
.PHONY: lint-no-todos
lint-no-todos:
	@if grep -rn 'TODO\|FIXME\|XXX' --include='*.go' \
	    --exclude-dir=vendor --exclude-dir=testdata \
	    cmd/ internal/ workflow/ sdk/ 2>&1 \
	    | grep -v '_test\.go' \
	    | grep -E .; then \
	    echo "FAIL: TODO/FIXME/XXX markers found in production code"; \
	    exit 1; \
	fi
	@echo "OK: no TODO/FIXME/XXX markers in production code"

lint: lint-imports lint-go lint-baseline-check lint-no-todos
```

If [doc-03](doc-03-llm-language-spec.md) has already extended the `lint` target with `spec-check`, append `lint-no-todos` after `spec-check`.

### Step 6 — Validation

```sh
make lint-no-todos    # expect: OK (zero matches)
make lint
go test -race -count=1 ./internal/transport/server/...   # covers Step 1
go test -race -count=1 ./internal/cli/...                # covers Step 2
go test -race -count=1 ./internal/engine/...             # covers Step 3
go test -race -count=1 ./workflow/...                    # covers Step 4
make ci
```

All seven must exit 0. Inspect:

```sh
grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v vendor | grep -v testdata
# expected: zero matches (or only test-file matches if any test legitimately added a TODO that explains a test-only concern — none expected from this workstream)
```

## Behavior change

**Behavior change: yes — one observable difference.**

Step 1 (`http://` rejection in `NewClient`): a previously-accepting construction now errors. Specifically:

- `NewClient("http://...", log, Options{TLSMode: TLSEnable})` previously returned `(*Client, nil)`. Now returns `(nil, fmt.Errorf("server: TLS mode %q requires an https:// URL; got %q", ...))`.
- Same for `TLSMode: TLSMutual`.
- `TLSMode: TLSDisable` (or whatever the disabled-TLS constant is) with `http://` remains accepted — that combination is consistent.
- `TLSEnable` / `TLSMutual` with `https://` remain accepted — also consistent.

This is a behavior tightening, not a new feature. It changes the failure mode from "fail later, when RPCs are attempted" to "fail immediately, at construction". The error message is more diagnostic.

No other observable changes. Steps 2–4 are pure comment cleanups (Step 2 may add a `switches:` section to `criteria plan` output if it wasn't already present, but that is an additive doc improvement, not a contract change). Step 5 is CI infrastructure only.

## Reuse

- Existing `url.Parse` from the stdlib (already imported wherever URL handling lives).
- Existing TLS mode constants in [internal/transport/server/client.go](../internal/transport/server/client.go).
- The plan-output formatter pattern in [internal/cli/plan.go](../internal/cli/plan.go) — extend, do not rewrite.
- Existing CI workflow structure in [.github/workflows/ci.yml](../.github/workflows/ci.yml) — add steps under existing jobs.
- Existing `make lint` chain — extend, do not duplicate.

## Out of scope

- Changing the TLS modes themselves or adding new ones.
- Reworking `criteria plan` output beyond adding a `switches:` section if needed.
- Rewriting `ConfigSpec`'s decode path (only `InputSpec` doc-comment update is in scope — `ConfigSpec` comment update is allowed as a tagalong only if its identical stale TODO-style language must be edited to keep the file's tone consistent).
- The `parallelNode` scheduling code itself — only the stale TODO comment is deleted.
- Adding any new feature.
- Modifying `cmd/criteria-adapter-*/` files.
- Editing any file under `docs/` other than `docs/contributing/lint-baseline.md` if the workstream adds the `no-todo-markers-in-production-code` step (in which case the lint-baseline doc is amended with one sentence about the new CI step). Note: `docs/contributing/lint-baseline.md` edit is **optional** and only needed if the executor judges the cross-reference helpful.

## Files this workstream may modify

- [`internal/transport/server/client.go`](../internal/transport/server/client.go) — add http+TLS rejection in `NewClient`.
- [`internal/transport/server/client_test.go`](../internal/transport/server/client_test.go) — replace the two accepting subtests at lines 862–887 with rejecting subtests; delete the two TODOs.
- [`internal/cli/plan.go`](../internal/cli/plan.go) — delete the TODO at line 122; optionally add a `switches:` block to the plan output if not already present.
- [`internal/cli/plan_test.go`](../internal/cli/plan_test.go) (if it exists) — update golden output to include the new `switches:` block if Step 2 added one.
- [`internal/engine/node.go`](../internal/engine/node.go) — delete the TODO at line 48.
- [`workflow/schema.go`](../workflow/schema.go) — replace the TODO at line 133 with a current-behavior doc-comment; optionally update the parallel `ConfigSpec` comment at line 125.
- [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) — add the `no-todo-markers-in-production-code` step.
- [`Makefile`](../Makefile) — add the `lint-no-todos` target and append it to `lint`.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file under `cmd/criteria-adapter-*/`.
- Any file under `docs/` (except per the optional doc note in the Out-of-scope section, and only if strictly necessary).
- Generated proto files.
- [`.golangci.yml`](../.golangci.yml) or [`.golangci.baseline.yml`](../.golangci.baseline.yml).

## Tasks

- [x] Implement `http://` rejection in `NewClient` and update both subtests (Step 1).
- [x] Delete TODO #3 in `internal/cli/plan.go`; verify or add switch rendering (Step 2).
- [x] Delete TODO #4 in `internal/engine/node.go` (Step 3).
- [x] Replace TODO #5 in `workflow/schema.go` with current-behavior comment (Step 4).
- [x] Add `lint-no-todos` Makefile target and CI step (Step 5).
- [x] Validation (Step 6).

## Exit criteria

- `grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v vendor | grep -v testdata` returns zero matches.
- `make lint-no-todos` exits 0 on a clean tree.
- `make lint-no-todos` exits non-zero if a `TODO` is added to a non-test file under `cmd/`, `internal/`, `workflow/`, `sdk/`. (Demonstrate this once during development with a temporary TODO, then revert; no permanent test required.)
- `NewClient("http://example.com", log, Options{TLSMode: TLSEnable})` returns a non-nil error.
- `NewClient("http://example.com", log, Options{TLSMode: TLSMutual, ...})` returns a non-nil error.
- `NewClient("https://example.com", log, Options{TLSMode: TLSEnable})` returns a nil error (regression check).
- `criteria plan examples/phase3-marquee/` includes a `switches:` section (assuming the example contains a switch — verify before relying on it; otherwise use any example workflow that contains a switch block).
- `make ci` exits 0.

## Tests

- Step 1: two replacement subtests (`tls_enable_with_http_url_rejected`, `tls_mutual_with_http_url_rejected`). Plus a regression check that `https://` + `TLSEnable` still accepts.
- Step 2: if a golden-output test exists for `criteria plan`, update it to include the new `switches:` block. If no test exists, this workstream optionally adds a minimal one:
  ```go
  func TestPlan_RendersSwitchBlock(t *testing.T) {
      // compile a workflow that contains a `switch "router" { ... }` block
      // run plan; assert output contains `switches:` and the switch name
  }
  ```
  Use the existing compile helpers; this is one focused unit test.
- Steps 3 and 4: pure comment changes; no test required. Build cleanness is the lock-in.
- Step 5: the CI step is itself a test (the build fails if a TODO sneaks in). Confirm by temporarily adding a TODO to a non-test file and running `make lint-no-todos`; expect non-zero exit. Revert before commit.

## Risks

| Risk | Mitigation |
|---|---|
| Step 1 (`http://` rejection) breaks a downstream caller that was relying on the lax behavior | The construction was undocumented; existing callers should use `https://` with TLS modes. Search the repo before changing. If a legitimate caller exists, escalate as a follow-up. |
| Step 2 (switch rendering) changes a golden-file test in a way that surfaces as a "fail on rebase" hazard | Run `go test ./internal/cli/...` and update any golden files in the same commit. Don't leave the rendering and the golden in different commits. |
| The `lint-no-todos` CI step rejects a legitimate TODO that a future contributor adds in good faith | The error message tells them to either close the TODO or move it to a workstream file. The convention is clear; the guard is a forcing function. |
| The `lint-no-todos` grep is too restrictive and bans `TODO` from doc-comments that describe intentional design (e.g. "TODO callers must do X") | The pattern `TODO\|FIXME\|XXX` is intentionally aggressive. If a legitimate use needs the word `TODO`, the comment can rephrase (e.g. "Callers: do X" rather than "TODO callers: do X"). The guard is opinionated by design. |
| Step 4 (`InputSpec` comment) understates the current decode path and a reader thinks the comment is wrong | The new comment is required to be accurate. If unsure, read `workflow/eval.go` `ResolveInputExprs` and `ResolveInputExprsAsCty` before writing the new comment text. |

## Implementation notes (executor)

### Step 1 — Behavior change detail
`NewClient` now validates that TLS mode `TLSEnable`/`TLSMutual` is not combined with an `http://` URL. The validation fires after option defaults are resolved (so implicit TLS from `https://` is never incorrectly rejected). The option-resolution logic was extracted into a private `resolveOptions` helper to keep `NewClient` under the `funlen` limit.

One additional caller was discovered and fixed: `TestSetupServerRun_MTLSMissingCert` in `internal/cli/apply_server_test.go` was passing `http://localhost:9999` with `TLSMutual` — it was testing the missing-cert error, but the new scheme check fires first. Updated the URL to `https://localhost:9999`; the test's intent (missing cert → error) is preserved since `buildHTTPClient` still rejects `TLSMutual` with no cert/key.

### Step 2 — Switch rendering added
`switches:` rendering was not present (only `states:` was). Added a `switches:` block between `states:` and `plugins required:`, formatted as `  <name>    conditions=<N>   default=<target>`. Used the existing `sortedSwitchNames` helper. Two golden files were regenerated: `switch_basic__workflow__testdata__switch_basic.golden` and `demo_tour_local__examples__demo_tour_local.golden`.

### Step 4 — ConfigSpec comment
`ConfigSpec` does NOT have expression-aware decoding (no `ResolveConfigExprs` equivalent). Per the workstream instructions ("If no, leave the ConfigSpec comment alone"), the ConfigSpec comment was left unchanged.

### docs/LANGUAGE-SPEC.md regeneration
The `workflow/schema.go` comment replacement shifted line numbers, causing `make spec-check` to fail. Ran `make spec-gen` to regenerate. This file is auto-generated and its update is a mandatory side-effect of editing schema.go.

### Validation performed
- `make lint-no-todos` → OK (zero matches)
- Demonstrated non-zero exit with a temporary TODO in `internal/engine/node.go`, then reverted
- `go test -race -count=1 ./internal/transport/server/...` → OK
- `go test -race -count=1 ./internal/cli/...` → OK
- `go test -race -count=1 ./internal/engine/...` → OK
- `go test -race -count=1 ./workflow/...` → OK
- `make ci` → OK (all gates green)
- Zero TODO/FIXME/XXX markers in production Go source confirmed

## Reviewer Notes

### Review 2026-05-12 — changes-requested

#### Summary
The implementation is close: the TLS/http construction check is in place, `criteria plan` now renders `switches:`, the stale TODOs were removed, and the required validation commands pass. Verdict is `changes-requested` for two blockers: Step 1's new tests do not assert the required diagnostic contract, and executor validation left an untracked backup artifact in `internal/engine/`.

#### Plan Adherence
- Step 1: Implemented in `internal/transport/server/client.go`, and the affected CLI test caller was updated in `internal/cli/apply_server_test.go`. Coverage exists, but it only proves rejection, not the required error detail.
- Step 2: Implemented. `internal/cli/plan.go` renders `switches:`, the golden files were updated, and reviewer validation confirmed `criteria plan examples/demo_tour_local/` includes the section.
- Step 3: `internal/engine/node.go` no longer carries the stale TODO, but the worktree still contains a related backup file under `internal/engine/`.
- Step 4: `workflow/schema.go` now documents current `InputSpec` behavior accurately enough, and `docs/LANGUAGE-SPEC.md` was regenerated consistently.
- Step 5: `lint-no-todos` was added to both `Makefile` and CI and behaves as intended on the reviewed tree.
- Step 6: The listed validation commands pass, but the acceptance bar is not met until the blockers below are fixed.

#### Required Remediations
- **Blocker** — `internal/transport/server/client_test.go:865-874`: the replacement Step 1 subtests only assert `err != nil`. The workstream explicitly requires the construction-time error to name both the TLS mode and the offending URL; that diagnostic contract is part of the intended behavior. **Acceptance:** update both rejection subtests to assert the returned error mentions the selected TLS mode and `http://example.com`, while keeping the existing `https://` regression check.
- **Blocker** — `internal/engine/node.go.bak:1`: remove the untracked backup artifact left by the temporary TODO-validation workflow. It contains `// TODO: temporary test marker`, leaves executor-generated junk under `internal/`, and bypasses the new guard only because it is not a `*.go` file. **Acceptance:** delete `internal/engine/node.go.bak` and ensure the worktree no longer contains this file.

### Remediation (executor)

Both blockers addressed:

1. **Error message assertions**: Updated `tls_enable_with_http_url_rejected` and `tls_mutual_with_http_url_rejected` to assert both that the error mentions the TLS mode constant value and `http://example.com`. Added `"strings"` import to the test file. All subtests pass.

2. **Backup artifact**: Deleted `internal/engine/node.go.bak`. Confirmed `git status --short` shows no untracked files under `internal/engine/`.

Validation re-run: `make ci` → OK.

#### Test Intent Assessment
The plan-output coverage is good: `internal/cli/plan_test.go` exercises `renderPlanOutput` across fixtures, and the updated goldens prove the new `switches:` block is present in real workflow plans. The TLS regression check for `https://` + `TLSEnable` is also useful. The weak point is the new rejection-path coverage: a broken implementation that returns an opaque or misleading error would still pass, even though debuggability is the stated reason for the behavior change. The tests need to lock in the diagnostic payload, not only the presence of an error.

#### Validation Performed
- `grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v vendor | grep -v testdata` → no matches
- `make lint-no-todos` → passed
- `go test -race -count=1 ./internal/transport/server/...` → passed
- `go test -race -count=1 ./internal/cli/...` → passed
- `go test -race -count=1 ./internal/engine/...` → passed
- `go test -race -count=1 ./workflow/...` → passed
- `make ci` → passed
- `./bin/criteria plan examples/demo_tour_local/` → output includes `switches:`
- `git status --short` → revealed untracked `internal/engine/node.go.bak`

### Review 2026-05-12-02 — approved

#### Summary
The previous blockers are resolved. The TLS/http rejection tests now lock in the intended diagnostic payload, the stray backup artifact is gone, the TODO-closure changes remain aligned with the workstream, and the reviewed validation suite passes. Verdict: `approved`.

#### Plan Adherence
- Step 1: `internal/transport/server/client.go` rejects `TLSEnable`/`TLSMutual` with `http://`, and `internal/transport/server/client_test.go` now asserts both rejection and error-message content while preserving the `https://` regression check.
- Step 2: `internal/cli/plan.go` renders `switches:`, and the corresponding plan goldens remain updated.
- Step 3: `internal/engine/node.go` no longer carries the stale TODO, and the temporary backup artifact has been removed.
- Step 4: `workflow/schema.go` documents current `InputSpec` behavior accurately, with generated spec output kept in sync.
- Step 5: `lint-no-todos` remains wired into both `Makefile` and CI.
- Step 6: Validation evidence matches the exit criteria.

#### Test Intent Assessment
The rejection-path tests are now strong enough: they would fail if construction stopped surfacing the TLS mode or offending URL, which is the core behavioral intent of the Step 1 tightening. The plan-output golden coverage remains appropriate for the `switches:` addition, and the existing `https://` success case still guards the non-regression path.

#### Validation Performed
- `git status --short` → no untracked backup artifact remains
- `make ci` → passed

### Review 2026-05-12-03 — changes-requested

#### Summary
The code and test changes for Steps 1–4 are in good shape, and the repository currently has no remaining production-code TODO markers. Verdict returns to `changes-requested` for one Step 5/6 blocker: the new TODO guard was wired into GitHub Actions and `make lint`, but the aggregate local `make ci` target still bypasses it, so the repo's documented "all CI gates" entrypoint is no longer aligned with the actual CI gate set.

#### Plan Adherence
- Step 1: Implemented as required. `NewClient` rejects `TLSEnable`/`TLSMutual` with `http://`, and the tests assert both the rejection and the diagnostic payload.
- Step 2: Implemented. `criteria plan` renders `switches:`, and the plan goldens cover the new output.
- Step 3: Implemented. The stale `parallelNode` TODO is gone and no backup artifact remains.
- Step 4: Implemented. `InputSpec` now documents current runtime evaluation behavior, and generated spec output is in sync.
- Step 5: **Partially implemented.** The new guard exists in `Makefile` and the GitHub Actions lint job runs it, but the local `ci` aggregate target still does not invoke `lint-no-todos`.
- Step 6: `make ci` exits 0 on the reviewed tree, but it is not yet exercising the full Step 5 gate set locally.

#### Required Remediations
- **Blocker** — `Makefile:253`: `ci` still expands to `build test lint-imports lint-go lint-baseline-check spec-check validate validate-self-workflows example-plugin`, so it never executes `lint-no-todos`. `make -n ci` confirms the TODO-check recipe is absent, while `make -n lint` includes it. This leaves the repo's advertised "Run all CI gates" target inconsistent with the real CI workflow and means a future production-code TODO can still slip past local `make ci`. **Acceptance:** update `ci` so it includes the TODO guard (either by depending on `lint` instead of spelling out the lint subtargets, or by adding `lint-no-todos` explicitly). Re-run `make -n ci` and confirm the TODO-check recipe is present.

#### Test Intent Assessment
The functional tests are now strong: the TLS rejection coverage locks in the intended error contract, and the plan-output goldens would fail on a regression in switch rendering. The remaining gap is not behavioral test coverage inside Go code; it is repository-gate coverage. The new TODO guard exists, but the main local CI entrypoint does not exercise it.

#### Validation Performed
- `grep -rn 'TODO\|FIXME\|XXX' --include='*.go' . | grep -v vendor | grep -v testdata` → no matches
- `make ci` → passed
- `./bin/criteria plan examples/demo_tour_local/` → output includes `switches:`
- `make -n ci` → does **not** include the `lint-no-todos` recipe
- `make -n lint` → includes the `lint-no-todos` recipe

### Remediation (executor) 2026-05-12-03

Blocker addressed: `Makefile` `ci` target updated to depend on `lint` instead of spelling out individual lint subtargets (`lint-imports lint-go lint-baseline-check spec-check`). This keeps `ci` and `lint` in sync automatically and ensures `lint-no-todos` is always exercised by `make ci`.

- `make -n ci` → now includes the `lint-no-todos` recipe (confirmed)
- `make ci` → passed (all gates green)
