# WS02 — Protocol v2: `.proto` file and generated bindings

**Phase:** Adapter v2 · **Track:** Foundation · **Owner:** Workstream executor · **Depends on:** [WS01](WS01-terminology-unification.md) (renames complete). · **Unblocks:** [WS03](WS03-host-v2-wire.md) (host wire), [WS14–WS19](WS14-output-schema.md) (protocol features), every SDK and adapter migration WS.

## Context

The v1 proto at [`proto/criteria/v1/adapter_plugin.proto`](../../proto/criteria/v1/adapter_plugin.proto) defines `AdapterService` (renamed in WS01) with five RPCs: `Info`, `OpenSession`, `Execute` (streaming), `Permit`, `CloseSession`. v2 (see `README.md` D22–D27) is a clean break with:

- New `output_schema` on `InfoResponse`.
- Dedicated `Log` server-stream RPC, separating log lines from semantic Execute events.
- Bidirectional `Permissions` stream replacing the unary `Permit` callback.
- New lifecycle ops: `Pause`, `Resume`, `Snapshot`, `Restore`, `Inspect`.
- A separate `secrets` field on `OpenSessionRequest` (and `secret_inputs` on `ExecuteRequest`) tagged with a custom `(criteria.sensitive) = true` field option for structural redaction.
- Chunked framing + explicit heartbeats so remote-friendly transports (WS20–WS22) can build on the same wire.
- **Capability negotiation** via `InfoResponse.supported_features` (D76) so the host can discover whether an adapter implements optional ops (Pause, Snapshot, Inspect) without probing.
- **Reserved field-number ranges** on every message so additive changes after WS41 (proto extraction) don't collide with field numbers used in private forks.

This workstream **only authors the proto + generated bindings + unit tests**. Host integration is WS03; SDK integration is WS23–WS25. Adapter migration follows.

## Prerequisites

- WS01 merged: `AdapterService`, `AdapterName`, and `internal/adapter/` exist.
- `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` versions documented in the repo's tooling files; the executor verifies these match before regenerating bindings.
- Familiarity with the protobuf "custom options" pattern for declaring `(criteria.sensitive) = true`.

## In scope

### Step 1 — Define the `criteria.sensitive` field option

Create `proto/criteria/v2/options.proto`:

```proto
syntax = "proto3";
package criteria.v2;
option go_package = "github.com/brokenbots/criteria/proto/criteria/v2;criteriav2";

import "google/protobuf/descriptor.proto";

extend google.protobuf.FieldOptions {
  // Marks a field as carrying secret material. The host's log pipeline, the
  // SDK's redaction-aware logger, and reflection-driven debug/audit code all
  // honor this and either mask or refuse to serialize the value.
  bool sensitive = 70000;
}
```

The extension number `70000` is in the user-defined range; document the choice in the file's leading comment.

### Step 2 — Define the v2 service

Create `proto/criteria/v2/adapter.proto`:

```proto
syntax = "proto3";
package criteria.v2;
option go_package = "github.com/brokenbots/criteria/proto/criteria/v2;criteriav2";

import "criteria/v2/options.proto";

service AdapterService {
  rpc Info(InfoRequest)             returns (InfoResponse);
  rpc OpenSession(OpenSessionRequest) returns (OpenSessionResponse);
  rpc Execute(ExecuteRequest)        returns (stream ExecuteEvent);
  rpc Log(LogRequest)                returns (stream LogEvent);
  rpc Permissions(stream PermissionEvent) returns (stream PermissionDecision);
  rpc Pause(PauseRequest)            returns (PauseResponse);
  rpc Resume(ResumeRequest)          returns (ResumeResponse);
  rpc Snapshot(SnapshotRequest)      returns (SnapshotResponse);
  rpc Restore(RestoreRequest)        returns (RestoreResponse);
  rpc Inspect(InspectRequest)        returns (InspectResponse);
  rpc CloseSession(CloseSessionRequest) returns (CloseSessionResponse);
}
```

### Step 3 — Define messages

Author the message types. Key shape decisions (see `README.md` D22–D27 plus the v2 hardening decisions D76–D81):

**General rule — reserved ranges (D77).** Every message reserves `100 to 999` for future additive fields:

```proto
message InfoResponse {
  // ... numbered fields 1..N ...
  reserved 100 to 999;
}
```

This block stays untouched by anyone editing the proto, so additions later land in a known-safe range and private/experimental forks can use the high range without colliding with the contract.

**Per-message shapes:**

