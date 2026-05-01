# Workstream 11 — Reviewer outcome aliasing (UF#03)

> **Status: CANCELLED (2026-04-30).**
> This workstream has been removed from Phase 2 scope. UF#03 is now
> addressed at the source by the new tool-call finalization workstreams
> ([W14](14-copilot-tool-call-wire-contract.md) +
> [W15](15-copilot-submit-outcome-adapter.md)): once the Copilot adapter
> finalizes via a structured `submit_outcome` tool call against the
> step's declared outcome set, host-side outcome aliasing is no longer
> the motivating user pain. UF#03 stays accounted for via W14/W15 in
> the cleanup gate's user-feedback ledger.
>
> **Do not execute this workstream.** The historical scope is preserved
> below for context only. If a host-side alias map is wanted later (for
> non-Copilot adapters), file a fresh workstream against this design.

---

**Owner:** Workstream executor · **Depends on:** none.

## Context

Deferred user-feedback item #03 (preserved in git history at commit
`4e4a357`,
`user_feedback/03-stabilize-reviewer-outcome-handling-user-story.txt`):

> Current pain:
> - Reviewer emitted needs_review, but the workflow had no mapped transition for that outcome.
> - The run failed with unmapped outcome, even though the intent was clearly "continue iteration".

Today, when an adapter returns an outcome that has no matching
`outcome { ... }` block on the step, the engine fails the run with:

> `step "<name>" produced unmapped outcome "<X>"`
> ([internal/engine/node_step.go:334](../internal/engine/node_step.go#L334))

This is the right default for type safety, but it is too brittle for
agent-driven runs where the adapter can produce semantically
equivalent outcomes (`needs_review`, `changes_requested`,
`requires_changes`) that the workflow author intended to handle the
same way.

Two complementary mechanisms:

1. **Optional `outcome_aliases` block** on a step (or workflow-wide
   default) that normalizes adapter outcomes before transition
   lookup.
2. **Better error message** when an outcome is still unmapped after
   alias resolution: include the nearest known outcomes and a
   suggested transition stub.

A new strict-mode flag preserves the current hard-fail behavior for
teams that want it.

## Prerequisites

- `make ci` green on `main`.
- Familiarity with
  [internal/engine/node_step.go:332-336](../internal/engine/node_step.go#L332-L336)
  (the existing transition lookup).
- Familiarity with [workflow/schema.go](../workflow/schema.go)
  StepSpec and StepNode types.
- Familiarity with
  [workflow/compile_steps.go](../workflow/compile_steps.go) for the
  decode pattern.

## In scope

### Step 1 — Schema

Add to [workflow/schema.go](../workflow/schema.go):

- A new optional `OutcomeAliasesSpec` (or simpler — a
  `map[string]string` field on StepSpec):

```go
// On StepSpec (the HCL-decoded shape):
OutcomeAliases map[string]string `hcl:"outcome_aliases,optional"`
```

- The map key is the *adapter-produced* outcome name; the value is
  the *workflow-declared* outcome name (the one matching an
  `outcome { ... }` block).
- The HCL type must be a string-to-string map. The HCL surface looks
  like:

  ```hcl
  step "review" {
    agent = "reviewer"
    outcome_aliases = {
      "needs_review"      = "changes_requested"
      "requires_changes"  = "changes_requested"
    }

    outcome "approved"          { transition_to = "done" }
    outcome "changes_requested" { transition_to = "execute" }
    outcome "failure"           { transition_to = "failed" }
  }
  ```

- Add `OutcomeAliases map[string]string` to the compiled `StepNode`
  struct (line 254 onward in schema.go).
- Add a workflow-level optional field:
  `WorkflowDefaults.OutcomeAliases map[string]string` for global
  defaults that apply to every step unless the step itself declares
  an alias. Plumb this through the workflow-level decode.

The merge precedence:
1. Step-local `outcome_aliases` (highest priority)
2. Workflow-level defaults
3. No alias (literal lookup)

### Step 2 — Add `strict_outcomes` policy flag

Add to the policy block (similar to `max_total_steps`):

```hcl
policy {
  strict_outcomes = true   # default: false
}
```

When `strict_outcomes = true`, the alias map is *ignored* and
unmapped outcomes hard-fail with the existing error. This is the
"opt-in to current behavior" knob for teams that prefer hard
typing.

When `strict_outcomes = false` (or omitted), aliases apply.

### Step 3 — Compile

In [workflow/compile_steps.go](../workflow/compile_steps.go):

- Decode `outcome_aliases` from each step block.
- Decode the workflow-level defaults.
- Resolve and copy onto `StepNode.OutcomeAliases` per the precedence
  in Step 1.
- Validate at compile time:
  - The *target* of every alias (the right-hand side of the map)
    must match a declared `outcome { ... }` block on the same step.
    A missing target is a compile error:
    `step "<name>": outcome alias "<key>" -> "<value>" but no outcome block named "<value>" is declared`.
  - An alias whose key is identical to a declared outcome name is a
    compile warning (not error): the alias would never fire because
    the declared outcome wins.

### Step 4 — Runtime alias resolution

In [internal/engine/node_step.go](../internal/engine/node_step.go),
update the unmapped-outcome lookup (lines 332-336):

```go
outcome := result.Outcome

if !n.graph.Policy.StrictOutcomes {
    if alias, ok := n.step.OutcomeAliases[outcome]; ok {
        // Emit a sink event so operators see the alias firing.
        deps.Sink.OnStepOutcomeAliased(n.step.Name, outcome, alias)
        outcome = alias
    }
}

next, ok := n.step.Outcomes[outcome]
if !ok {
    // The new improved error path. See Step 5.
    return "", buildUnmappedOutcomeError(n.step, result.Outcome, outcome)
}

// Note: OnStepTransition uses the original adapter-produced outcome
// for visibility; the transition is to the alias-resolved target.
deps.Sink.OnStepTransition(n.step.Name, next, result.Outcome)
return next, nil
```

Add `OnStepOutcomeAliased(node, fromOutcome, toOutcome string)` to
the [Sink interface](../internal/engine/engine.go) (the section
introduced around line 27 of `engine.go`). Default
implementations in any test sinks need a no-op stub. The
console / events / Local sinks need to render the alias event
(small change in each sink — verify by grep).

### Step 5 — Improved unmapped-outcome error

When the lookup fails (after alias resolution), emit a richer error:

```go
func buildUnmappedOutcomeError(step *workflow.StepNode, originalOutcome, resolvedOutcome string) error {
    // List all declared outcome names for the step.
    declared := make([]string, 0, len(step.Outcomes))
    for name := range step.Outcomes {
        declared = append(declared, name)
    }
    sort.Strings(declared)

    // Find the closest match by Levenshtein or simple prefix.
    nearest := findNearestOutcome(resolvedOutcome, declared)

    // Build a suggested HCL stub.
    stub := fmt.Sprintf(`outcome %q { transition_to = "<state>" }`, resolvedOutcome)

    msg := fmt.Sprintf(
        "step %q produced unmapped outcome %q (declared outcomes: %s).\n"+
        "  Nearest declared outcome: %q.\n"+
        "  To handle this outcome, add to the step:\n    %s\n"+
        "  Or alias it:\n    outcome_aliases = { %q = %q }",
        step.Name, originalOutcome,
        strings.Join(declared, ", "),
        nearest,
        stub,
        originalOutcome, nearest,
    )
    return errors.New(msg)
}
```

`findNearestOutcome` can use a simple Levenshtein implementation
(small helper in `internal/engine/`). If no declared outcome exists,
return an empty string and adjust the message accordingly.

### Step 6 — Tests

In [internal/engine/engine_test.go](../internal/engine/engine_test.go)
or a sibling:

- `TestOutcomeAlias_StepLocal` — workflow with
  `outcome_aliases = { "needs_review" = "changes_requested" }`;
  adapter returns `needs_review`; assert run transitions to the
  `changes_requested` target and `OnStepOutcomeAliased` fires.
- `TestOutcomeAlias_WorkflowDefault` — workflow-level alias applies
  to a step that does not declare its own.
- `TestOutcomeAlias_StepOverridesWorkflow` — step-local alias takes
  precedence over a conflicting workflow-level alias.
- `TestOutcomeAlias_StrictModeIgnoresAlias` —
  `policy { strict_outcomes = true }` causes unmapped outcomes to
  hard-fail even when an alias is declared.
- `TestUnmappedOutcomeError_IncludesSuggestion` — the error text
  contains the declared outcomes and a suggested stub.
- `TestOutcomeAlias_IdentityWarning` — compile warning fires when an
  alias key equals a declared outcome name.

In `workflow/compile_steps_test.go`:

- `TestCompileOutcomeAlias_MissingTarget` — compile error when an
  alias's target outcome is not declared.
- `TestCompileOutcomeAlias_StrictModeOK` — compile succeeds even
  with `strict_outcomes = true` and an alias declared (the alias is
  inert at runtime but valid syntactically).

### Step 7 — Documentation

Update [docs/workflow.md](../docs/workflow.md):

- Add an "Outcome aliases" section to the step block reference.
- Document the merge precedence (step > workflow > literal).
- Document `strict_outcomes` in the policy block reference.
- Add a worked example showing the canonical reviewer-loop pattern
  where `needs_review` aliases to `changes_requested`.

Update [docs/plugins.md](../docs/plugins.md) if it discusses outcome
shaping for the Copilot adapter — at minimum, the existing reference
to "RESULT: needs_review" should mention that workflows can alias it.

Do **not** edit `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`.

## Behavior change

**Yes.**

- New optional HCL field `outcome_aliases` on step blocks and on a
  new workflow-level defaults block (TBD whether this lives on
  `workflow { ... }` directly or in a sub-block — pick the simpler
  path and document).
- New optional HCL field `policy.strict_outcomes` (default `false`).
- New sink event `OnStepOutcomeAliased(node, fromOutcome,
  toOutcome)`. The `Sink` interface gains a method; existing
  implementers must add a (no-op or rendering) implementation.
- The unmapped-outcome error message text changes substantially.
  Consumers that string-matched the old `step "<name>" produced
  unmapped outcome "<X>"` pattern need to update; the prefix
  `step "<name>" produced unmapped outcome "<X>"` is preserved as
  the first line of the new message so most matchers still work.
- New compile error: `outcome alias "<key>" -> "<value>" but no
  outcome block named "<value>"`.
- New compile warning: alias key shadows a declared outcome.
- Default behavior for *existing* workflows (no `outcome_aliases`,
  no `strict_outcomes`): identical to today. Aliases must be
  declared to take effect.

## Reuse

- Existing `step.Outcomes map[string]string` lookup. The alias map
  layers on top; do not refactor the lookup.
- Existing `Sink` interface for emitting the alias event.
- Existing test harness for engine-level workflow tests
  (`internal/engine/engine_test.go`).
- Existing diagnostic infrastructure in `workflow/compile_steps.go`
  for the missing-target error.

## Out of scope

- Globbing aliases (e.g.
  `"needs_*" = "changes_requested"`). Exact-key only.
- Regex-based aliases. Out.
- Adapter-declared aliases (the adapter advertising "I can produce
  outcomes A, B, C; please alias A to X"). The host-side approach is
  sufficient.
- Changing the iteration / for_each outcome shaping
  (`all_succeeded` / `any_failed`). Iteration outcomes are not
  routed through the alias map; document this explicitly in
  `docs/workflow.md`.
- Aliases on approval / wait nodes. Approval outcomes are
  hard-coded `approved` / `rejected`; not aliasable. Wait outcomes
  come from `payload["outcome"]`; aliases on wait nodes can be a
  follow-up if asked for.

## Files this workstream may modify

- `workflow/schema.go` — add `OutcomeAliases` to step types and
  workflow defaults; add `StrictOutcomes` to policy.
- `workflow/compile_steps.go` — decode + validate aliases; merge
  precedence.
- `workflow/compile.go` — workflow-level defaults decode (if added).
- `workflow/compile_steps_test.go` — compile tests.
- `internal/engine/engine.go` — add `OnStepOutcomeAliased` to Sink.
- `internal/engine/node_step.go:332-336` — alias resolution.
- `internal/engine/engine_test.go` — runtime tests.
- All sink implementations (locate via grep for `OnStepTransition`):
  - `internal/run/console_sink.go` (concise mode rendering).
  - `internal/run/local_sink.go` or equivalent.
  - `internal/transport/server/*.go` (events forwarded to
    orchestrator).
  - `events/*.go` (event-stream serialization).
  - Test sinks under `internal/engine/*_test.go` (no-op stubs).
- `docs/workflow.md` — outcome aliases reference.
- `docs/plugins.md` — Copilot reviewer-loop note.

This workstream may **not** edit `README.md`, `PLAN.md`, `AGENTS.md`,
`CHANGELOG.md`, `workstreams/README.md`, or any other workstream file.
It may **not** modify the wire contract proto (no proto change is
needed — the alias event is host-internal).

## Tasks

- [ ] Add `OutcomeAliases` to step types in `workflow/schema.go`.
- [ ] Add `StrictOutcomes` to policy schema.
- [ ] Decode + merge aliases (step > workflow > none).
- [ ] Add the `OnStepOutcomeAliased` sink hook with no-op default
      implementations across all sinks.
- [ ] Implement runtime alias resolution in `node_step.go`.
- [ ] Implement the improved unmapped-outcome error.
- [ ] Add the compile-time validation: missing-target error and
      identity-shadow warning.
- [ ] Add unit tests per Step 6.
- [ ] Update `docs/workflow.md` and `docs/plugins.md`.
- [ ] `make build`, `make plugins`, `make test`, `make ci` all green.

## Exit criteria

- `outcome_aliases = { "needs_review" = "changes_requested" }`
  decodes, compiles, and at runtime causes `needs_review` to follow
  the `changes_requested` transition.
- `policy { strict_outcomes = true }` causes unmapped outcomes to
  hard-fail even when aliases are declared.
- Unmapped-outcome error text includes declared outcomes and a
  suggested HCL stub.
- All new compile-time validations fire correctly.
- All existing tests pass unchanged.
- `make ci` green.

## Tests

Six runtime tests + two compile tests per Step 6. Sink-implementer
no-op tests (one per sink) confirm the new method does not break
sink construction.

## Risks

| Risk | Mitigation |
|---|---|
| Adding a method to the `Sink` interface breaks every existing implementation | The change is mechanical: every sink gains a no-op or render-this-event method. Coordinate with [W12](12-lifecycle-log-clarity.md) if that workstream is also touching sinks. Do this change in a single PR; don't split. |
| The "nearest outcome" suggestion is unhelpful (e.g. picks "failure" for "needs_review") | A simple Levenshtein-by-prefix match is fine; perfection is not required. Document the heuristic in the error-builder code comment. |
| Workflow-level defaults block (in `workflow { ... }` directly vs. a sub-block) is ambiguous | Pick the simpler path: an attribute on `workflow { ... }` like `default_outcome_aliases = { ... }`. If the schema rejects map attributes at that scope, fall back to a `defaults { outcome_aliases = { ... } }` sub-block. Document the choice in reviewer notes. |
| The alias event clutters concise-mode output | Render only when `--output verbose` is on (Phase 3 ships verbose mode). For concise mode, suppress the event. Document. |
| Strict-mode behavior surprises operators who set `strict_outcomes = true` and have aliases declared | The compile-time validation catches the dangerous half (missing alias targets). At runtime, strict mode just means the alias is inert; this matches the documented contract. Add the test for it. |
