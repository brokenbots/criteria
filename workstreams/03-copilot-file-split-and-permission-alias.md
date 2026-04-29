# Workstream 3 — copilot.go file split + permission-kind alias (UF#02)

**Owner:** Workstream executor · **Depends on:** [W01](01-lint-baseline-mechanical-burn-down.md), [W02](02-lint-ci-gate.md) · **Unblocks:** [W14](14-phase2-cleanup-gate.md) (cleanup gate verifies the W03 baseline-tagged entries are gone).

## Context

The v0.2.0 tech evaluation
([tech_evaluations/TECH_EVALUATION-20260429-01.md](../tech_evaluations/TECH_EVALUATION-20260429-01.md)
section 6 item 3) flags
[cmd/criteria-adapter-copilot/copilot.go](../cmd/criteria-adapter-copilot/copilot.go)
as the single largest non-test, non-generated file in the repo at
**793 LOC** with 34 top-level functions covering five distinct
concerns (plugin lifecycle, session state, turn execution, permission
bridge, utilities). The Phase 1 W03 god-function refactor decomposed
the *functions* but the file itself accumulated more methods rather
than splitting. The eval's recommendation is a file-level split into
≤350-LOC siblings.

The 42 W03-tagged `funlen` / `gocyclo` baseline entries on
`handlePermissionRequest` and `permissionDetails` cannot be burned
down without first splitting the file — once the permission concerns
live in their own file, the funlen exceptions become obvious and
either resolve through extraction or earn a documented inline
`//nolint:funlen` justification.

This workstream also lands user-feedback item **#02 (align Copilot
permission kinds with `allow_tools` ergonomics)**: today
`read_file` / `write_file` in a step's `allow_tools` cause runtime
denial because Copilot's permission kinds are `read` / `write`. The
workflow looks correct but the agent fails. Fix is twofold:

1. Auto-map `read_file` → `read` and `write_file` → `write` (and any
   other documented aliases) when the host evaluates allow_tools
   patterns against the Copilot permission kind.
2. Improve the runtime denial message to suggest valid `allow_tools`
   patterns when the deny path fires.

The split + alias work lands together because the alias touches
`handlePermissionRequest` / `permissionDetails`, and both code paths
become much clearer once they live in `copilot_permission.go`.

## Prerequisites

- [W01](01-lint-baseline-mechanical-burn-down.md) and
  [W02](02-lint-ci-gate.md) merged.
- `make ci` green on `main`.
- Familiarity with the existing W03 god-function split done in
  Phase 1 (see
  [workstreams/archived/v1/03-god-function-refactor.md](archived/v1/03-god-function-refactor.md)).

## In scope

### Step 1 — Plan the split

Target layout (all in `package main`,
`cmd/criteria-adapter-copilot/`):

| New file | Lines (target) | Contents |
|---|---:|---|
| `copilot.go` (kept) | ≤ 200 | package doc, imports, constants, top-level types (`copilotPlugin`, `permDecision`), `Info`, `ensureClient`, `resolveGitHubToken`, `getSession`. |
| `copilot_session.go` | ≤ 200 | `sessionState` struct + helpers, `sdkSession` wrapper, `copilotSession` interface, `OpenSession`, `buildSessionConfig`, `applyOpenSessionModel`, `CloseSession`. |
| `copilot_turn.go` | ≤ 250 | `turnState` struct, `Execute`, `prepareExecute`, `beginExecution`, `newTurnState`, `sendErr`, `handleEvent`, `handleAssistantDelta`, `handleAssistantMessage`, `awaitOutcome`, `applyRequestModel`, `applyRequestEffort`, `validateReasoningEffort`, `parseOutcome`. |
| `copilot_permission.go` | ≤ 250 | `Permit`, `handlePermissionRequest`, `permissionDetails`, `includeSensitivePermissionDetails`, the new permission-kind alias logic (Step 4). |
| `copilot_util.go` | ≤ 100 | `resultEvent`, `logEvent`, `adapterEvent`, `stringifyAny`. |

**Constraints:**

- All methods stay on the `copilotPlugin` receiver (no struct rename,
  no interface change).
- No new exported symbols.
- Imports per file are exactly the imports each file uses (run
  `goimports -w` after the split).