- **`InfoResponse`** carries `name`, `version`, `description`, `capabilities`, `platforms`, `sdk_protocol_version`, `source_url`, `config_schema`, `input_schema`, **`output_schema`** (new), `secrets` (declared secret names with descriptions), `permissions`, `compatible_environments`, `container_image` (optional, see D12b). **New v2 fields (D76, D78):**
  - `repeated string supported_features` — capability list. Well-known values: `pause`, `resume`, `snapshot`, `restore`, `inspect`. Host gates UI/behavior on this list rather than probing for `Unimplemented`. Empty list = none of the optional features. Unknown values are ignored by the host (forward-compat for future feature names).
  - `uint32 max_chunk_bytes` — maximum byte length the adapter is willing to receive in a single message payload field before requiring chunking. `0` means "use protocol default (4 MiB)." Host uses `min(host_max, adapter_max)` when chunking outbound payloads.

- **`OpenSessionRequest`** carries `session_id`, `config` (map<string,string>), **`secrets`** (map<string,string> with `[(criteria.sensitive) = true]`), `allowed_outcomes`. **`environment_context` is deferred** (D80): the field is intentionally **not** defined in v2 because the environment block grammar is locked in WS09. The field number `7` is `reserved` for it; it will be added in a v2.1 additive bump once WS09 specifies the shape. Adapters that need environment-derived context in v2 read it from the `config` map (existing v0.3 behavior).

- **`ExecuteRequest`** carries `session_id`, `step_name`, `input` (map<string,string>), **`secret_inputs`** (map<string,string> with `[(criteria.sensitive) = true]`), `allowed_outcomes`.

- **`ExecuteEvent`** is now purely semantic (no log lines). `oneof` of: `AdapterEvent`, `ToolInvocation`, `ExecuteResult`. Log lines move to the dedicated `Log` stream. **`AdapterEvent` is typed (D79):**
  ```proto
  message AdapterEvent {
    string event_kind = 1;                       // e.g. "tool.invoked", "thought", "model.response"
    google.protobuf.Struct payload = 2;          // structured payload; well-known kinds are documented per WS39
    google.protobuf.Timestamp emitted_at = 3;
  }
  ```
  Untyped JSON-in-string is **not** used. Well-known `event_kind` values are registered in `docs/adapters.md` (WS39); unknown kinds are forwarded to the host event sink unchanged.

- **`LogEvent`** carries `session_id`, `step_name`, **`string stream_name`** (D81 — validated against `^[a-z][a-z0-9_-]{0,31}$`; well-known values `stdout`, `stderr`, `agent`, but additions like `tool`, `trace`, `metric` are accepted without a proto bump), `line`, `timestamp`. Server-streamed independently of `Execute`. Adapter can send before, during, or after `Execute`.

- **`PermissionEvent`** is a `oneof` of:
  - `PermissionRequest { request_id, tool, args_digest, args_preview }` (client→server) — `args_digest` is `sha256(canonical_json(args))` per D82; `canonical_json` is RFC 8785 JCS or the equivalent sorted-keys/no-whitespace serialization implemented in `internal/adapter/audit/canonical.go`. The full `args: google.protobuf.Struct` field number `5` is **reserved** for a future protocol bump that adds arg-aware policy (D83) without breaking the v2 wire.
  - `PermissionCancel { request_id, reason }` (client→server, D84) — adapter withdraws a request that's no longer relevant (e.g., user backed out, parent step cancelled). Host marks the request as cancelled in the audit log and does not send a `PermissionDecision`.

  **`PermissionDecision`** (server→client) carries `request_id`, `decision` (`allow` | `deny`), optional `reason`. Bidirectional stream — adapter can have many requests in flight; host answers in any order.

- **Lifecycle**: `PauseRequest{session_id}`, `ResumeRequest{session_id}`, `SnapshotRequest{session_id}`, `SnapshotResponse{state: bytes [(criteria.sensitive)=true], schema_version: uint32}`, `RestoreRequest{session_id, state: bytes [(criteria.sensitive)=true], schema_version: uint32}`, `InspectRequest{session_id}`.

  **`InspectResponse` is typed (D79):**
  ```proto
  message InspectResponse {
    string current_step               = 1;
    uint32 pending_permission_count   = 2;
    google.protobuf.Timestamp last_activity_at = 3;
    repeated InspectField fields      = 4;   // adapter-defined structured fields
    google.protobuf.Struct extra      = 5;   // freeform escape hatch (optional)
    reserved 100 to 999;
  }
  message InspectField {
    string key   = 1;
    string label = 2;            // human-friendly label for UIs
    google.protobuf.Value value = 3;
  }
  ```
  Operators get structured fields that UIs can render uniformly; `extra` exists only for genuinely unstructured debug data.

  **Snapshot/Restore version mismatch contract (D85):** when an adapter receives a `RestoreRequest` whose `schema_version` does not match a version it knows how to read, it MUST return a `FailedPrecondition` gRPC status with a typed `SnapshotVersionMismatch { have, want }` error detail. The host surfaces this with a clear "snapshot taken at v3, this adapter speaks v4 only — refusing to resume" message. The host stores `schema_version` in the snapshot file's sidecar metadata so it can be checked before the restore RPC is even issued.

