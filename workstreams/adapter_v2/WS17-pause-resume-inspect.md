# WS17 — `Pause`, `Resume`, `Inspect` lifecycle RPCs

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS03](WS03-host-v2-wire.md), [WS16](WS16-bidi-permission-stream.md). · **Unblocks:** [WS18](WS18-snapshot-restore.md).

## Context

`README.md` D25–D26. Three new lifecycle ops:

- `Pause(session)` — adapter halts work without losing state.
- `Resume(session)` — adapter continues from where it paused.
- `Inspect(session)` → structured state, read-only.

Combined with the bidi permission stream's freeze (WS16), these let operators pause a long-running agent workflow, inspect what it's doing, and resume.

## Prerequisites

WS02, WS03, WS16 merged.

## In scope

### Step 1 — Host-side wiring

In `internal/adapter/sessions.go`:

```go
func (s *Session) Pause(ctx context.Context) error {
    _, err := s.client.Pause(ctx, &v2.PauseRequest{SessionID: s.id})
    if err != nil { return err }
    s.permissions.Pause()
    return nil
}

func (s *Session) Resume(ctx context.Context) error {
    s.permissions.Resume(ctx)
    _, err := s.client.Resume(ctx, &v2.ResumeRequest{SessionID: s.id})
    return err
}

func (s *Session) Inspect(ctx context.Context) (*v2.InspectResponse, error) {
    return s.client.Inspect(ctx, &v2.InspectRequest{SessionID: s.id})
}
```

### Step 2 — Engine integration

`internal/engine/`: add a top-level mechanism to pause/resume an entire workflow, which iterates over open sessions and calls Pause/Resume on each. Engine pause is reentrant and idempotent.

### Step 3 — CLI verbs

`internal/cli/`:

- `criteria pause <run-id>` — pauses an active run.
- `criteria resume <run-id>` — resumes a paused run.
- `criteria inspect <run-id> [--session <id>]` — pretty-prints `InspectResponse`.

(These are workflow-level commands, not under `adapter`, since they affect the whole run.)

### Step 4 — Inspect output rendering

A small renderer that turns `InspectResponse.state_json` + structured fields into a human-readable view:

```
session abc123 (claude.assistant)
  current_step:           generate_outline
  pending_permissions:    2
  last_activity:          2026-05-12T14:32:11Z (3s ago)
  state summary:
    turns_taken: 4
    tools_invoked: ["read_file", "edit_file"]
    last_user_message: "Now make it more concise" [REDACTED if tainted]
```

The `state_json` is opaque to the host — the renderer pretty-prints any well-formed JSON; adapters can shape it however they like.

### Step 5 — Tests

- Pause-resume round trip on a test adapter that increments a counter every 100ms; verify counter stalls during pause.
- Inspect during normal execution returns sensible fields.
- Concurrent Pause/Resume calls are idempotent.

## Out of scope

- `Snapshot`/`Restore` — WS18.
- Permission stream behavior under pause — already in WS16.

## Behavior change

**Yes** — new CLI verbs, new RPC capabilities.

## Tests required

- Pause/resume tests on a synthetic adapter.
- CLI verb tests.

## Exit criteria

- `criteria pause/resume/inspect` works end-to-end.

## Files this workstream may modify

- `internal/adapter/sessions.go`.
- `internal/engine/` (engine-level pause/resume).
- `internal/cli/pause.go`, `resume.go`, `inspect.go` *(new)*.

## Files this workstream may NOT edit

- WS16 / WS18 territory.
