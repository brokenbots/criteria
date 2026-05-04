# SDK Changelog

All notable changes to the `github.com/brokenbots/criteria/sdk` module are
documented here. The SDK follows the bump policy in
[CONTRIBUTING.md](../CONTRIBUTING.md).

---

## [v0.3.0] — 2026-05-03

### Changed — Phase 3 W11: Proto field rename `agent_name` → `adapter`

- **Proto field rename**: `pb.StepEntered.agent_name` → `pb.StepEntered.adapter` in `proto/criteria/v1/events.proto`.
  - Field number 2 (unchanged for wire compatibility; message definition uses implicit field numbering and `adapter` occupies the same wire slot).
  - Generated Go binding: `StepEntered.Adapter string` (previously `AgentName`).
  - Orchestrators and SDKs reading by field name must regenerate protobuf bindings (`protoc` with the updated `.proto` file) to update generated code.
  - SDKs that read by field number (direct proto parsing, not generated bindings) are unaffected at runtime; readers continue to work. Writers must update to emit the new field name.
- **Backward compatibility**: Field numbers stable; wire format unchanged. Old readers consuming by field number continue to work. **Old writers must upgrade** — clients built against v0.2.0 bindings emitting `agent_name` will not match the v0.3.0 field name.
- **Scope**: This is a wire-format **stable** breaking change. Only affected for code that regenerates protos or hand-constructs `StepEntered` messages. Pre-1.0 projects treat as a **minor** version bump.

### Added — Phase 3 W09: Output blocks and `OnRunOutputs` interface method

- **New proto message**: `RunOutputs` on `pb.Envelope` (field number 33) in
  `proto/criteria/v1/events.proto`. Shape: `repeated Output outputs` where each
  `Output` contains `string name` (output declaration name), `string value`
  (JSON-stringified cty value for transport), and `string declared_type` (type
  string if set, empty otherwise). All fields marked permanent (wire format locked).
- **New sink interface method**: `OnRunOutputs([]map[string]string)` on
  `run.Sink` in `internal/run/sink.go`. External SDK consumers implementing
  their own `run.Sink` interface must add this method (even as an empty stub)
  to avoid compilation errors. The method receives output name→value pairs as
  the workflow enters terminal state (after all steps complete).
- **Wire shape**: output values are currently stringified (via `cty/json`
  marshaling) for wire transport. Consumers must JSON-parse the `value` field
  to recover type structure. A future SDK upgrade may add a `typed_value` field
  with `google.protobuf.Value` for native type preservation.
- **Backward compatibility**: existing clients unaware of field 33 simply ignore
  the new `run.outputs` envelope type. New clients emit `RunOutputs` before
  `RunCompleted` (observable in event stream ordering).

### Bump rationale

Phase 3 W11 introduces a proto field rename (breaking for generated code) but the wire format remains stable (same field number). Phase 3 W09 adds a new message field (additive) and an interface method (breaking for external implementors). The combined version is treated as a **minor** bump for pre-1.0 (`v0.3.0`). SDK consumers must:
1. Regenerate protobuf bindings from the updated `.proto` files.
2. Update any external `run.Sink` implementations to add the `OnRunOutputs` method.

[v0.3.0]: https://github.com/brokenbots/criteria/releases/tag/v0.3.0

---

## [v0.2.0] — 2026-05-02

### Added — Phase 2 W14: `allowed_outcomes` field on `ExecuteRequest`

- **New field**: `allowed_outcomes` (field number 4, `repeated string`) on
  `pb.ExecuteRequest` in `proto/criteria/v1/adapter_plugin.proto`.
- **Generated Go field**: `AllowedOutcomes []string` on `*pb.ExecuteRequest`.
- **Host behaviour**: the host now populates `AllowedOutcomes` on every
  `Execute` RPC call, derived from the step's declared outcome set (the keys of
  `workflow.StepNode.Outcomes`), sorted ascending for determinism.
- **Adapter behaviour**: adapters may consume `AllowedOutcomes` to constrain or
  validate outcome selection (e.g. by exposing the list to a model as a
  structured tool schema). Adapters are **not** required to consume the field;
  no runtime semantics change in this workstream. The first adapter consumer is
  the Copilot `submit_outcome` tool, shipping in
  [W15](../workstreams/15-copilot-submit-outcome-adapter.md).
- **Backward compatibility**: existing adapters that ignore the new field
  continue to function unchanged. Adapters built against older generated
  bindings silently ignore field 4 when decoding, though they may drop it if
  they re-serialize the message.

### Bump rationale

Adding a field to `ExecuteRequest` is an additive proto change. Per
[CONTRIBUTING.md](../CONTRIBUTING.md), additive changes are non-breaking at
minor or patch level. This change is treated as a **minor** bump (new
observable field on a request message that plugin authors hand-constructing
`ExecuteRequest` will see in the generated struct). The bump ships in `v0.2.0`
alongside the Phase 1 + Phase 2 release.

[v0.2.0]: https://github.com/brokenbots/criteria/releases/tag/v0.2.0