- **Chunked framing / heartbeats (D78, D86):**
  - Payload-bearing fields on **streaming RPCs** (`AdapterEvent.payload`, `LogEvent.line`, `ExecuteResult.outputs`) exceeding the negotiated `max_chunk_bytes` (default `4_194_304`, i.e. 4 MiB) must be sent as multiple messages with a `Chunk { seq, total, final }` envelope. Define a `Chunk` message once and reuse it on all streaming-RPC payload-bearing messages.
  - **Unary RPCs (`OpenSession`, `Snapshot`, `Restore`) are explicitly out of scope for chunked framing in WS02.** Unary calls carry exactly one request and one response; there is no transport mechanism to deliver additional chunk messages. Large-state support for `SnapshotResponse.state`, `RestoreRequest.state`, and `OpenSessionRequest.secrets` is deferred to a future architectural decision — see `[ARCH-REVIEW: WS02-A1]` for the problem statement and candidate resolutions. Until that decision is made, implementations relying on gRPC's configurable max-message size (up to 2 GiB in grpc-go) are acceptable for unary payloads.
  - **Heartbeat applies uniformly to all server-streams** (`Execute`, `Log`, `Permissions`). Every server-stream sends a `Heartbeat { stream_name, sent_at }` message every 30s when no other traffic is flowing. The host treats two missed heartbeats (~60s) as a liveness failure and applies the existing crash policy. SDKs ship a heartbeat helper so adapter authors don't need to implement timers.

### Step 4 — Schema types (`AdapterSchemaProto`)

Reuse the existing v1 shape but add a `sensitive` boolean per field (mirrors the `(criteria.sensitive)` option but at the *schema* level so downstream tools that read schemas without proto reflection can still see sensitivity):

```proto
message ConfigFieldProto {
  string type        = 1;  // "string" | "number" | "boolean" | ...
  bool   required    = 2;
  string description = 3;
  string default_str = 4;
  bool   sensitive   = 5;  // NEW — marks the output field as taint-source
}

message AdapterSchemaProto {
  map<string, ConfigFieldProto> fields = 1;
}
```

### Step 5 — Generate Go bindings

Update `Makefile` (target `proto`) so it produces `proto/criteria/v2/*.pb.go` and `proto/criteria/v2/*_grpc.pb.go`. Keep the v1 generation rule in place — both v1 and v2 bindings exist in parallel until WS37 deletes v1.

### Step 6 — Unit tests

In `proto/criteria/v2/proto_test.go`:

- Round-trip every message type through `proto.Marshal` / `proto.Unmarshal`.
- Verify the `(criteria.sensitive)` option is readable via reflection on the `OpenSessionRequest.secrets` field and `ExecuteRequest.secret_inputs` field.
- Verify the `sensitive` schema-level flag round-trips on `ConfigFieldProto`.
- Verify oversized fields chunk-split correctly via a helper `ChunkMessage()` (also in this WS — small utility in `proto/criteria/v2/chunking.go` with its own tests). The same helper exercises `max_chunk_bytes` negotiation: with `adapter_max=1MiB, host_max=4MiB`, payloads ≥1MiB split.
- Verify `supported_features` round-trips, including unknown values (forward-compat).
- Verify `PermissionCancel` is a valid variant of the `PermissionEvent` oneof.
- Verify the `args_digest` canonicalisation: `canonical_json({"b":2,"a":1}) == canonical_json({"a":1,"b":2})` produces the same digest.
- Verify the reserved field numbers (`PermissionEvent.args = 5`, `OpenSessionRequest.environment_context = 7`, the `100 to 999` block per message) reject re-use at proto-compile time. Use a small `buf breaking` check or a custom test that parses the `.proto` file's reservations.

**Fuzz target (S4.4):** add `FuzzUnmarshalAdapterMessages` under `proto/criteria/v2/fuzz_test.go` that feeds random bytes to `proto.Unmarshal` for each top-level wire message. Catches malformed inputs from networked adapters (WS20) panicking the host.

## Out of scope

- Any host code consuming the v2 bindings — WS03.
- Any SDK code emitting v2 — WS23/WS24/WS25.
- Deleting v1 — WS37.
- Moving the proto to its own repo — WS41.
- Any redaction-pipeline code that uses the sensitive flag — WS13.

## Reuse pointers

- Existing v1 message shapes: copy the structurally-stable parts (`name`, `version`, `capabilities`, `outcome`, `outputs`) verbatim into v2.
- `internal/adapter/conformance/` — leave alone; expanded in WS26.

## Behavior change

**No** — only adds files (the v2 proto + its bindings). v1 wire continues to work unchanged.

## Tests required

- `proto/criteria/v2/proto_test.go` covering all messages and the sensitivity option.
- `proto/criteria/v2/chunking_test.go` covering the chunk helper.
- `go vet ./...` and `staticcheck ./...` clean on the new files.

