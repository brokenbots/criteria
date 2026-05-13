# WS19 — Chunked framing + heartbeats in the v2 wire

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS03](WS03-host-v2-wire.md), [WS15](WS15-dedicated-log-channel.md). · **Unblocks:** [WS20](WS20-remote-environment-and-shim.md) (remote benefits most from this; local works too).

## Context

`README.md` D27. The protocol carries some potentially large payloads: snapshot blobs, accumulated adapter events, log batches. To survive remote transports (WS20) and intermediate proxies that may impose message-size caps, the wire chunks any message above 4 MiB and uses explicit heartbeats so disconnects are detectable independent of the underlying TCP/HTTP/2 keep-alive.

## Prerequisites

WS02 (proto with `Chunk` message), WS03 (host wire), WS15 (Log stream where heartbeats live).

## In scope

### Step 1 — Chunk helpers

`proto/criteria/v2/chunking.go` is already created by WS02 with `Chunk` types and a basic helper. This WS exercises and hardens it:

```go
// SendChunks splits a large message body into Chunk envelopes and emits
// them on the provided sink. Chunk size defaults to 1 MiB.
func SendChunks(body []byte, sink ChunkSink) error

// AssembleChunks accumulates chunks until the final flag is seen and
// returns the reassembled body. Errors if a chunk arrives out of order
// or with a duplicate seq.
func AssembleChunks(stream ChunkSource) ([]byte, error)
```

### Step 2 — Wire integration

In `internal/adapter/sessions.go`:

- Outgoing `SnapshotResponse.state` and `RestoreRequest.state` use chunked framing transparently when > 4 MiB.
- Incoming chunks reassemble before being delivered to the consumer.
- Adapter events that exceed 4 MiB chunk-split.

### Step 3 — Heartbeats on Log stream

`internal/adapter/sessions.go`: the Log consumer goroutine (WS15) emits a host-side timer; if no traffic in 30s, it expects the adapter to send a `Heartbeat`. If none arrives in 90s, the session is considered crashed (existing crash-policy machinery handles it).

Adapter SDK side (WS23–WS25): the SDK helper emits Heartbeats automatically — adapter code does not need to manage this.

### Step 4 — Reconnect-safe chunk identifiers

Each chunk envelope carries `chunk { seq, total, final, payload_id }`. Across a reconnect (relevant for WS20 remote scenarios), the receiver can resume by acknowledging the last-received seq for each `payload_id`. v1 reconnect isn't supported in this WS — only the wire-level fields are reserved.

### Step 5 — Tests

- Chunking round-trip for sizes 0, 1B, 1MiB, 4MiB, 16MiB, 100MiB.
- Out-of-order / duplicate / missing-final detection.
- Heartbeat-stall integration test.
- Local UDS works unchanged for sub-4-MiB payloads (no regression).

## Out of scope

- Actual remote transport — WS20.
- Reconnect resume semantics — deferred to a future workstream when there's user demand.

## Behavior change

**Mostly no, with edge cases.** Existing payloads stay sub-threshold and are unchunked (single-message wire). Heartbeats are new but invisible to adapter authors (SDK handles them) and to end users.

## Tests required

- Unit + integration tests as above.

## Exit criteria

- Round-trip across all chunking sizes succeeds.
- Heartbeat-stall detected within the 90s window.

## Files this workstream may modify

- `proto/criteria/v2/chunking.go` and tests (extending WS02's stub).
- `internal/adapter/sessions.go`.
- `internal/adapter/heartbeat.go` *(new)*.

## Files this workstream may NOT edit

- `proto/criteria/v2/*.proto` — WS02.
- WS15 Log consumer wire-up (consumer only).
- Other workstream files.