- One-line file-level doc comment on each new file naming its slice
  of responsibility (e.g. `// copilot_permission.go — host
  permission bridge and allow_tools alias resolution.`).
- Test files mirror the split. The existing single test file (or
  files) split into `copilot_session_test.go`, `copilot_turn_test.go`,
  `copilot_permission_test.go`, etc., **only** if existing tests
  cleanly belong in one of those buckets. Otherwise leave the test
  file as-is and add new tests in the appropriately named file.

### Step 2 — Move functions verbatim

Use `git mv` semantics — i.e., the diff for each function move should
read as add+delete with identical bodies. Do **not** rename, refactor,
or change signatures during the split. The split itself is no-behavior
change.

After the moves:

- `make build` succeeds.
- `make test` (specifically the copilot adapter package tests) is
  green.
- `make lint-go` reports the W03-tagged `funlen`/`gocyclo` entries
  pointing at functions that are now in the new files. Update the
  baseline entries' file paths accordingly *only if the rule still
  fires* — otherwise remove the entry.

### Step 3 — Burn down W03 baseline entries that no longer fire

After the move, run `make lint-go`. For each W03-tagged entry in
`.golangci.baseline.yml`:

1. If the rule no longer fires (because the function is now small
   enough or the surrounding context changed), remove the entry.
2. If the rule still fires, the function is still too long / complex.
   Try to extract a helper — keep the change minimal. If extraction
   is not natural, replace the baseline entry with an inline
   `//nolint:funlen // <one-sentence justification>` annotation. The
   rule of thumb: a baseline entry is worse than an inline nolint
   because the latter forces a justification.

Target: `# W03:`-tagged entry count drops from 42 to **≤ 10**.

### Step 4 — Permission-kind alias (UF#02)

Add an alias map to `copilot_permission.go`:

```go
// permissionKindAliases maps host-facing tool names that operators
// commonly write in allow_tools to the Copilot SDK's permission
// kinds. The aliases let workflows declare allow_tools = ["read_file"]
// instead of allow_tools = ["read"], matching the documented Copilot
// tool names.
var permissionKindAliases = map[string]string{
    "read_file":  "read",
    "write_file": "write",
    // Add more aliases here as Copilot evolves; document the source
    // of the canonical name in the comment above the entry.
}
```

The host-side `allow_tools` evaluator currently lives in the engine
(it predates this workstream). Inspect
[internal/engine/](../internal/engine/) and
[internal/plugin/policy.go](../internal/plugin/policy.go) — find the
function that decides whether a permission request matches an
allow_tools pattern. The alias resolution must happen at the *host*
level, not inside the plugin, because:

1. The plugin emits the canonical Copilot kind (`read`/`write`/`shell`/`mcp`).
2. The host compares against the workflow's `allow_tools` strings.
3. The mismatch is `read_file` (in workflow) vs. `read` (from plugin).

Resolution: when matching, normalize the workflow-side pattern through
the alias map *if* the requesting plugin is the copilot adapter. Two
ways to do this:

- **Plugin-declared aliases (preferred).** Extend the plugin `Info`
  RPC schema to include an optional `permission_kind_aliases` field
  (a `map<string, string>`). The host reads it during plugin discovery
  and applies it during allow_tools matching for that adapter. This is
  generic and lets future adapters declare their own aliases.
- **Adapter-name hardcode (fallback).** If the proto extension is too
  large for this workstream, hardcode an alias map in the engine
  keyed by adapter name (`copilot`). Document this as a temporary
  shim and file a follow-up to move it into the proto.

Pick the proto-extension path unless it expands the workstream beyond
~5 days of effort. If hardcoded, the constant must live in the
copilot adapter's package and be exposed via a non-RPC accessor used
by the engine — do not duplicate the map.

**Compile-time diagnostic:** when the workflow compiler resolves
`allow_tools` for a step bound to the copilot adapter, emit a
diagnostic warning if a pattern uses the legacy alias name
(`read_file` / `write_file`) suggesting the canonical form. This is a
warning, not an error — workflows continue to compile, but the
operator sees the suggestion. Plumb through the existing diagnostic
infrastructure used by W09 (Phase 1) — see
[workflow/compile_steps.go](../workflow/compile_steps.go) for the
pattern.

