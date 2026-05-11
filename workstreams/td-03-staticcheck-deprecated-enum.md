# td-03 — Migrate copilot adapter off deprecated `PermissionRequestResultKind` values

**Phase:** Pre-Phase-4 (adapter-rework prep) · **Track:** B (tech debt) · **Owner:** Workstream executor · **Depends on:** none. (Can run in parallel with [td-02-nolint-suppression-sweep.md](td-02-nolint-suppression-sweep.md) — td-02 explicitly excludes these 4 directives so there is no conflict.) · **Unblocks:** none.

## Context

The copilot adapter binary at [cmd/criteria-adapter-copilot/copilot_permission.go](../cmd/criteria-adapter-copilot/copilot_permission.go) carries 4 inline `//nolint:staticcheck` directives — all on uses of two deprecated enum values from `github.com/github/copilot-sdk/go v0.3.0`:

- `copilot.PermissionRequestResultKindDeniedCouldNotRequestFromUser` (lines 39, 51, 70)
- `copilot.PermissionRequestResultKindDeniedInteractivelyByUser` (line 84)

The current rationale comments say "no replacement for user-absent denial" and "no replacement for interactive denial". When those directives were added, the SDK upgrade did not provide a clean replacement so the suppressions were the right call. We now need to revisit:

1. Has `github.com/github/copilot-sdk/go` published a non-deprecated replacement in a newer version?
2. If yes, upgrade the dependency and migrate the call sites.
3. If no, decide whether to:
   - Stay on the current SDK version and keep the suppressions (do nothing in this workstream — close as no-op with a documented "still no replacement" finding).
   - Pin to the current version and add a `# kept:` baseline entry per directive instead of inline `//nolint`.

The 4 directives all fire on the same two enum values used in three distinct denial scenarios. Whatever the migration target is, it must preserve the **observable behavior** of the copilot adapter: the deny path must continue to result in the engine receiving a deny event and the copilot session terminating gracefully.

This workstream's primary deliverable is the **investigation outcome** plus whichever code change follows from it. If the investigation concludes "no replacement exists yet", the workstream still ships: it documents the finding, sharpens the rationale comments, and marks the directives as "intentionally retained pending upstream API change" with a tracking note.

## Prerequisites

- `make ci` green on `main`.
- Network access to `pkg.go.dev` and `github.com/github/copilot-sdk` (read-only — to inspect newer versions).
- `go` toolchain matches the version pinned in [go.mod](../go.mod).
- The 4 deprecated-enum sites at lines 39, 51, 70, 84 of `copilot_permission.go` are still present (verify via `grep -n PermissionRequestResultKindDenied cmd/criteria-adapter-copilot/copilot_permission.go`).

## In scope

### Step 1 — Investigate the upstream SDK

1. Identify the current pinned version: `grep copilot go.mod` should show `github.com/github/copilot-sdk/go v0.3.0` (or whatever the current pin is).
2. Check the latest released version at `https://pkg.go.dev/github.com/github/copilot-sdk/go?tab=versions` (or `go list -m -versions github.com/github/copilot-sdk/go`).
3. For each newer minor/patch version, read the `CHANGELOG.md` and the `permission.go` source on GitHub. Look for:
   - Replacement enum values for `PermissionRequestResultKindDeniedCouldNotRequestFromUser` and `PermissionRequestResultKindDeniedInteractivelyByUser`.
   - Any new field on `PermissionRequestResult` (e.g. a `DenyReason` enum) that subsumes the deprecated kinds.
   - Migration notes referencing these specific kinds.
4. Record findings in reviewer notes:
   - Newest available version.
   - Whether a replacement API exists.
   - If yes: the exact replacement (struct/field/value) and the migration shape.
   - If no: cite the line in the SDK source that confirms `// Deprecated:` is still the only signal and there is no replacement.

The investigation must be thorough — the deliverable depends on it. If the SDK's deprecation comment points to a successor type/value (`// Deprecated: use X.Y instead`), use it. If it does not, walk the type's other constants and the type's docstring to confirm no replacement.

### Step 2 — Pick the migration path

Based on Step 1, choose **one** of three paths. The choice is not optional — one must be picked and documented.

#### Path A — Replacement exists; upgrade SDK and migrate

