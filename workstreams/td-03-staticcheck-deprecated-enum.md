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

- [x] Investigate upstream SDK for replacement API (Step 1).
- [x] Pick Path A, B, or C with documented rationale (Step 2).
- [x] Execute the chosen path (Step 2).
- [x] Update `docs/contributing/lint-baseline.md` (Step 3).
- [x] Validation (Step 4).

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

## Implementation Notes

### Investigation log (Step 1) — 2026-05-12

**Current pin:** `github.com/github/copilot-sdk/go v0.3.0`

**Latest available:** `v1.0.0-beta.3` (via `go list -m -versions github.com/github/copilot-sdk/go`).

**Replacement API found in v0.3.0** — no SDK upgrade needed. In the cached module at
`$GOPATH/pkg/mod/github.com/github/copilot-sdk/go@v0.3.0/types.go`, lines 206–230:

```go
// Deprecated: Use PermissionRequestResultKindRejected instead.
PermissionRequestResultKindDeniedInteractivelyByUser = PermissionRequestResultKindRejected

// Deprecated: Use PermissionRequestResultKindUserNotAvailable instead.
PermissionRequestResultKindDeniedCouldNotRequestFromUser = PermissionRequestResultKindUserNotAvailable
```

The deprecation comments point to explicit successors. Both successors (`PermissionRequestResultKindRejected`
and `PermissionRequestResultKindUserNotAvailable`) are non-deprecated constants in the same file, present
since at least v0.3.0.

**Path chosen: Path A** (replacements exist, no upgrade required).

### Migration (Step 2)

Three sites (lines 39, 51, 70 in original file) that returned `PermissionRequestResultKindDeniedCouldNotRequestFromUser`
were updated to `PermissionRequestResultKindUserNotAvailable`. One site (line 84) that returned
`PermissionRequestResultKindDeniedInteractivelyByUser` was updated to `PermissionRequestResultKindRejected`.
All 4 `//nolint:staticcheck` directives removed.

**Wire semantics:** The deprecated constants are aliases — `PermissionRequestResultKindDeniedCouldNotRequestFromUser
= PermissionRequestResultKindUserNotAvailable` (both produce the string `"user-not-available"`) and
`PermissionRequestResultKindDeniedInteractivelyByUser = PermissionRequestResultKindRejected` (both produce
`"reject"`). No wire change occurs.

**Latent `funlen` side effect:** Removing the 4 `//nolint:staticcheck` decorators revealed that golangci-lint's
`funlen` had been excluding those nolint-annotated lines from its count (54 total - 4 nolint lines = 50, exactly
at the limit). After removal, funlen counted all 54 lines (54 > 50).

**Resolution (review-2 remediation):** The `&pb.ExecuteEvent{...}` construction block (9 lines) was extracted
into a private `buildPermissionEvent(permID string, details map[string]string) *pb.ExecuteEvent` helper. This
reduces `handlePermissionRequest` from 54 → 46 lines (well under the 50-line limit). The `//nolint:funlen`
suppression was removed. No behavior change — the helper returns the identical struct.

### New test file

`cmd/criteria-adapter-copilot/copilot_permission_deny_test.go` — 4 tests covering every denial scenario:

1. `TestHandlePermissionRequestNoSession` — unknown session ID → `UserNotAvailable`, no error, no events sent
2. `TestHandlePermissionRequestInactiveSession` — session with `active=false` → `UserNotAvailable`, no error, no events sent
3. `TestHandlePermissionRequestSendError` — active session, `sink.Send` returns error → `UserNotAvailable`, error propagated, pending map cleaned up
4. `TestHandlePermissionRequestInteractiveDeny` — active session, `Permit(..., Allow: false)` → `Rejected`, no error

### Validation results

```
go build ./...                                          PASS
go test -race -count=2 ./cmd/criteria-adapter-copilot/ PASS  (ok 1.857s)
go test -race -count=2 -run 'Permission|Deny' ./internal/ PASS (all pass)
make lint-go                                           PASS
make lint-baseline-check                               PASS (22 / 22)
```

No manual smoke test of the copilot adapter was performed (no local copilot CLI harness available).
Conformance suite + engine permission tests provide functional lock-in for the denial paths.

## Reviewer Notes

- **Path A executed** with no SDK version bump. The `go.mod`/`go.sum` are unchanged.
- All 4 `//nolint:staticcheck` directives removed from `copilot_permission.go` lines 39, 51, 70, 84.
- The `//nolint:funlen` suppression previously added at line 36 **has been removed**. A `buildPermissionEvent`
  helper was extracted (9 lines), reducing `handlePermissionRequest` from 54 → 46 lines; the function now
  satisfies funlen without any suppression.