### Step 5 — Improved denial message

When a permission request hits the deny path in
`handlePermissionRequest` (no matching allow_tools entry), enrich
the runtime error with:

- The requested permission kind.
- The list of allow_tools patterns the workflow declared.
- A suggested allow_tools string the operator could add.

Today the host emits `permission.denied` with reason
`no matching allow_tools entry`. Extend the reason / details to
include the suggestion. Locate the host code that emits
`permission.denied` (in [internal/plugin/](../internal/plugin/) or
[internal/engine/](../internal/engine/)) — adjust the message there;
the plugin itself stays unchanged for this part.

### Step 6 — Documentation

Update [docs/plugins.md](../docs/plugins.md):

- Document the alias map (under the Copilot Adapter Reference section).
- Update the "Permission Gating" section to mention that
  `read_file` and `write_file` are recognized aliases.
- Add a one-line note that the compile-time warning surfaces the
  canonical form.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, or
`CHANGELOG.md`.

### Step 7 — Validate

- `make ci` green.
- New unit test in `copilot_permission_test.go` exercises:
  - `allow_tools = ["read_file"]` allows a `read` permission request.
  - `allow_tools = ["write_file"]` allows a `write` permission request.
  - `allow_tools = ["read"]` continues to allow `read` (no
    regression).
  - A non-aliased name (e.g. `shell:git status`) is unaffected.
  - The compile-time warning fires for `allow_tools = ["read_file"]`
    when the step is bound to the copilot adapter, and the workflow
    still compiles.
- New unit test in the host-side denial path exercises the suggestion
  message includes the requested kind and allowlist.

## Behavior change

**Yes — for the alias and diagnostic. No — for the file split itself.**

File split:
- All 34 functions move verbatim. No signature change. No exported
  symbol change. All existing tests pass unchanged. CLI / HCL / event
  contract unaffected.