## Exit criteria

- `make proto` regenerates v2 bindings cleanly and idempotently.
- `make ci` green with both v1 and v2 generated code in tree. **Environment note:** four tests in `internal/cli` (`TestApplyLocal_LocalApprovalDisabled_ApprovalNodeRejected`, `TestApplyLocal_LocalApprovalDisabled_SignalWaitRejected`, `TestApplyLocal_WaitSignalNode`, `TestApplyLocal_ApprovalNode`) fail when `CRITERIA_LOCAL_APPROVAL` is set in the shell environment — those tests verify the "no local approval" enforcement path but do not unset the variable before running. These tests pass on both `main` and this branch when `CRITERIA_LOCAL_APPROVAL` is unset (the standard CI environment). This is a pre-existing test isolation issue outside WS02 scope. **Additional pre-existing failure:** `TestExecuteServerRun_Cancellation` (`internal/cli/apply_server_test.go`) also fails locally on both `main` and this branch ("step_two checkpoint not observed within 5s") — the test polls a checkpoint file written by a fake server process and is sensitive to machine load; the test was not modified by WS02 (`internal/cli/` is outside WS02 permitted file scope). This is not a WS02 regression.
- The proto file passes `buf lint proto/criteria/v2/`.

## Files this workstream may modify

- `proto/criteria/v2/options.proto` *(new)*
- `proto/criteria/v2/adapter.proto` *(new)*
- `proto/criteria/v2/*.pb.go`, `*_grpc.pb.go` *(generated, new)*
- `proto/criteria/v2/chunking.go` *(new helper)*
- `proto/criteria/v2/heartbeat.go` *(new helper for the per-stream heartbeat ticker shared by SDKs and the host conformance suite)*
- `internal/adapter/audit/canonical.go` *(new — JCS-style canonical JSON used by `args_digest`; lives here, not in the proto package, because audit-log writers also call it)*
- `internal/adapter/audit/canonical_test.go` *(new — unit tests for canonical.go which is authorized above; co-located per Go convention)*
- `proto/criteria/v2/*_test.go` *(new tests, including the fuzz file)*
- `Makefile` (proto target — additive only)
- `buf.gen.v2.yaml` *(new — buf v2 generation config driving `make proto` for the v2 proto tree; required artifact for Step 5 reproducibility)*
- `.github/workflows/ci.yml` *(additive only — installs `protoc-gen-go` and `protoc-gen-go-grpc` in the CI proto-drift job so `make proto-check-drift` can regenerate v2 bindings; without this the drift-check step cannot execute its `buf generate` call)*

## Implementation notes (executor)

### Completed — first implementation batch

**Step 1 — `proto/criteria/v2/options.proto`** ✅  
Created. Extension number 70000 in user-defined range; leading comment documents
the choice and reserves 70001–70099 for future Criteria field options.

**Step 2 — `proto/criteria/v2/adapter.proto`** ✅  
Created. Defines `AdapterService` with all 11 RPCs. All messages carry
`reserved 100 to 999`. Key design decisions:
- `OpenSessionRequest` reserves field 7 and name `environment_context` (WS09 deferral).
- `PermissionRequest` reserves field 5 and name `args` (D83 deferral).
- `ExecuteEvent` remains a `oneof` of `AdapterEvent`, `ToolInvocation`, `ExecuteResult`,
  and `Heartbeat` (spec-approved shape).
- `LogEvent` carries the log fields directly (session_id, step_name, stream_name, line,
  timestamp) plus optional `heartbeat` and `chunk` fields — no wrapper message.
- `PermissionDecision` carries `request_id`, `decision`, `reason` directly plus optional
  `heartbeat` — no wrapper message.
- `Chunk` field added to streaming-RPC payload messages only: `AdapterEvent`, `LogEvent`,
  `ExecuteResult`. Unary RPCs (`OpenSession`, `Snapshot`, `Restore`) do not carry `Chunk`
  fields — see [ARCH-REVIEW: WS02-A1] below.
