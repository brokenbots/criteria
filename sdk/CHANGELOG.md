# SDK Changelog

All notable changes to the `github.com/brokenbots/criteria/sdk` module are
documented here. The SDK follows the bump policy in
[CONTRIBUTING.md](../CONTRIBUTING.md).

---

## [v0.3.0] — 2026-05-03

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

Adding a new message field to `Envelope` is additive at the proto level. Adding
a method to the `run.Sink` interface is a breaking change for external
implementations but acceptable for pre-1.0 SDK (no strict API stability
guarantee). The bump is treated as a **minor** version bump (`v0.3.0`). SDK
consumers must update their `run.Sink` implementations to compile against
`v0.3.0+`.

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
