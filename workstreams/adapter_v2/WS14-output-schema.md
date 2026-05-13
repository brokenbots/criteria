# WS14 — Output schema + compile-time output-reference validation + sensitive output taint

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS09](WS09-environment-block-and-secret-taint.md). · **Unblocks:** clearer error messages for downstream output usage; closes a known v1 gap.

## Context

`README.md` D22, D63. v2's `InfoResponse` carries `output_schema`. The compiler now validates `steps.X.outputs.Y` references against the adapter's declared output schema, and honors the `sensitive: true` flag on output fields (which auto-taints downstream references per D63).

## Prerequisites

WS02 (proto v2), WS09 (taint compiler with `OriginRef` plumbing).

## In scope

### Step 1 — Wire `output_schema` through compile

`workflow/compile_steps_adapter_ref.go`:

After resolving the adapter manifest for a step, expose `manifest.OutputSchema` on the `StepNode` so subsequent passes can validate `steps.X.outputs.Y`.

### Step 2 — Output-reference validation pass

`workflow/compile_output_refs.go` (new):

Walk every HCL expression that references `steps.X.outputs.Y`. For each:

1. Resolve the target step's adapter manifest.
2. Confirm `Y` is in `manifest.OutputSchema.Fields`.
3. If not, emit a diagnostic with file:line and suggested field names (Levenshtein-distance-sorted).

### Step 3 — Sensitive-output taint hook

In the WS09 taint pass: when a value originates from `steps.X.outputs.Y` where `manifest.OutputSchema.Fields[Y].Sensitive == true`, the value is tainted. Existing WS09 propagation handles the rest.

### Step 4 — Runtime registration

When an adapter emits an `ExecuteResult` whose outputs include a sensitive field, the host's session code calls `redaction.Register(value)` (the registry from WS13) for that field's value before propagating it to downstream steps.

### Step 5 — Tests

- Unit: every output-schema validation rule.
- Compile-error golden tests for misspelled output references.
- Integration: a workflow uses an adapter declaring `token: { sensitive: true }`; another step references `step.X.outputs.token`; assert the value is redacted in logs and that an attempt to interpolate it into a `config` field is a compile error.

## Out of scope

- The taint compiler itself — WS09.
- Redaction infrastructure — WS13.
- SDK manifest emission — WS23–WS25.

## Behavior change

**Yes** — invalid `steps.X.outputs.Y` references are caught at compile time rather than failing silently at runtime. Sensitive outputs auto-taint.

## Tests required

- `workflow/compile_output_refs_test.go`.
- Updates to existing fixtures that reference outputs.

## Exit criteria

- All output references validated at compile.
- Sensitive outputs taint correctly.

## Files this workstream may modify

- `workflow/compile_output_refs.go` *(new)* + tests.
- Small additions to `workflow/compile_steps_adapter_ref.go` and `workflow/compile_taint.go`.

## Files this workstream may NOT edit

- `proto/criteria/v2/` — WS02.
- `internal/adapter/secrets/` — WS13.
- Other workstream files.