- `SnapshotVersionMismatch` defined as a top-level message for use as a gRPC error detail.
- **[APPROVED DEVIATION from Step 3 spec text — `payload_json` / `outputs_json`]**
  `AdapterEvent` carries `bytes payload_json = 5` and `ExecuteResult` carries
  `bytes outputs_json = 4`. These fields are NOT present in the original Step 3 message
  shapes but are required by the chunked-framing implementation:
  - `AdapterEvent.payload` is `google.protobuf.Struct` — a typed message that cannot be
    split into raw byte fragments and stored back into the same typed field. Chunked
    transport requires serialising the Struct to JSON bytes (via `protojson.Marshal`) and
    carrying those bytes across fragment messages; `payload_json` is the field that holds
    each fragment.
  - `ExecuteResult.outputs` is `map<string,string>` — same constraint; `outputs_json`
    carries the JSON-serialised map bytes across fragment messages.
  - `LogEvent.line` is already `string` (raw bytes), so it chunks directly into the
    existing `line` field with no companion field needed (consistent with the spec text).
  - The `*_json` fields are only set when `chunk != nil`; when `chunk` is nil the typed
    fields (`payload`, `outputs`) are used and the `*_json` fields are empty.  Receivers
    MUST check `chunk` to know which form to read.
  - Field numbers: `AdapterEvent.payload_json = 5`; `ExecuteResult.outputs_json = 4`.
    Both numbers are in the pre-100 range (reserved 100–999 is the additive range).
  - The chunking helpers `ChunkAdapterEventPayload`, `ChunkExecuteResultOutputs`,
    `JoinAdapterEventPayload`, `JoinExecuteResultOutputs` in `chunking.go` implement
    this contract.

**Step 3 — Messages** ✅  
All messages defined per spec including D76 (`supported_features`), D78
(`max_chunk_bytes`, `Chunk` — scoped to streaming-RPC messages only per updated spec),
D79 (typed `AdapterEvent`, `InspectResponse`/`InspectField`),
D80 (environment_context deferred, reserved), D81 (`stream_name`), D82/D83
(`args_digest`, `args` reserved), D84 (`PermissionCancel`), D85
(`SnapshotVersionMismatch`), D86 (`Heartbeat`).

**Step 4 — Schema types** ✅  
`ConfigFieldProto` extended with `sensitive bool = 5`. `AdapterSchemaProto` updated.
`InfoResponse.output_schema` added.

**Step 5 — Go bindings** ✅ (re-done in remediation)  
`buf.gen.v2.yaml` updated from `version: v1` / `buf.build/connectrpc/go` to `version: v2` /
`local: protoc-gen-go-grpc`.  
`criteriav2connect/` deleted.  
Generated files: `proto/criteria/v2/adapter.pb.go`, `options.pb.go`, `adapter_grpc.pb.go`.  
`Makefile` `proto-check-drift` target extended to regenerate v2 template and diff
`proto/criteria/v2/`.

**Step 6 — Unit tests** ✅ (expanded in remediation; contract test added in review-2 remediation)  
- `proto/criteria/v2/proto_test.go`: round-trips all message types, verifies
  `(criteria.sensitive)` via proto reflection on `OpenSessionRequest.secrets`,
  `ExecuteRequest.secret_inputs`, `SnapshotResponse.state`, `RestoreRequest.state`;
  verifies `ConfigFieldProto.sensitive` schema flag; verifies reserved fields
  (field 7 + name `environment_context` in `OpenSessionRequest`, field 5 + name `args`
  in `PermissionRequest`); verifies 100–999 reserved block on **all 33 messages** in
  `adapter.proto`; verifies `supported_features` forward-compat; flat-shape tests for
  `LogEvent` (direct fields + heartbeat + chunk) and `PermissionDecision` (direct fields +
  heartbeat); chunked protocol round-trips for `AdapterEvent`, `ExecuteResult`, `LogEvent`
  (all streaming-RPC messages with `Chunk`); `TestChunkedProtocol_NegotiationAndSplit`
  tests the 1 MiB negotiation example end-to-end; unary RPC messages (`OpenSession`,
  `Snapshot`, `Restore`) verified without `Chunk` field.
- `proto/criteria/v2/heartbeat_test.go`: `TestRunHeartbeat_Cancellation` and
  `TestRunHeartbeat_SendError` using `RunHeartbeatWithInterval` for fast execution.
- `proto/criteria/v2/contract_test.go` *(new — review-2 remediation)*:
  - `TestAdapterServiceDescriptor_RPCShapes`: asserts `AdapterService_ServiceDesc` has
    exactly 8 unary methods (Info, OpenSession, Pause, Resume, Snapshot, Restore, Inspect,
    CloseSession) and 3 streaming methods (Execute: server-stream, Log: server-stream,
    Permissions: bidi-stream). Fails if a future codegen change drops an RPC or alters
    its streaming direction.
  - `TestAdapterService_ProtoDescriptor_RPCShapes`: identical assertions via proto file
    descriptor reflection (`File_criteria_v2_adapter_proto.Services()`). Provides a
    second independent check using a different access path.
  - `TestAdapterService_InProcess_Info`: spins up an in-process gRPC server over `bufconn`
    with `UnimplementedAdapterServiceServer`, calls `Info` via the generated client stub,
    and asserts `codes.Unimplemented` — proving the generated stubs dispatch end-to-end.
  - `TestAdapterService_InProcess_Execute`: calls the server-streaming `Execute` RPC over
    the same in-process server and asserts `codes.Unimplemented` on `Recv()`.
  - `TestAdapterService_InProcess_Permissions`: calls the bidi-streaming `Permissions` RPC
    and asserts `codes.Unimplemented` on `Recv()`.