Permission alias (UF#02):
- A workflow that previously failed at runtime with `permission.denied`
  for `allow_tools = ["read_file"]` now succeeds with
  `permission.granted`. This is the intent of the user feedback.
- A new compile-time warning surfaces (does not block compile) when an
  alias is used in a copilot-adapter `allow_tools`.
- The `permission.denied` event reason text changes to include
  suggestions. The event *kind* and *id* fields are unchanged. Any
  consumer that string-matched the reason `no matching allow_tools
  entry` may need to update — list this as a CHANGELOG note for
  [W14](14-phase2-cleanup-gate.md) to capture.
- If the proto-declared aliases path is taken, `Info` response gains
  an optional `permission_kind_aliases` map. Older hosts ignore the
  field; older plugins still work (host falls back to identity match).

## Reuse

- Existing `copilotPlugin` struct, `sessionState`, `turnState`,
  `permDecision` types. No struct rename.
- Existing host-side allow_tools matcher (locate via grep — likely in
  `internal/plugin/policy.go` or `internal/engine/`). Add the alias
  resolution there; do not reimplement.
- Existing compile-time diagnostic infrastructure
  ([workflow/compile_steps.go](../workflow/compile_steps.go) — see
  the W09 misplaced-agent-config diagnostic for the pattern).
- The `Info()` RPC response if the proto-extension path is taken.

## Out of scope

- Renaming `copilotPlugin` or any of its methods.
- Changing the SDK's permission-kind vocabulary
  (`read`/`write`/`shell`/`mcp` is the SDK contract).
- Introducing aliases for non-Copilot adapters in this workstream.
- Refactoring `handleEvent` further than what naturally falls out of
  the file move.
- Removing the `CRITERIA_COPILOT_INCLUDE_SENSITIVE_PERMISSION_DETAILS`
  env var; that is a separate concern.
- Editing generated proto bindings by hand. If the proto-extension
  path is taken, run `make proto` and commit the regenerated
  bindings.

## Files this workstream may modify

- `cmd/criteria-adapter-copilot/copilot.go` (slim down).
- `cmd/criteria-adapter-copilot/copilot_session.go` (new).
- `cmd/criteria-adapter-copilot/copilot_turn.go` (new).
- `cmd/criteria-adapter-copilot/copilot_permission.go` (new).
- `cmd/criteria-adapter-copilot/copilot_util.go` (new).
- `cmd/criteria-adapter-copilot/copilot_*_test.go` (split + new
  alias / suggestion tests).
- `proto/criteria/v1/adapter_plugin.proto` (only if the proto-extension
  alias path is taken — add an optional field to `InfoResponse`).
- `sdk/pb/criteria/v1/*.pb.go` (regenerated by `make proto`; commit
  alongside the proto edit).
- The host-side allow_tools matcher (likely
  `internal/plugin/policy.go` or an engine sibling — locate via grep).
- `workflow/compile_steps.go` (compile-time warning).
- `internal/plugin/sessions.go` or wherever `permission.denied` is
  emitted (suggestion message).
- `docs/plugins.md` (alias documentation).
- `.golangci.baseline.yml` (entry removal / file-path updates after
  the move).

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.

## Tasks

- [ ] Decide proto-extension vs. hardcoded alias path; document choice
      in reviewer notes.
- [ ] Split `copilot.go` into the five files per Step 1, moving
      functions verbatim.
- [ ] Update `.golangci.baseline.yml` file paths and remove entries
      that no longer fire. Target ≤ 10 W03-tagged entries.
- [ ] Implement permission-kind alias resolution at the host.
- [ ] Add compile-time warning for legacy alias names in copilot
      `allow_tools`.
- [ ] Improve `permission.denied` reason with the requested kind and
      a suggestion.
- [ ] Update `docs/plugins.md` with the alias documentation.
- [ ] Add unit tests per Step 7.
- [ ] `make build`, `make plugins`, `make test`, `make lint-go`,
      `make ci` all green.

## Exit criteria

- All five copilot files exist; each ≤ the target line count in
  Step 1.
- `make build`, `make plugins`, `make test -race -count=2`,
  `make lint-go`, `make lint-baseline-check`, `make ci` all green.
- `# W03:`-tagged baseline entries ≤ 10.
- A workflow with `allow_tools = ["read_file"]` bound to the copilot
  adapter receives `permission.granted` for a `read` permission
  request (manually verified or covered by an integration test).
- The compile-time warning fires on `allow_tools = ["read_file"]`
  with copilot adapter; workflow still compiles.
- `permission.denied` events on copilot steps include the requested
  kind and a suggested `allow_tools` pattern.
- `docs/plugins.md` documents the aliases.

## Tests

New unit tests:

- `copilot_permission_test.go` — alias resolution (4 cases per
  Step 7).
- `copilot_session_test.go` / `copilot_turn_test.go` — only as needed
  to keep coverage at parity after the file split. The existing
  coverage threshold for `cmd/criteria-adapter-copilot` is 65.9%
  (per the v0.2.0 eval); do not regress.
- `workflow/compile_steps_test.go` (or equivalent) — compile-time
  warning for legacy alias name on copilot step.
- Host-side denial-path test asserting the new suggestion message.

Existing tests must pass unchanged. If any test breaks during the
move, the move is wrong — back out and redo.

## Risks

| Risk | Mitigation |
|---|---|
| The proto extension is rejected by `buf lint` rules | Add the new field as `optional` with a high field number; run `make proto-lint` before committing. If rejected, fall back to the hardcoded-alias path and document. |
| The host-side allow_tools matcher is harder to locate than expected | Spend up to 30 minutes greping. If still unclear, surface a question in reviewer notes — the matcher predates this workstream and the architecture should not block on it. |
| Coverage drops on `cmd/criteria-adapter-copilot` after the file split | Coverage is per-package, not per-file. As long as the same code paths run, coverage stays even. If the split changes coverage by >2%, investigate. |
| The compile-time warning fires for legitimate non-copilot adapters that happen to share the alias name | Gate the warning on `step.adapter == "copilot"` (or, more generally, on the plugin's declared aliases) — do not warn for adapters that don't declare aliases. |
| Removing baseline entries hides a real lint regression | The lint cap from [W02](02-lint-ci-gate.md) catches new findings. If a removed entry's rule re-fires elsewhere, the cap will surface it. |
