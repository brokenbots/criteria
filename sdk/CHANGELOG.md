# SDK Changelog

All notable changes to the `github.com/brokenbots/criteria/sdk` module are
documented here. The SDK follows the bump policy in
[CONTRIBUTING.md](../CONTRIBUTING.md).

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