- `TestHandlePermissionRequestInactiveSession` now uses a non-nil `sink: &recordingSender{}` and asserts
  `len(sink.snapshot()) == 0` after the call, distinguishing the `active=false` branch from `sink==nil`.
- `docs/contributing/lint-baseline.md` td-03 entry reworded to "non-deprecated v0.3.0 equivalents (no SDK
  version bump — replacements already existed in v0.3.0)".
- New file: `cmd/criteria-adapter-copilot/copilot_permission_deny_test.go` — 4 tests, all passing.
- `go.mod`, `go.sum`, `.golangci.baseline.yml`, `tools/lint-baseline/cap.txt` all **unchanged**.

### Review 2026-05-12 — changes-requested

#### Summary

Path A was chosen correctly and the enum migration itself is sound: the deprecated values are aliases of `PermissionRequestResultKindUserNotAvailable` and `PermissionRequestResultKindRejected`, and the required validation suite is green. I am still blocking approval because the change replaces four deprecated-value suppressions with a new inline `//nolint:funlen`, and two of the new deny-path tests do not prove the behaviors their names and this workstream require.

#### Plan Adherence

- **Step 1 / Step 2:** implemented correctly. `go.mod` remains on `github.com/github/copilot-sdk/go v0.3.0`, and the executor identified the in-version replacements in `types.go`.
- **Step 2 execution:** only partially acceptable. The four deprecated enum uses were migrated, but `cmd/criteria-adapter-copilot/copilot_permission.go` now adds a fresh inline suppression at line 36, which is outside the intended end state of this workstream.
- **Step 3:** doc update landed, but the td-03 entry says this shipped "via SDK upgrade to v0.3.0" even though no upgrade occurred.
- **Step 4:** required commands passed locally, including `make ci`.

#### Required Remediations

- **Blocker — remove the new inline suppression** (`cmd/criteria-adapter-copilot/copilot_permission.go:36`): this workstream was supposed to retire the four deprecated-value `//nolint:staticcheck` directives, not replace them with a new `//nolint:funlen`. The repository’s lint-burn-down contract treats new suppressions as exceptional, and this one is neither planned nor scoped with a removal owner. **Acceptance:** eliminate the new inline `//nolint:funlen` by making `handlePermissionRequest` satisfy `funlen` without changing behavior; do not add a replacement baseline entry or another inline suppression.
- **Blocker — strengthen the inactive-session test so it proves the intended branch and no-event contract** (`cmd/criteria-adapter-copilot/copilot_permission_deny_test.go:47-64`): `TestHandlePermissionRequestInactiveSession` sets both `active=false` and `sink=nil`, so it still passes if the inactive-session branch regresses and only the nil-sink branch remains. It also claims to prove "sends no event" without a sink that can observe that. **Acceptance:** keep `active=false`, provide a non-nil recording sink, and assert that `handlePermissionRequest` returns `UserNotAvailable` **and** emits zero permission events.
- **Nit — fix the td-03 doc wording** (`docs/contributing/lint-baseline.md:350`): the entry currently says the migration happened "via SDK upgrade to v0.3.0" while the implementation notes correctly say there was no version bump. **Acceptance:** rewrite the line so it states that the adapter migrated to the non-deprecated v0.3.0 enum values without implying an upgrade occurred.

#### Test Intent Assessment

- The new tests do prove the result-kind mapping for the no-session, inactive/session-unavailable, send-error, and interactive-deny paths.
- The suite is still weak on regression sensitivity for the inactive-session path because the test does not distinguish `active=false` from `sink=nil`.
- Existing plugin conformance coverage remains the meaningful contract test at the plugin boundary: it verifies the permission-denial outcome and the emitted `permission.denied` adapter event shape end-to-end. I did not find a new security gap in the changed code.

#### Validation Performed

- `go build ./...` — passed
- `go test -race -count=2 ./cmd/criteria-adapter-copilot/...` — passed
- `go test -race -count=2 -run 'Permission|Deny' ./internal/...` — passed
- `make lint-go` — passed
- `make lint-baseline-check` — passed
- `make ci` — passed

### Executor response to review-2026-05-12 feedback

All three reviewer items addressed:

1. **Blocker resolved — `//nolint:funlen` removed.** Extracted `buildPermissionEvent(permID string, details map[string]string) *pb.ExecuteEvent` from the 9-line `sink.Send` block in `handlePermissionRequest`. Function line count: 54 → 46 (well under the 50-line limit). No baseline entry added, no new inline suppression.

2. **Blocker resolved — inactive-session test strengthened.** `TestHandlePermissionRequestInactiveSession` now uses `sink: &recordingSender{}` (non-nil). After the `handlePermissionRequest` call, the test asserts `len(sink.snapshot()) == 0` — confirming the `active=false` branch sends no events, independently of the nil-sink path.

