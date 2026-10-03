# SDK Changelog

All notable changes to the `github.com/brokenbots/criteria/sdk` module are
documented here. The SDK follows semantic versioning: additive changes are
non-breaking; any change to an exported surface requires a major-version bump.

---

## [v0.6.0] — 2026-10-03

### Added — KB-104: `OrchestratorService` + `RunMetadata`/`AdapterLifecycle` envelope arms

- **New proto service**: `criteria.v1.OrchestratorService` in
  `proto/criteria/v1/orchestrator.proto` (new file), carrying
  `SubscribeRunEvents`, `ListActiveRuns`, and `CancelRun` with their request /
  response messages. Generated Go bindings in
  `sdk/pb/criteria/v1/orchestrator.pb.go` and Connect handler stubs in
  `sdk/pb/criteria/v1/criteriav1connect/orchestrator.connect.go`. The proto
  file is byte-identical to the orchestrator's shipped proto copy (castle),
  which is the merge-gate authority for the service, so castle's operator
  API compiles against the released SDK without a local fork.
- **New envelope arms** in `proto/criteria/v1/events.proto` (oneof fields
  34/35/36, previously free), byte-identical to the castle fork's copy:
  `RunMetadata run_metadata = 34` (kind `run.metadata`; fields `ticket`,
  `repo_url`, `pr_url`, each "only overwrites when non-empty"), and the
  adapter-pod reconcile pair `AdapterLifecycleProvisionWanted` = 35 /
  `AdapterLifecycleReleased` = 36 (kinds `adapter.lifecycle.provision_wanted`
  / `adapter.lifecycle.released`; shared shape `scope_instance_id`,
  `shim_listen_address`, `token_ref`, `reason`, `ttl`).
- **SDK aliases**: `Envelope_RunMetadata`,
  `Envelope_AdapterLifecycleProvisionWanted`, `Envelope_AdapterLifecycleReleased`
  in `events.go`; payload aliases `RunMetadata` (`payloads_run.go`) and
  `AdapterLifecycleProvisionWanted` / `AdapterLifecycleReleased`
  (`payloads_adapter.go`); `OrchestratorServiceClient` / `OrchestratorServiceHandler`
  type aliases plus their `New*` constructors and procedure-name constants in
  `connect.go`; `TypeString`/`IsTerminal` discriminator strings
  (`run.metadata`, `adapter.lifecycle.*`) in the kind mapping. None of these
  are new request fields: run context rides the generic `RunMetadata`
  envelope (CRI-131 ruling — `CreateRunRequest`/`Run` gain no
  `ticket`/`repo_url`/`pr_url`).
- **Conformance**: new `RunMetadataRoundTrip` (metadata envelopes persist with
  fields and submission order preserved) and `AdapterPodReconcileEventsRoundTrip`
  (provision_wanted/released pair persists with scope_instance_id /
  shim_listen_address / token_ref preserved, submission ordering stable, and
  `IsTerminal()==false`) cases; the descriptor-driven `EnvelopeRoundTrip` walk
  covers the new arms automatically.
- **Backward compatibility**: consumers unaware of fields 34–36 ignore the new
  envelope types; readers require no changes. Orchestrator implementations that
  previously vendored the fork's proto can now delete it and re-pin this SDK.

### Bump rationale

Adding oneof fields 34–36 changes the event field numbers, which AGENTS.md's
breaking-change policy classifies as a breaking SDK change ("Any change to
the `Subject`/`ServiceHandler` surface or to event field numbers is a
breaking SDK change and requires an SDK major-version bump"). Per this
changelog's established pre-1.0 convention (see v0.3.0 and v0.4.0), pre-1.0
additive surface is recorded as a minor bump: **v0.6.0**. SDK consumers
regenerate protobuf bindings from the updated `.proto` files to gain the new
service and messages; no existing reader or writer code is affected. Castle
follows up by re-pinning this SDK, switching promotion to envelope-only, and
deleting its criteria-sdk fork (KB-102); a later criteria PR drops the CRI-131
first-class request/run fields.

[v0.6.0]: https://github.com/brokenbots/criteria/releases/tag/v0.6.0

---

## [v0.4.0] — 2026-09-21

### Added — CRI-278: `WorkflowGraphs` event (oneof field 37)

- **New proto message**: `WorkflowGraphs` on `pb.Envelope` (oneof field
  number 37) in `proto/criteria/v1/events.proto`, carrying
  `repeated SubworkflowGraph subworkflows` where each `SubworkflowGraph` has
  `string name` (1), `string source_path` (2), and `string body` (3). The
  message definitions are byte-identical to the orchestrator's shipped proto
  copy (castle), the merge-gate authority for the type, so the event survives
  server ingest verbatim.
- **Emission contract**: the agent emits exactly one `WorkflowGraphs` envelope
  per run, immediately after compiling the run's workflow and before the
  engine starts, so the event lands at or before `RunStarted`. Emitted
  unconditionally — a run whose workflow declares no subworkflows carries an
  empty layers array. Resend-replaces: consumers use the most recent
  `workflow.graphs` event per run.
- **Wire shape note**: `SubworkflowGraph.body` is a string on the wire; the
  agent serializes the layer's complete compiled body graph as JSON into it,
  built from the already-compiled in-memory graph (no second compilation
  pass). Local ND-JSON (`--events-file`) renders the same layers as a bare
  JSON array matching `criteria compile --format json`'s `subworkflows[]`
  shape (nested `body.subworkflows` inline).
- **Backward compatibility**: consumers unaware of field 37 ignore the new
  envelope type; readers require no changes. A server predating the field
  stores the event verbatim or drops it silently; neither is harmful.

### Bump rationale

Adding oneof field 37 changes the event field numbers, which AGENTS.md's
breaking-change policy classifies as a breaking SDK change ("Any change to
the `Subject`/`ServiceHandler` surface or to event field numbers is a
breaking SDK change and requires an SDK major-version bump"). Per this
changelog's established pre-1.0 convention (see v0.3.0), pre-1.0 breaking
changes are recorded as a minor bump: **v0.4.0**. SDK consumers regenerate
protobuf bindings from the updated `.proto` files to gain the new message;
no existing reader or writer code is affected.

[v0.4.0]: https://github.com/brokenbots/criteria/releases/tag/v0.4.0

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
  no runtime semantics change here. The first adapter consumer is
  the Copilot `submit_outcome` tool.
- **Backward compatibility**: existing adapters that ignore the new field
  continue to function unchanged. Adapters built against older generated
  bindings silently ignore field 4 when decoding, though they may drop it if
  they re-serialize the message.

### Bump rationale

Adding a field to `ExecuteRequest` is an additive proto change. Additive
changes are non-breaking at minor or patch level. This change is treated as a
**minor** bump (new
observable field on a request message that plugin authors hand-constructing
`ExecuteRequest` will see in the generated struct). The bump ships in `v0.2.0`
alongside the Phase 1 + Phase 2 release.

[v0.2.0]: https://github.com/brokenbots/criteria/releases/tag/v0.2.0
