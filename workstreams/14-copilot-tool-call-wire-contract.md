# Workstream 14 — Copilot tool-call wire contract (`allowed_outcomes`)

**Owner:** Workstream executor · **Depends on:** none ·
**Unblocks:** [W15](15-copilot-submit-outcome-adapter.md) (the adapter
consumes the new wire field), [W16](16-phase2-cleanup-gate.md)
(cleanup gate verifies SDK bump + transport coverage).

## Context

Today the Copilot adapter derives a step's outcome by string-matching a
`result:` prefix in the model's final assistant message
([cmd/criteria-adapter-copilot/copilot_turn.go:223](../cmd/criteria-adapter-copilot/copilot_turn.go#L223)
— `parseOutcome`, default `needs_review`). The host's
`StepNode.Outcomes` map keys
([workflow/schema.go:284](../workflow/schema.go#L284),
[internal/engine/node_step.go:340](../internal/engine/node_step.go#L340))
are never communicated to the adapter — the model has no structured
view of what outcomes the workflow author actually declared.

Phase 2 replaces prose parsing with a structured `submit_outcome` tool
call (full design captured in
[architecture_archive/](../architecture_archive/)). This workstream is
the **mechanical, no-behavior-change first half** of that move: extend
the wire contract so adapters know the per-step outcome set. The
adapter behavior change ships separately in
[W15](15-copilot-submit-outcome-adapter.md).

Splitting the work this way:

1. Keeps the proto / SDK bump isolated and reviewable on its own.
2. Lets [W15](15-copilot-submit-outcome-adapter.md) land Copilot
   tool-call finalization without also re-reviewing wire generation.
3. Bounds blast radius: this PR alters generated Go bindings and one
   field on `pb.ExecuteRequest`, with no runtime semantics change.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with
  [proto/criteria/v1/adapter_plugin.proto](../proto/criteria/v1/adapter_plugin.proto),
  [internal/plugin/loader.go](../internal/plugin/loader.go), and
  [internal/engine/node_step.go](../internal/engine/node_step.go).
- Familiarity with
  [CONTRIBUTING.md](../CONTRIBUTING.md)'s SDK-bump policy (this
  workstream is a breaking SDK contract change for plugin authors who
  hand-roll an `ExecuteRequest`; the bump must follow that policy).

## In scope

### Step 1 — Extend `ExecuteRequest` with `allowed_outcomes`

Edit
[proto/criteria/v1/adapter_plugin.proto](../proto/criteria/v1/adapter_plugin.proto)
`message ExecuteRequest` (currently lines 52–56):

```proto
message ExecuteRequest {
  string session_id = 1;             // permanent
  string step_name = 2;              // permanent
  map<string, string> config = 3;    // permanent
  repeated string allowed_outcomes = 4; // permanent (W14 — declared outcome names for this step, sorted ascending)
}
```

Hard requirements for the field:

- Field number `4`. Do not reuse any prior tag.
- Trailing `// permanent (W14 ...)` comment per repo convention.
- Field name `allowed_outcomes` (snake_case in proto).
- Generated Go field becomes `AllowedOutcomes []string`.

### Step 2 — Regenerate Go bindings

Run `make proto`. This refreshes
[sdk/pb/criteria/v1/adapter_plugin.pb.go](../sdk/pb/criteria/v1/adapter_plugin.pb.go)
(the generated file the rest of the tree imports as
`pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"`).

Verify:

- `make proto-check-drift` exits 0 after the regen is committed.
- `make proto-lint` exits 0.
- Only the expected files changed: the `.proto`, the generated
  `.pb.go`(s), and any descriptor blobs (`*.pb.bin` if present).

If `make proto` produces unrelated diffs (e.g. timestamp tags, reorder
of unrelated messages), root-cause and revert those before committing.
The goal is a minimal, reviewable diff.

### Step 3 — SDK bump

This is a breaking SDK contract change for plugin authors who construct
`ExecuteRequest` manually (the host populates `AllowedOutcomes`; the
adapter side reads it). Follow the bump policy in
[CONTRIBUTING.md](../CONTRIBUTING.md).

Concretely:

- Locate the SDK module version source. In this tree the SDK is the
  sub-module at [sdk/](../sdk/) with its own `go.mod` and version
  metadata; consult
  [sdk/CHANGELOG.md](../sdk/CHANGELOG.md) (or `sdk/VERSION`,
  whichever the repo uses) and follow the existing conventions for
  bumping.
- Add an SDK CHANGELOG entry text in **reviewer notes** (do not edit
  top-level `CHANGELOG.md` — that is
  [W16](16-phase2-cleanup-gate.md)'s territory). The text must say:
  - The new field name and tag (`allowed_outcomes` field 4).
  - That host implementations now populate it from the step's declared
    outcome set.
  - That adapter implementations may consume it but are not required
    to (no runtime semantics change yet — see
    [W15](15-copilot-submit-outcome-adapter.md) for the Copilot
    consumer).
  - Backward compatibility note: existing adapters that ignore the
    field continue to function unchanged.

If the SDK bump policy requires a tagged commit, name the version in
reviewer notes; do **not** push the tag in this PR — tag bumps belong
to the cleanup gate.

### Step 4 — Populate `AllowedOutcomes` in the host

Edit
[internal/plugin/loader.go:204](../internal/plugin/loader.go#L204):

Today:

```go
recv, err := p.rpc.Execute(ctx, &pb.ExecuteRequest{
    SessionId: sessionID,
    StepName:  step.Name,
    Config:    cloneConfig(step.Input),
})
```

After this workstream:

```go
recv, err := p.rpc.Execute(ctx, &pb.ExecuteRequest{
    SessionId:       sessionID,
    StepName:        step.Name,
    Config:          cloneConfig(step.Input),
    AllowedOutcomes: collectAllowedOutcomes(step),
})
```

Add `collectAllowedOutcomes` as a small helper in the same file (or a
sibling `loader_helpers.go` if one exists already — do not create a
new file just for one helper):

```go
// collectAllowedOutcomes returns the declared outcome names for a step,
// sorted ascending for determinism. Returns an empty (non-nil) slice
// when the step has no outcomes declared (terminal-routing steps,
// iteration steps that route via cursor outcomes, etc.).
func collectAllowedOutcomes(step *workflow.StepNode) []string {
    if len(step.Outcomes) == 0 {
        return []string{}
    }
    out := make([]string, 0, len(step.Outcomes))
    for name := range step.Outcomes {
        out = append(out, name)
    }
    sort.Strings(out)
    return out
}
```

Hard requirements:

- Output **must be sorted**. Map iteration order is non-deterministic
  in Go; downstream tests and adapter logic must be able to rely on a
  stable ordering.
- Empty step.Outcomes ⇒ empty (non-nil) slice. The proto serializer
  treats nil and empty `repeated` identically on the wire, but tests
  compare against `[]string{}`; emit the empty slice for clarity.
- The helper is package-private; do not export it.

### Step 5 — Engine guard remains as defense-in-depth

Do **not** modify
[internal/engine/node_step.go:340-342](../internal/engine/node_step.go#L340)
in this workstream. The unmapped-outcome guard:

```go
next, ok := n.step.Outcomes[result.Outcome]
if !ok {
    return "", fmt.Errorf("step %q produced unmapped outcome %q", n.step.Name, result.Outcome)
}
```

stays exactly as-is. The wire contract is informational for the
adapter; the engine still independently validates the returned outcome.
This is intentional belt-and-suspenders behavior — document the
intent in reviewer notes so it is not "cleaned up" later.

### Step 6 — Tests

#### Step 6.1 — Transport-level test for `AllowedOutcomes` propagation

Add to
[internal/plugin/loader_test.go](../internal/plugin/loader_test.go) a
new test:

```go
// TestLoader_PopulatesAllowedOutcomes verifies that ExecuteRequest is
// constructed with AllowedOutcomes derived from the step's declared
// outcome set, sorted ascending.
func TestLoader_PopulatesAllowedOutcomes(t *testing.T) {
    // Use the existing fake-plugin pattern in this file (search for
    // how TestLoader_ExpectedCloseLogsAtDebug stands up its plugin).
    // Capture the *pb.ExecuteRequest the fake receives via a recording
    // stub, then assert:
    //   req.AllowedOutcomes == []string{"approved", "changes_requested", "failure"}
    // for a step whose Outcomes map contains those three keys (in
    // any insertion order).
}
```

Required assertions:

- The recorded `ExecuteRequest.AllowedOutcomes` exactly equals the
  sorted outcome name list.
- Inserting outcomes in a non-sorted order on `step.Outcomes` (e.g.
  `failure`, `approved`, `changes_requested`) still yields a
  sorted-ascending slice.
- A step with no outcomes (terminal-routed) yields an empty
  (non-nil) slice.

#### Step 6.2 — Helper unit test

Add a sibling test in the same file (or `loader_test.go` if that is
where helpers live):

```go
func TestCollectAllowedOutcomes_Sorted(t *testing.T) {
    step := &workflow.StepNode{Outcomes: map[string]string{
        "failure":            "failed",
        "approved":           "done",
        "changes_requested":  "rework",
    }}
    got := collectAllowedOutcomes(step)
    want := []string{"approved", "changes_requested", "failure"}
    // assert deep-equal
}

func TestCollectAllowedOutcomes_Empty(t *testing.T) {
    got := collectAllowedOutcomes(&workflow.StepNode{})
    if got == nil { t.Fatal("expected non-nil empty slice") }
    if len(got) != 0 { t.Fatalf("got %v, want empty", got) }
}
```

#### Step 6.3 — Existing tests must remain green

- All existing `internal/plugin/...` tests pass unchanged.
- All existing `cmd/criteria-adapter-*/...` tests pass unchanged
  (the adapters ignore the new field; this is verified by passing).
- All existing `internal/engine/...` tests pass unchanged (no engine
  semantics change).
- Conformance suite (`make test-conformance`) passes — adapters that
  do not yet read `AllowedOutcomes` are still conformant.

### Step 7 — Documentation

Update [docs/plugins.md](../docs/plugins.md):

- Locate the section that documents `Execute` request fields. Add
  `allowed_outcomes` with this exact wording (or close to it):

  > **`allowed_outcomes`** *(repeated string, sorted ascending)* — The
  > set of outcome names the workflow declares for this step. Adapters
  > may use this list to constrain or validate outcome selection (e.g.
  > by exposing it to a model as a structured tool schema). Adapters
  > are not required to consume the field; the host independently
  > validates the returned outcome against the same set. The list is
  > deterministic — sorted ascending — so adapter implementations may
  > rely on stable ordering across runs.

- Note that the host validation in
  [internal/engine/node_step.go](../internal/engine/node_step.go) is
  unchanged; adapters that ignore the field continue to function.
- Cross-reference [W15](15-copilot-submit-outcome-adapter.md) as the
  first adapter consumer (Copilot `submit_outcome` tool).

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`,
`workstreams/README.md`, or any other workstream file.

## Behavior change

**No runtime behavior change.** This is a wire-contract / SDK
extension only.

Observable surface changes:

- `pb.ExecuteRequest` gains an `AllowedOutcomes []string` field.
  Plugin authors who construct `ExecuteRequest` from generated
  bindings see the new field appear; nothing breaks if they ignore
  it.
- The host now populates `AllowedOutcomes` on every `Execute` call.
  Adapters that ignore it (all of them, today) behave identically.
- SDK bump category per
  [CONTRIBUTING.md](../CONTRIBUTING.md): documented in reviewer
  notes; the actual version-source edit lives in this PR.
- No HCL surface change. No CLI flag change. No engine semantics
  change. No new sink event.

## Reuse

- `pb.ExecuteRequest` — extend, do not redesign.
- The existing `make proto` toolchain — do not introduce a new
  generation step.
- The existing test pattern in
  [internal/plugin/loader_test.go](../internal/plugin/loader_test.go)
  for stubbing a fake plugin and capturing requests (search for
  `TestLoader_ExpectedCloseLogsAtDebug` and similar W12 tests for the
  pattern).
- `workflow.StepNode.Outcomes` — read directly; do not duplicate the
  Outcomes shape elsewhere.

## Out of scope

- The `submit_outcome` tool, per-step state on the Copilot adapter,
  the reprompt loop, the strict-failure policy, fixture updates for
  tool calls — **all of that is [W15](15-copilot-submit-outcome-adapter.md)**.
- Removing the `result:` prose parsing in
  [cmd/criteria-adapter-copilot/copilot_turn.go:223](../cmd/criteria-adapter-copilot/copilot_turn.go#L223)
  — leave it intact; [W15](15-copilot-submit-outcome-adapter.md)
  removes it after the tool path is wired.
- Modifying the engine unmapped-outcome guard. It stays.
- Adding `AllowedOutcomes` to any other proto message. The contract
  is per-Execute, not session-level.
- Renaming or restructuring `pb.ExecuteRequest`. The change is
  additive only.
- Tag bumps / version-source edits beyond what
  [CONTRIBUTING.md](../CONTRIBUTING.md)'s SDK-bump policy already
  prescribes for an additive proto field.

## Files this workstream may modify

- `proto/criteria/v1/adapter_plugin.proto` — add field 4.
- `sdk/pb/criteria/v1/adapter_plugin.pb.go` (and any sibling
  `*.pb.go` regenerated by `make proto`).
- Any descriptor or registered-types file `make proto` writes to
  (e.g. `*.pb.bin`) — leave whatever the generator produces;
  do not hand-edit.
- `internal/plugin/loader.go` — populate `AllowedOutcomes` in
  `Execute`.
- `internal/plugin/loader_test.go` — new transport + helper tests.
- `docs/plugins.md` — `allowed_outcomes` field documentation.
- `sdk/CHANGELOG.md` (or `sdk/VERSION` / equivalent) — SDK bump per
  [CONTRIBUTING.md](../CONTRIBUTING.md).

This workstream may **not** edit:

- `README.md`, `PLAN.md`, `AGENTS.md`, top-level `CHANGELOG.md`,
  `workstreams/README.md`, or any other workstream file.
- `cmd/criteria-adapter-copilot/*` — the adapter consumer ships in
  [W15](15-copilot-submit-outcome-adapter.md). Do not preemptively
  wire anything.
- Any other `cmd/criteria-adapter-*/` adapter — they are unaffected.
- `internal/engine/node_step.go` — the unmapped-outcome guard
  intentionally remains unchanged.

## Tasks

- [ ] Add `repeated string allowed_outcomes = 4;` to
      `ExecuteRequest` in `adapter_plugin.proto` with the trailing
      `// permanent (W14 ...)` comment.
- [ ] Run `make proto`; commit the regenerated bindings; verify
      `make proto-check-drift` and `make proto-lint` exit 0.
- [ ] Add `collectAllowedOutcomes` helper in `internal/plugin/loader.go`.
- [ ] Wire the helper into `rpcPlugin.Execute` at line ~204.
- [ ] Add the transport-level test
      `TestLoader_PopulatesAllowedOutcomes`.
- [ ] Add the helper tests `TestCollectAllowedOutcomes_Sorted` and
      `TestCollectAllowedOutcomes_Empty`.
- [ ] Update `docs/plugins.md` with the `allowed_outcomes` field
      documentation and cross-reference to W15.
- [ ] Bump the SDK version per [CONTRIBUTING.md](../CONTRIBUTING.md);
      capture the bump rationale in reviewer notes.
- [ ] `make build`, `make plugins`, `make test`, `make
      test-conformance`, `make ci` all green.

## Exit criteria

- `pb.ExecuteRequest` has the `AllowedOutcomes []string` field.
- `make proto-check-drift` exits 0.
- `make proto-lint` exits 0.
- The host populates `AllowedOutcomes` on every `Execute` call,
  sorted ascending, derived from `step.Outcomes` keys.
- A transport-level test asserts propagation.
- Helper unit tests assert sorting and the empty-slice case.
- All existing tests (`make test`, `make test-conformance`) pass
  unchanged.
- `docs/plugins.md` documents the new field.
- SDK CHANGELOG / version source updated; rationale recorded in
  reviewer notes.
- `make ci` green.

## Tests

Two helper unit tests + one transport propagation test. No new
end-to-end tests — this workstream is wire-only and the engine
semantics are unchanged. Engine integration of the new field happens
indirectly via [W15](15-copilot-submit-outcome-adapter.md)'s adapter
tests.

## Risks

| Risk | Mitigation |
|---|---|
| `make proto` produces unrelated drift in generated files (timestamps, reorder) | Inspect the diff; revert any non-required changes; if the generator is non-deterministic, document the expected diff in reviewer notes and fix the generator config in a follow-up workstream rather than letting noise into this PR. |
| The SDK-bump policy in `CONTRIBUTING.md` is ambiguous for "additive proto field" | Default to the policy's most conservative tier (treat as breaking for plugin authors who hand-construct requests). Document the choice in reviewer notes. The cleanup gate ([W16](16-phase2-cleanup-gate.md)) confirms the bump landed. |
| A downstream adapter author already used field tag `4` on `ExecuteRequest` in an out-of-tree fork | The repo controls the canonical proto. Forks must re-tag. Do not avoid tag `4` to dodge a hypothetical fork. |
| `collectAllowedOutcomes` for iteration steps (those that route via `routeIteratingStep`) returns the wrong set | Iteration steps still have `step.Outcomes` populated for the iteration cursor outcomes (`all_succeeded`, `any_failed`, etc.) — those are real outcomes the host validates against. Emit them. The Copilot adapter does not run as the iteration cursor's adapter, so this is benign. |
| The proto change forces a major SDK version bump that is disproportionate to the change | The bump policy is repo-defined. Follow it. If the cost is high, raise a docs-only follow-up to soften future additive-field bump guidance — out of scope here. |
| Existing `make test-conformance` lanes break because conformance fixtures construct `ExecuteRequest` manually with explicit field initialization that fails on unrecognized fields | Generated Go does not break on field addition; existing fixtures are forward-compatible. If conformance fails, root-cause before merge. |
