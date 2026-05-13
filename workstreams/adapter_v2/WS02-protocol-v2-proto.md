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

Author the message types. Key shape decisions (see `README.md` D22–D27):

- **`InfoResponse`** carries `name`, `version`, `description`, `capabilities`, `platforms`, `sdk_protocol_version`, `source_url`, `config_schema`, `input_schema`, **`output_schema`** (new), `secrets` (declared secret names with descriptions), `permissions`, `compatible_environments`, `container_image` (optional, see D12b).

- **`OpenSessionRequest`** carries `session_id`, `config` (map<string,string>), **`secrets`** (map<string,string> with `[(criteria.sensitive) = true]`), `allowed_outcomes`, `environment_context` (a flattened, host-redacted view of the environment block).

- **`ExecuteRequest`** carries `session_id`, `step_name`, `input` (map<string,string>), **`secret_inputs`** (map<string,string> with `[(criteria.sensitive) = true]`), `allowed_outcomes`.

- **`ExecuteEvent`** is now purely semantic (no log lines). `oneof` of: `AdapterEvent` (structured JSON payload), `ToolInvocation`, `ExecuteResult`. Log lines move to the dedicated `Log` stream.

- **`LogEvent`** carries `session_id`, `step_name`, `stream` (`stdout` | `stderr` | `agent`), `line`, `timestamp`. Server-streamed independently of `Execute`. Adapter can send before, during, or after `Execute`.

- **`PermissionEvent`** (client→server) carries `request_id`, `tool`, `args_digest`, optional human-readable `args_preview`. **`PermissionDecision`** (server→client) carries `request_id`, `decision` (`allow` | `deny`), optional `reason`. Bidirectional stream — adapter can have many requests in flight; host answers in any order.

- **Lifecycle**: `PauseRequest{session_id}`, `ResumeRequest{session_id}`, `SnapshotRequest{session_id}`, `SnapshotResponse{state: bytes [(criteria.sensitive)=true], schema_version: uint32}`, `RestoreRequest{session_id, state: bytes [(criteria.sensitive)=true], schema_version: uint32}`, `InspectRequest{session_id}`, `InspectResponse{state_json: string, current_step, pending_permission_count, last_activity_at}`.

- **Chunked framing / heartbeats**: any payload field exceeding `4_194_304` bytes (4 MiB) must be sent as multiple messages with a `chunk { seq, total, final }` envelope. Define a `Chunk` message and use it in `AdapterEvent`, `SnapshotResponse.state`, `RestoreRequest.state`. The `Heartbeat` message is sent on the `Log` stream every 30s when no other traffic is flowing.

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
- Verify oversized fields chunk-split correctly via a helper `ChunkMessage()` (also in this WS — small utility in `proto/criteria/v2/chunking.go` with its own tests).

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
- `proto/criteria/v2/*_test.go` *(new tests)*
- `Makefile` (proto target — additive only)

## Files this workstream may NOT edit

- Anything under `internal/adapter/` or `sdk/adapterhost/` — that's WS03.
- `proto/criteria/v1/` — left untouched, deleted later in WS37.
- `README.md`, `PLAN.md`, `AGENTS.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `workstreams/README.md`, other workstream files.
