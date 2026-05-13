# WS15 — Dedicated `Log` channel separate from `Execute` events

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS03](WS03-host-v2-wire.md), [WS13](WS13-secrets-channel-redaction.md). · **Unblocks:** cleaner adapter UX; redaction-correctness on log surfaces.

## Context

`README.md` D23. v1 interleaved log lines with semantic Execute events in the same stream. v2 has a dedicated `Log` server-stream RPC. The host consumes both streams concurrently and merges by timestamp for display, while preserving the semantic stream's event-ordering invariants.

## Prerequisites

WS02 (Log RPC defined), WS03 (host wire on v2), WS13 (redaction registry exists for log lines).

## In scope

### Step 1 — Host-side Log consumer

In `internal/adapter/sessions.go`: at session open, spawn a goroutine that:

1. Calls `client.Log(ctx, &v2.LogRequest{SessionID: ...}, sink)`.
2. Pipes received `LogEvent` messages to the host log pipeline, after redaction.
3. Continues until session close.

The Log stream is independent of any Execute call — adapters can log even when no Execute is in flight (useful for connection-lifecycle messages).

### Step 2 — Merged display

The terminal renderer that today shows a single stream of events now displays Log events interleaved with Execute events, sorted by adapter-supplied timestamp. Out-of-order arrival within a small window (≤500ms) is tolerated by buffering; older events are flushed.

### Step 3 — Heartbeat handling

Per D27, the Log stream carries periodic `Heartbeat` messages (every 30s when otherwise idle). The host's session crash detector watches for heartbeat-stall (no heartbeat for >90s) and treats it as a crash, falling through to the existing crash-policy machinery.

### Step 4 — Tests

- Unit: log-event flow + redaction.
- Integration: a v2 test adapter emits 100 log lines + 10 execute events; assert ordering at display + all redaction applied.
- Heartbeat-stall test: simulated adapter stops responding; assert crash detected within timeout.

## Out of scope

- The `Log` RPC proto definition — WS02.
- Redaction registry — WS13.
- Crash policy itself — already exists from v1 and is reused.

## Behavior change

**Yes** — log surface separates from event surface; adapter SDKs (WS23–WS25) expose `log.stdout(...)` and `log.stderr(...)` helpers that emit on the Log stream instead of via `Execute`.

## Tests required

- Unit + integration tests as above.

## Exit criteria

- Logs flow on a dedicated stream end-to-end.
- Heartbeat-stall crash detection works.

## Files this workstream may modify

- `internal/adapter/sessions.go` — spawn log consumer goroutine.
- `internal/log/` or terminal renderer — interleaved display.
- Test fixtures.

## Files this workstream may NOT edit

- WS13's redaction registry source (consumer only).
- WS02's proto definitions.