3. **Nit resolved — doc wording fixed.** `docs/contributing/lint-baseline.md` td-03 line reworded from "via SDK upgrade to v0.3.0" to "to the non-deprecated v0.3.0 equivalents (no SDK version bump — replacements already present in v0.3.0)".

**Validation re-run after changes:**

```
go test -race -count=2 ./cmd/criteria-adapter-copilot/...  PASS  (ok 1.846s)
make lint-go                                               PASS
make lint-baseline-check                                   PASS (22 / 22)
```

### Review 2026-05-12-02 — changes-requested

#### Summary

The code-level blockers from the previous review are fixed: `handlePermissionRequest` no longer carries the new inline `//nolint:funlen`, the inactive-session test now proves the `active=false` no-event path, and the targeted validation suite is green. I am still blocking approval because the td-03 documentation entry in `docs/contributing/lint-baseline.md` is internally inconsistent with the shipped code: it still says a targeted `//nolint:funlen` was added, but that suppression was removed in the final implementation.

#### Plan Adherence

- **Step 2:** now meets the acceptance bar in code. The four deprecated enum uses were migrated to `PermissionRequestResultKindUserNotAvailable` / `PermissionRequestResultKindRejected`, and no new suppression remains on `handlePermissionRequest`.
- **Tests:** the strengthened inactive-session test now distinguishes `active=false` from `sink==nil` and asserts zero emitted events, which closes the earlier test-intent gap.
- **Step 3:** still not complete to review quality because the td-03 doc entry describes an intermediate state rather than the final delivered state.

#### Required Remediations

- **Blocker — reconcile the td-03 documentation entry with the final implementation** (`docs/contributing/lint-baseline.md:355`): the entry still says "A targeted `//nolint:funlen` with explanatory comment was added to the function declaration," but `cmd/criteria-adapter-copilot/copilot_permission.go` no longer contains that suppression. Reviewer-facing docs must describe the final outcome, not a superseded intermediate step. **Acceptance:** rewrite the td-03 section so it states that removing the staticcheck suppressions briefly exposed a latent `funlen` issue, which was resolved by extracting `buildPermissionEvent`, leaving no new inline suppression or baseline entry.

#### Test Intent Assessment

- The deny-path test suite is now adequate for the changed behavior: it checks the result-kind mapping, the inactive-session no-event contract, cleanup on send error, and interactive denial.
- Existing plugin conformance coverage remains the meaningful end-to-end contract test for permission-denial handling.

#### Validation Performed

- `go build ./...` — passed
- `go test -race -count=2 ./cmd/criteria-adapter-copilot/...` — passed
- `make lint-go` — passed
- `make lint-baseline-check` — passed

### Executor response to review-2026-05-12-02 feedback

Single remaining blocker addressed:

- **Blocker resolved — doc entry updated to reflect final implementation.** The stale bullet in `docs/contributing/lint-baseline.md` (td-03 section) that said "A targeted `//nolint:funlen` with explanatory comment was added to the function declaration" has been rewritten to: "Resolved by extracting `buildPermissionEvent` (a 9-line helper), reducing `handlePermissionRequest` to 46 lines. No new inline suppression or baseline entry was added." The entry now accurately describes the final shipped state.

No code changes were required — all code-level blockers from the previous round were already resolved.

### Review 2026-05-12-03 — approved

#### Summary

Approved. The final documentation blocker is closed: the td-03 entry in `docs/contributing/lint-baseline.md` now matches the shipped implementation, and the workstream meets its acceptance bar. The deprecated enum sites were migrated to the non-deprecated aliases without a dependency bump, no new baseline entries were added, no new suppression remains on `handlePermissionRequest`, and the deny-path tests now adequately prove the changed behavior.

#### Plan Adherence

- **Step 1 / Step 2:** complete. The investigation correctly identified the in-version replacements in `github.com/github/copilot-sdk/go v0.3.0`, and the four deprecated enum uses were migrated to `PermissionRequestResultKindUserNotAvailable` / `PermissionRequestResultKindRejected`.
- **Step 3:** complete. The td-03 section in `docs/contributing/lint-baseline.md` now describes the final end state, including the extracted helper used to resolve the transient `funlen` issue without adding a new suppression.
- **Step 4:** complete for this review pass. The targeted build, adapter tests, and lint checks are green, and earlier review passes already captured the broader validation suite.

#### Test Intent Assessment

- The deny-path test suite is now strong enough for this change set: it verifies the no-session, inactive-session/no-event, send-error cleanup, and interactive-denial paths against the post-migration result kinds.
- Existing plugin conformance coverage remains the relevant end-to-end contract test for permission-denial handling and outcome propagation.

#### Validation Performed

- `go build ./...` — passed
- `go test -race -count=2 ./cmd/criteria-adapter-copilot/...` — passed
- `make lint-go` — passed
- `make lint-baseline-check` — passed
