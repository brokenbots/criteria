# WS18 — `Snapshot` / `Restore` lifecycle RPCs

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS13](WS13-secrets-channel-redaction.md), [WS16](WS16-bidi-permission-stream.md), [WS17](WS17-pause-resume-inspect.md). · **Unblocks:** long-running workflow durability story.

## Context

`README.md` D25, D67. `Snapshot()` returns an opaque adapter-defined blob plus the host's per-session state (permission queue + decision log + secret origin refs). `Restore()` accepts the blob and re-establishes the session deterministically.

## Prerequisites

WS02, WS13, WS16, WS17 merged.

## In scope

### Step 1 — Host orchestration

In `internal/adapter/sessions.go`:

```go
type SessionSnapshot struct {
    AdapterState     []byte           // opaque to host (from adapter via Snapshot RPC)
    SchemaVersion    uint32
    PermissionState  []byte           // from PermissionState.MarshalState() (WS16)
    SecretOriginRefs map[string]OriginRef  // from sessions config; values not included
    AdapterDigest    digest.Digest    // adapter manifest digest at snapshot time
    HostArch         string           // GOOS/GOARCH at snapshot
    CreatedAt        time.Time
}

func (s *Session) Snapshot(ctx context.Context) (*SessionSnapshot, error)
func (sm *SessionManager) Restore(ctx context.Context, ref AdapterRef, env *EnvironmentNode, snap *SessionSnapshot) (*Session, error)
```

### Step 2 — Persistence layout

```
~/.criteria/runs/<run-id>/snapshots/<session-id>/<seq>.bin
~/.criteria/runs/<run-id>/snapshots/<session-id>/<seq>.json   # SessionSnapshot metadata
```

Sequence numbers monotonically increase; the latest is the resume target.

### Step 3 — Cross-host compatibility rules

Restore is **refused** if:

- `AdapterDigest` does not match the lockfile's current digest for the same adapter ref (the adapter was upgraded). Error: *"snapshot was taken against adapter `<ref>@digest1`; current lockfile pins `<ref>@digest2`. Resume requires the same adapter version."*
- `HostArch` does not match the resume host's arch (snapshots are not portable across architectures in v1; documented limitation).
- `SchemaVersion` is unknown.

### Step 4 — Secret re-resolution on restore

The `SecretOriginRefs` map is replayed through the WS13 provider stack. Resolution failures (e.g., env var missing on resume host) are fatal with a clear "missing secret <name>" message.

### Step 5 — Permission state restore

`PermissionState.RestoreState(...)` (WS16) is called with the blob; previously-answered requests replay deterministically.

### Step 6 — Engine-level resume

`internal/engine/`: after `criteria pause`, calling `criteria resume` finds the latest snapshot and reconstructs all sessions before resuming step execution.

### Step 7 — Tests

- Round-trip snapshot: pause an adapter mid-run, snapshot, kill host, start new host, restore, verify continuation matches what would have happened without the pause.
- Refusal tests for each rule (digest mismatch, arch mismatch, schema mismatch).
- Missing-secret-on-resume test.

## Out of scope

- The `Pause`/`Resume`/`Inspect` RPC wiring — WS17 (snapshot uses them implicitly).
- Permission-state marshaling — WS16.
- Secret re-resolution provider — WS13.

## Behavior change

**Yes** — long workflows can be paused, host-restarted, and resumed. The snapshot file is in the run directory and is human-inspectable as JSON metadata + opaque blob.

## Tests required

- Round-trip + refusal tests.

## Exit criteria

- Snapshot/Restore round-trip works on a test adapter that has non-trivial state.

## Files this workstream may modify

- `internal/adapter/sessions.go`.
- `internal/engine/` resume integration.
- New persistence helpers in `internal/runtime/state/`.

## Files this workstream may NOT edit

- WS13, WS16, WS17 territory (consumed only).