1. Bump `github.com/github/copilot-sdk/go` to the version that provides the replacement. Update `go.mod` and `go.sum` (`go get -u github.com/github/copilot-sdk/go@vX.Y.Z`, then `go mod tidy`).
2. Replace each of the 4 deprecated-enum uses with the new API. Map:
   - `PermissionRequestResultKindDeniedCouldNotRequestFromUser` (3 sites) → `<new value or struct shape>`.
   - `PermissionRequestResultKindDeniedInteractivelyByUser` (1 site) → `<new value or struct shape>`.
3. Remove all 4 `//nolint:staticcheck` directives.
4. Confirm the test at [cmd/criteria-adapter-copilot/copilot_internal_test.go:320](../cmd/criteria-adapter-copilot/copilot_internal_test.go#L320) (which uses `PermissionRequestResultKindApproved`, not deprecated) still compiles. If `Approved` was also renamed, update it too.
5. Run the copilot adapter conformance suite to confirm denial paths still terminate correctly:
   ```sh
   go test -race -count=2 ./cmd/criteria-adapter-copilot/...
   ```
6. Run the engine tests that exercise permission denial:
   ```sh
   go test -race -count=2 -run 'Permission|Deny' ./internal/...
   ```

If the SDK upgrade brings other breaking changes beyond these 4 sites, the workstream's scope grows — but only to the minimum needed to keep the build green. Document each additional fix in reviewer notes. If the additional scope is large (> 200 lines or > 5 files), stop and split into a follow-up workstream.

#### Path B — No replacement; move to baseline

1. Add 4 entries to `.golangci.baseline.yml` (one per call site, or one tighter regex covering all 4 if they share a unique substring like `Permission.*Denied.*FromUser`):
   ```yaml
   # kept: copilot-sdk v0.3.0 deprecated PermissionRequestResultKindDeniedCouldNotRequestFromUser without providing a replacement;
   #   investigated 2026-MM-DD and confirmed no successor in vX.Y.Z (latest). Re-audit on next SDK upgrade.
   - path: cmd/criteria-adapter-copilot/copilot_permission\.go
     linters:
       - staticcheck
     text: 'PermissionRequestResultKindDeniedCouldNotRequestFromUser'
   # kept: same — interactive-denial variant. Re-audit on next SDK upgrade.
   - path: cmd/criteria-adapter-copilot/copilot_permission\.go
     linters:
       - staticcheck
     text: 'PermissionRequestResultKindDeniedInteractivelyByUser'
   ```
2. Remove all 4 inline `//nolint:staticcheck` directives.
3. Update `tools/lint-baseline/cap.txt` to the new exact count (the cap rises by however many baseline entries were added — typically 2 if the regex consolidates).
4. Run `make lint-go` and `make lint-baseline-check`; confirm green.

#### Path C — No replacement; tighten inline rationales and stay

1. Keep the 4 directives in place.
2. Rewrite each comment to include the investigation date and the latest SDK version checked:
   ```go
   //nolint:staticcheck // copilot-sdk vX.Y.Z still has no replacement for this denial kind (verified 2026-MM-DD); see workstreams/td-03 for investigation log
   ```
3. Add a `# investigation:` block to the `## Implementation Notes` section of this workstream file with the date, SDK version checked, and the conclusion.

**Pick Path A if at all possible.** Path B is the next-best (centralises the suppression with documented context). Path C is the fallback (chosen only when neither A nor B is appropriate — e.g. Path A is unsafe because the SDK upgrade brings unrelated breakage, and Path B is unsafe because the staticcheck rule might miss a future deprecation in this file if the regex is too broad).

### Step 3 — Update `docs/contributing/lint-baseline.md`

If Path B was chosen, append to the file (after the td-02 section if td-02 has landed):

```markdown
## td-03 (pre-Phase-4) — 2026-MM-DD

- Migrated copilot adapter off deprecated `PermissionRequestResultKindDenied*` values via Path B.
- 4 inline `//nolint:staticcheck` directives removed; 2 `# kept:` baseline entries added.
- SDK version checked: vX.Y.Z. Successor API: none as of investigation date.
- Re-audit trigger: next bump of `github.com/github/copilot-sdk/go`.
```

If Path A was chosen, the entry is shorter:

```markdown
## td-03 (pre-Phase-4) — 2026-MM-DD

- Migrated copilot adapter off deprecated `PermissionRequestResultKindDenied*` values via SDK upgrade to vX.Y.Z.
- 4 inline `//nolint:staticcheck` directives removed; no baseline entries added.
```

If Path C was chosen:

```markdown
## td-03 (pre-Phase-4) — 2026-MM-DD

- Investigated copilot-sdk vX.Y.Z; no replacement for deprecated `PermissionRequestResultKindDenied*` values.
- 4 inline `//nolint:staticcheck` directives retained with tightened rationale and investigation date.
- Re-audit trigger: next bump of `github.com/github/copilot-sdk/go` past vX.Y.Z.
```

### Step 4 — Validation

```sh
go build ./...
go test -race -count=2 ./cmd/criteria-adapter-copilot/...
go test -race -count=2 -run 'Permission|Deny' ./internal/...
go test -race -count=1 ./...
make lint-go
make lint-baseline-check
make ci
```

All seven must exit 0. Inspect:

- For Path A: `grep -c 'staticcheck' cmd/criteria-adapter-copilot/copilot_permission.go` returns 0.
- For Path B: same as Path A on the source file; baseline file has the new `# kept:` entries.
- For Path C: each inline directive's comment includes a date in `YYYY-MM-DD` format and the SDK version.

For Path A, also run an end-to-end smoke test of the copilot adapter with a denial scenario:

```sh
make example-plugin   # builds the example plugin used in CI
# Manually exercise a copilot workflow that triggers a deny path; confirm the run terminates with the expected outcome.
```

If a manual smoke is impractical (no copilot test harness available locally), rely on the conformance suite + engine permission tests. Document in reviewer notes that no manual smoke was performed.

## Behavior change

**Path A (SDK upgrade):** behavior change is **possible but should be invisible**. The replacement enum values must produce the same wire-level deny event. Verify by running the conformance test that exercises the deny event payload at [internal/adapter/conformance/](../internal/adapter/conformance/) — if no such test exists, this workstream adds one (a one-shot test that drives a deny scenario through the copilot adapter and asserts the resulting `pb.ExecuteEvent` envelope matches the pre-upgrade envelope byte-for-byte).

**Path B and Path C:** **No behavior change.** Suppression relocation only.

If Path A reveals that the new SDK API has subtly different semantics (e.g. the new value carries an extra field that the engine doesn't expect), that is a real migration risk and must be addressed in this workstream — either by adapting the engine consumer or by escalating and reverting to Path B/C with a documented reason.

## Reuse

- Existing copilot adapter session/permission machinery in [cmd/criteria-adapter-copilot/](../cmd/criteria-adapter-copilot/).
- `getSession`, `pending` map, `permDecision` channel, `sink.Send` in `copilot_permission.go` — do not change these structures; only the enum values change (Path A) or the suppressions move (Path B/C).
- Existing baseline tooling at [tools/lint-baseline/](../tools/lint-baseline/).
- Existing `make lint-go` and `make lint-baseline-check` targets.
- Existing conformance harness at [internal/adapter/conformance/](../internal/adapter/conformance/).

## Out of scope

- Other deprecated APIs in `github.com/github/copilot-sdk/go`. Only the 4 listed deprecated-enum sites are addressed.
- Changes to `cmd/criteria-adapter-copilot/copilot_permission.go` beyond what is required to remove the 4 directives.
- Refactoring `permissionDetails` (line 93) — its `funlen,gocognit,gocyclo` directive is a separate concern owned by [td-02-nolint-suppression-sweep.md](td-02-nolint-suppression-sweep.md).
- Bumping any other Go module dependency.
- Adding or changing any HCL surface, CLI flag, or proto field.
- Modifying `internal/cli/`, `workflow/`, or any other package outside `cmd/criteria-adapter-copilot/`.

## Files this workstream may modify

- [`cmd/criteria-adapter-copilot/copilot_permission.go`](../cmd/criteria-adapter-copilot/copilot_permission.go) — Path A: replace deprecated enum uses; Path B: remove 4 inline directives; Path C: tighten 4 inline comments.
- [`go.mod`](../go.mod), [`go.sum`](../go.sum) — Path A only: bump copilot-sdk version.
- [`.golangci.baseline.yml`](../.golangci.baseline.yml) — Path B only: add 1–4 `# kept:` entries.
- [`tools/lint-baseline/cap.txt`](../tools/lint-baseline/cap.txt) — Path B only: bump cap.
- [`docs/contributing/lint-baseline.md`](../docs/contributing/lint-baseline.md) — append the td-03 section per Step 3.
- (Path A only) New test file [`cmd/criteria-adapter-copilot/copilot_permission_deny_test.go`](../cmd/criteria-adapter-copilot/) — one test per deny scenario asserting the wire envelope. Only added if no equivalent test already exists; check first.

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, or any other workstream file.
- Any file outside `cmd/criteria-adapter-copilot/` other than `go.mod`, `go.sum`, `.golangci.baseline.yml`, `tools/lint-baseline/cap.txt`, `docs/contributing/lint-baseline.md`.
- Generated proto files.
- [`.golangci.yml`](../.golangci.yml).

## Tasks

- [ ] Investigate upstream SDK for replacement API (Step 1).
- [ ] Pick Path A, B, or C with documented rationale (Step 2).
- [ ] Execute the chosen path (Step 2).
- [ ] Update `docs/contributing/lint-baseline.md` (Step 3).
- [ ] Validation (Step 4).

## Exit criteria

- The 4 deprecated-enum uses on lines 39, 51, 70, 84 of `copilot_permission.go` are either:
  - **Path A:** replaced with non-deprecated equivalents and `//nolint:staticcheck` directives removed.
  - **Path B:** still present, but inline directives removed and replaced with `# kept:` baseline entries.
  - **Path C:** still present with tightened inline comments that include the investigation date and SDK version.
- `go build ./...` exits 0.
- `go test -race -count=2 ./cmd/criteria-adapter-copilot/...` exits 0.
- `make lint-go` exits 0.
- `make lint-baseline-check` exits 0.
- `make ci` exits 0.
- `docs/contributing/lint-baseline.md` contains the new td-03 section with the chosen path and SDK version.
- Reviewer notes contain the investigation log from Step 1.

## Tests

- Path A: existing `cmd/criteria-adapter-copilot/copilot_internal_test.go` and the conformance suite are the lock-in. If no test currently exercises the three deny paths (user-absent, interactive deny, send-error) and asserts the resulting `pb.ExecuteEvent` envelope, **add `copilot_permission_deny_test.go`** with three test cases — one per scenario at lines 39 / 51 / 70 / 84. Each test:
  1. Constructs a fake session with a fake `sink`.
  2. Calls `handlePermissionRequest(sessionID, &copilot.PermissionRequest{...})`.
  3. Asserts the returned `PermissionRequestResult.Kind` matches the expected (post-migration) value.
  4. (For lines 39/51/70 — `DeniedCouldNotRequestFromUser`) Asserts no `pb.ExecuteEvent` was sent on the sink (or, post-migration, asserts whatever the new wire contract is — confirm with the SDK migration notes).
- Path B and Path C: no new tests required. The existing test at [cmd/criteria-adapter-copilot/copilot_internal_test.go:320](../cmd/criteria-adapter-copilot/copilot_internal_test.go#L320) using `PermissionRequestResultKindApproved` continues to lock in the approved path; the deny paths are unchanged so existing coverage applies.

## Risks

| Risk | Mitigation |
|---|---|
| SDK upgrade brings breaking changes beyond the 4 sites | Cap the workstream scope to the minimum needed to keep the build green. If extra scope exceeds 200 lines or 5 files, escalate to a follow-up workstream and revert to Path B/C for now. |
| New SDK enum has subtly different wire semantics (different denial reason on the engine side) | The Path A test additions assert the wire envelope shape. If a regression appears, document it and choose Path B/C instead. |
| The newer SDK version drops support for an older Go minor that `go.mod` pins | Check the SDK's `go.mod` directive against ours before upgrading. If incompatible, choose Path B/C. |
| `go.sum` checksum changes ripple into a CI cache invalidation that takes longer to diagnose than the workstream itself | Run `make ci` locally before pushing; confirm `go mod download` + tests pass with the new pin. |
| Path C's comment rewrite is the only outcome and the workstream feels like it accomplished nothing | Path C is still a real improvement: the rationale now names the date and SDK version, so the next person knows when to re-investigate. The investigation log itself is the deliverable. |
| The investigation in Step 1 is shallow and misses a replacement API | Reviewer asks the executor to cite the specific SDK source line that confirms "no replacement". If the executor cannot, they re-investigate. |