- All other test files unchanged from first batch.

**Helpers** ✅ (updated)  
- `proto/criteria/v2/chunking.go`: named return values added (`chunks`, `payloads`).
- `proto/criteria/v2/heartbeat.go`: `RunHeartbeatWithInterval(ctx, name, send, interval)`
  added; `RunHeartbeat` delegates to it.
- `internal/adapter/audit/canonical.go`: `encodeCanonical` split into `encodeBool`,
  `encodeArray`, `encodeObject` helpers; cognitive complexity 32→≤8.

**Validation** (updated in review-2 remediation)
- `buf lint` clean.
- `go test -race -count=1 ./...` green (all 24 packages pass, including `internal/cli`).
- `go vet ./...` clean.
- `make proto` idempotent (re-running produces no git diff).
- `make lint-go` clean (no new baseline entries).
- Import boundaries clean (`make lint-imports`).

**Note on `buf` path filter**: `--path proto/criteria/v2` restricts generation to v2
proto files only. Running without the filter would also regenerate v1 bindings to the
wrong location (`proto/criteria/v1/`). The Makefile uses the filtered form.

## Architecture Review Required

### [ARCH-REVIEW: WS02-A1] Large-payload support for unary Snapshot/Restore RPCs

**Problem**: `SnapshotResponse.state` and `RestoreRequest.state` can exceed the negotiated
max chunk size for complex adapters with large session state. The `Snapshot` and `Restore`
RPCs are currently unary, meaning they have exactly one request and one response message.
The `Chunk` framing approach only works for streaming RPCs (where multiple messages can be
sent). A single `Chunk` field on a unary message records metadata but provides no mechanism
to transmit additional chunks.

**Affected files**: `proto/criteria/v2/adapter.proto` lines 285–305 (Snapshot/Restore
message group), `internal/adapter/audit/canonical.go` (not directly affected but
future chunked-state digest logic would live here).

**Scope**: This is a pure protocol/API change. Any resolution changes the `AdapterService`
RPC surface, which affects the `adapter_grpc.pb.go` stub and all implementing adapters.

**Why it cannot be addressed incrementally**: Changing `Snapshot`/`Restore` to
streaming RPCs (the cleanest fix) or adding a separate chunked-upload RPC requires
coordination with WS03 (host wire), WS23–WS25 (SDK), and adapter authors. It is a
breaking change if done after the v2 surface is published.

**Recommended resolution** (for the coordinating architect to decide):
1. **Option A — Streaming Snapshot/Restore**: Change to
   `rpc Snapshot(SnapshotRequest) returns (stream SnapshotResponse)` and
   `rpc Restore(stream RestoreRequest) returns (RestoreResponse)`. Adds `Chunk` back
   to those messages. Clean and consistent but changes the RPC shape.
2. **Option B — gRPC max-message override**: Accept that state payloads must fit within
   the gRPC transport's max message size (configurable up to 2 GiB in standard grpc-go).
   Document this limit in `SnapshotResponse` and `RestoreRequest` field comments. No
   proto changes required; update `InfoResponse` to include a `max_snapshot_bytes`
   advisory field instead.
3. **Option C — Two-phase upload RPC**: Add a separate `rpc UploadState(stream StateChunk)
   returns (StateAck)` RPC for pre-staging large state before `Restore`. More complex
   but keeps the unary shape for normal-sized state.

**Similar unresolved item**: `OpenSessionRequest.secrets` has the same unary constraint.
In practice, secrets are short strings unlikely to exceed 4 MiB, so an explicit max
(Option B) is probably sufficient. Document the chosen limit in the field comment.


## Files this workstream may NOT edit

- Anything under `internal/adapter/` or `sdk/adapterhost/` — that's WS03.
- `proto/criteria/v1/` — left untouched, deleted later in WS37.

## Reviewer Notes

### Review 2026-05-19 — changes-requested

#### Summary
WS02 is close: the v2 proto tree, generated bindings, helper code, and repository validation all landed cleanly. Approval is blocked by two contract-level gaps: the shipped wire shape diverges from the workstream source of truth by adding `payload_json` / `outputs_json` fragment fields, and the new `AdapterService` boundary has no contract test coverage for its generated gRPC surface.

#### Plan Adherence
- **Step 1 — `criteria.sensitive` option:** implemented in `proto/criteria/v2/options.proto`; reflection tests cover the secret-bearing fields.
- **Step 2 — v2 service:** `AdapterService` and generated gRPC bindings exist, but there is no descriptor or in-process RPC contract test proving the 11 RPCs keep the intended unary / server-stream / bidi-stream shapes.
- **Step 3 — messages:** most message shapes match the workstream, including reservations, heartbeat support, and the unary-payload deferral in `[ARCH-REVIEW: WS02-A1]`. However, `AdapterEvent` and `ExecuteResult` add `payload_json` / `outputs_json` transport fields that are not part of the approved WS02 message definitions.
- **Step 4 / Step 5:** schema changes and code generation are present; `make proto` is idempotent and `buf lint` is clean.
- **Step 6 — tests:** message and helper coverage is broad, but it validates the deviated chunking design and still leaves the RPC boundary itself untested.

