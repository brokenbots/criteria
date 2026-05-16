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
  - Any payload-bearing field (`AdapterEvent.payload`, `LogEvent.line`, `ExecuteResult.outputs`, `SnapshotResponse.state`, `RestoreRequest.state`, `OpenSessionRequest.secrets`) exceeding the negotiated `max_chunk_bytes` (default `4_194_304`, i.e. 4 MiB) must be sent as multiple messages with a `Chunk { seq, total, final }` envelope. Define a `Chunk` message once and apply the rule consistently — do not enumerate which messages get chunking; the rule is "any user-controllable bytes/string/map field."
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
- `make ci` green with both v1 and v2 generated code in tree.
- The proto file passes `buf lint proto/criteria/v2/`.

## Files this workstream may modify

- `proto/criteria/v2/options.proto` *(new)*
- `proto/criteria/v2/adapter.proto` *(new)*
- `proto/criteria/v2/*.pb.go`, `*_grpc.pb.go` *(generated, new)*
- `proto/criteria/v2/chunking.go` *(new helper)*
- `proto/criteria/v2/heartbeat.go` *(new helper for the per-stream heartbeat ticker shared by SDKs and the host conformance suite)*
- `internal/adapter/audit/canonical.go` *(new — JCS-style canonical JSON used by `args_digest`; lives here, not in the proto package, because audit-log writers also call it)*
- `proto/criteria/v2/*_test.go` *(new tests, including the fuzz file)*
- `Makefile` (proto target — additive only)

## Files this workstream may NOT edit

- Anything under `internal/adapter/` or `sdk/adapterhost/` — that's WS03.
- `proto/criteria/v1/` — left untouched, deleted later in WS37.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, other workstream files.