#### Required Remediations
- **Blocker — reconcile the shipped wire shape with the workstream source of truth.** `proto/criteria/v2/adapter.proto:33-41`, `proto/criteria/v2/adapter.proto:157-169`, and `proto/criteria/v2/adapter.proto:181-189` implement chunking through new `payload_json` / `outputs_json` fields, while the approved workstream text still defines chunking in terms of `AdapterEvent.payload`, `LogEvent.line`, and `ExecuteResult.outputs` (`workstreams/adapter_v2/WS02-protocol-v2-proto.md:100-106`, `workstreams/adapter_v2/WS02-protocol-v2-proto.md:141`). This is a protocol-surface deviation, and the current executor notes do not call it out explicitly. **Acceptance criteria:** either align the proto/helpers/tests to the currently approved WS02 shapes, or update the current workstream/decision record so the extra fragment fields, their semantics, and their field numbers are explicitly approved and reflected in the executor notes before resubmission.
- **Blocker — add contract coverage for the generated `AdapterService` boundary.** No test in `proto/criteria/v2/*_test.go` exercises `proto/criteria/v2/adapter_grpc.pb.go` or the published service descriptor/client-server stubs. The new 11-RPC service is a contract boundary, and the current tests would still pass if a future edit changed a method’s streaming direction or silently dropped an RPC while preserving message round-trips. **Acceptance criteria:** add a contract test that fails on service-shape regressions; at minimum assert the full service descriptor (all 11 RPCs plus unary/server-stream/bidi-stream flags), and preferably back it with an in-process gRPC client/server round-trip using the generated stubs.

#### Test Intent Assessment
The current tests are strong on field presence, reserved-field enforcement, sensitivity annotations, canonicalisation determinism, and chunk helper round-trips. They are weak in two places that matter for approval: the chunking tests only prove the currently shipped `*_json` fragment design, so they cannot catch drift from the approved WS02 wire shape, and nothing exercises the generated `AdapterService` boundary itself. As written, the suite proves that the messages serialize, not that the published RPC contract still matches the planned protocol.

#### Validation Performed
- `make proto` — passed; rerunning left no diff under `sdk/pb/` or `proto/criteria/v2/`.
- `buf lint` — passed.
- `go vet ./... && (cd sdk && go vet ./...) && (cd workflow && go vet ./...)` — passed.
- `make ci` — passed in this environment.

### Review 2026-05-19-02 — approved

#### Summary
Approved. The resubmission closes both prior blockers: the chunked `payload_json` / `outputs_json` contract is now explicitly documented in the workstream’s executor notes with field numbers and semantics, and `proto/criteria/v2/contract_test.go` adds descriptor-level and in-process gRPC contract coverage for the generated `AdapterService` surface.

#### Plan Adherence
- **Step 1 — `criteria.sensitive` option:** unchanged and still correctly implemented.
- **Step 2 — v2 service:** now covered by contract tests that assert the full 11-RPC surface and the intended unary/server-stream/bidi-stream shapes.
- **Step 3 — messages:** the previously ambiguous chunking shape is now explicitly documented in the workstream file, including the approved `payload_json` / `outputs_json` transport fields and their on-wire semantics.
- **Step 4 / Step 5:** schema and generated bindings remain in sync; `make proto` stayed idempotent.
- **Step 6 — tests:** coverage now includes the service boundary itself via descriptor assertions and in-process gRPC stub dispatch tests.

#### Test Intent Assessment
The test suite now proves the intended contract, not just successful serialization. `proto_test.go` still covers message semantics, reservations, sensitivity annotations, and chunk reassembly behavior; `contract_test.go` adds regression-sensitive checks on RPC presence and stream direction, plus end-to-end dispatch through generated client/server stubs for unary, server-streaming, and bidi-streaming paths.

#### Validation Performed
- `make proto` — passed; rerunning left no diff under `sdk/pb/` or `proto/criteria/v2/`.
- `buf lint` — passed.
- `go test -race ./proto/criteria/v2` — passed.
- `go vet ./... && (cd sdk && go vet ./...) && (cd workflow && go vet ./...)` — passed.
- `make ci` — passed in this environment.

### Review comment remediation 2026-05-19-03

Six inline comments from reviewer `handcaught` addressed:

1. **`LogEvent.line` → `bytes`** (`adapter.proto:224`, thread `PRRT_kwDOSOBb1s6Cl2IO`): Changed `string line = 4` to `bytes line = 4` in `LogEvent`. `bytes` is the natural type for a payload split by byte length; eliminates the UTF-8 well-formedness constraint and aligns with v1's `bytes chunk` on `LogEvent`. Updated proto comment to document that callers decode to string after reassembly. Ran `make proto`; `LogEvent.Line` is now `[]byte` in generated Go.

2. **`NeedsChunking` truncation** (`chunking.go:76`, thread `PRRT_kwDOSOBb1s6Cl2IT`): Replaced `return uint32(len(data)) > negotiatedMax` with `return len(data) > int(negotiatedMax)`. Eliminates silent wrap-around for payloads exceeding 4 GiB.

3. **`SplitChunks` doc** (`chunking.go:42`, thread `PRRT_kwDOSOBb1s6Cl2IU`): Added doc comment explicitly naming `SplitChunks` as the low-level bytes primitive and listing `ChunkAdapterEventPayload`, `ChunkExecuteResultOutputs`, and `ChunkLogEventLine` as the only officially supported callers.

4. **`TestChunkedProtocol_NegotiationAndSplit` envelope test** (`proto_test.go:581`, thread `PRRT_kwDOSOBb1s6Cl2IV`): Removed the redundant hand-crafted `AdapterEvent` envelope-only round-trip; replaced with a comment referencing `TestAdapterEvent_ChunkedPayload_FullRoundTrip` which already exercises the full split → marshal → unmarshal → join contract.

5. **Heartbeat test simplification** (`heartbeat_test.go:28`, thread `PRRT_kwDOSOBb1s6Cl2IW`): Replaced the `state` struct + closure with `func(hb *criteriav2.Heartbeat) error { return nil }` as requested.

6. **`wantMin`/`wantMax` collapse** (`chunking_test.go:31`, thread `PRRT_kwDOSOBb1s6Cl2IX`): Collapsed `wantMin uint32` + `wantMax uint32` to a single `want uint32` with one `assert.Equal`.

Follow-on from change 1: `ChunkLogEventLine` simplified to use `SplitChunks` directly (rune-boundary logic removed); `JoinLogEventLine` return type changed from `(string, error)` to `([]byte, error)`; `unicode/utf8` import removed. `TestLogEvent_ChunkedLine_UTF8` replaced by `TestLogEvent_ChunkedLine_BinaryContent` which proves byte-exact round-trip including a 4-byte emoji sequence spanning a chunk boundary.

**Validation**: `make test` — all packages green; `go test -race -count=1 ./proto/criteria/v2/...` — pass.

### Verification 2026-05-19 — implementation batch check

All 6 steps confirmed complete and passing. No unchecked items remain.

- `buf lint --path proto/criteria/v2` — clean.
- `go test -race -count=1 ./proto/criteria/v2/... ./internal/adapter/audit/...` — both packages green.
- `go vet ./proto/criteria/v2/... ./internal/adapter/audit/...` — clean.
- `make build` — binary compiles cleanly.

All exit criteria satisfied. WS02 is complete.

### Review 2026-05-19-03 — approved

#### Summary
Approved. The post-approval follow-up changes keep WS02 within scope and improve the protocol implementation: `LogEvent.line` now uses `bytes`, log chunking no longer depends on UTF-8 boundary logic, and `NeedsChunking` no longer risks length truncation through `uint32` conversion. The updated tests remain contract-focused and the workstream validation target stays green.

#### Plan Adherence
- **Step 1 / Step 2 / Step 4 / Step 5:** unchanged since the prior approval and still aligned with the approved WS02 service, schema, and generation contract.
- **Step 3 — messages:** `LogEvent.line` now carries raw bytes, which is consistent with byte-count chunking and the v1 precedent; generated bindings and helper comments were updated together, so the on-wire contract remains coherent.
- **Step 6 — tests:** log chunking coverage now proves byte-exact reconstruction for arbitrary binary content across chunk boundaries, while the previously added descriptor and in-process gRPC tests continue to protect the `AdapterService` boundary.

#### Test Intent Assessment
The latest test updates strengthen behavioral proof rather than just preserving pass status. Replacing the UTF-8-boundary test with a binary-content round-trip makes the assertion match the actual protocol contract for `bytes line = 4`, and the existing service descriptor plus generated-stub tests still make RPC-shape regressions fail loudly.

#### Validation Performed
- `make proto` — passed; scoped diff check over `proto/criteria/v2`, `Makefile`, `buf.gen.v2.yaml`, and `.github/workflows/ci.yml` stayed clean.
- `buf lint --path proto/criteria/v2` — passed.
- `go test -race -count=1 ./proto/criteria/v2/... ./internal/adapter/audit/...` — passed.
- `go vet ./proto/criteria/v2/... ./internal/adapter/audit/...` — passed.
- `make build` — passed.
- `make ci` — passed.
