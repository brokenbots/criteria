# WS16 — Bidirectional `Permissions` stream + per-session permission state

**Phase:** Adapter v2 · **Track:** Protocol features · **Owner:** Workstream executor · **Depends on:** [WS02](WS02-protocol-v2-proto.md), [WS03](WS03-host-v2-wire.md). · **Unblocks:** [WS18](WS18-snapshot-restore.md) (snapshots carry permission state).

## Context

`README.md` D24. Replace the unary `Permit` callback with a bidi `Permissions` stream. The implementation is a `PermissionState` field on the existing `Session` struct in `internal/adapter/sessions.go` plus a goroutine that runs for the session's lifetime: reads `PermissionEvent`s from the stream, calls the existing policy evaluator (extended for env-block policy), writes `PermissionDecision`s back, appends to the run audit log.

**Not a new service.** Same process, same package, ~150 LOC of new code. The FSM is unchanged — permissions stay below the FSM level; the FSM still transitions only on step outcomes.

## Prerequisites

WS02, WS03 merged.

## In scope

### Step 1 — PermissionState struct

`internal/adapter/permission_state.go`:

```go
type PermissionState struct {
    mu      sync.Mutex
    inflight map[string]requestState  // request_id → state
    decisions []DecisionLogEntry      // recent decisions window for audit replay
    policy   PolicyEvaluator
    audit    AuditWriter
}

type requestState struct {
    request    *v2.PermissionEvent
    receivedAt time.Time
    decision   *v2.PermissionDecision  // nil until decided
    decidedAt  time.Time
}

type DecisionLogEntry struct {
    SessionID    string
    RequestID    string
    Tool         string
    ArgsDigest   string
    Decision     string
    Reason       string
    EvaluatedAt  time.Time
}
```

### Step 2 — Stream consumer goroutine

In `internal/adapter/sessions.go`, on session open, spawn:

```go
func (s *Session) runPermissionStream(ctx context.Context) {
    requestsCh := make(chan *v2.PermissionEvent, 16)
    decisionsCh := make(chan *v2.PermissionDecision, 16)
    go func() {
        defer close(decisionsCh)
        for req := range requestsCh {
            dec := s.permissions.Evaluate(req)
            decisionsCh <- dec
        }
    }()
    if err := s.client.Permissions(ctx, requestsCh, decisionsCh); err != nil {
        s.logger.Warn("permission stream ended", "err", err)
    }
}
```

`Evaluate` runs the existing `allow_tools` glob matcher (currently in `internal/adapter/policy.go`) extended with the WS09 environment-block policy fields (network, filesystem, permissions list).

### Step 3 — Policy evaluator extension

`internal/adapter/policy.go` — current code matches against `allow_tools` patterns. Add:

```go
type PolicyEvaluator interface {
    Evaluate(req *v2.PermissionEvent) *v2.PermissionDecision
}

type CombinedPolicy struct {
    AllowTools   []string                       // existing
    EnvPolicy    workflow.ResolvedPolicy        // from WS09
}

func (p *CombinedPolicy) Evaluate(req *v2.PermissionEvent) *v2.PermissionDecision { … }
```

### Step 4 — Audit log writer

Append per-decision entries to `~/.criteria/runs/<run-id>/audit.log` (existing file). Single goroutine for the writer; entries marshalled as one JSON object per line.

### Step 5 — Snapshot/restore hooks (for WS18)

```go
// MarshalState writes the in-flight queue and a window of recent decisions
// into a proto blob suitable for embedding in the Snapshot output.
func (ps *PermissionState) MarshalState() ([]byte, error)

// RestoreState rehydrates from a blob; previously-answered requests are
// re-answered from the decision log; unanswered are re-presented to policy.
func (ps *PermissionState) RestoreState(data []byte, policy PolicyEvaluator, audit AuditWriter) error
```

### Step 6 — Pause/resume hooks (for WS17)

```go
// Pause cancels the consumer goroutine's context. The stream is held open
// at the adapter side; no new decisions are dispatched.
func (ps *PermissionState) Pause()

// Resume restarts the consumer goroutine.
func (ps *PermissionState) Resume(ctx context.Context)
```

### Step 7 — Tests

- Unit: evaluator combines allow_tools + env policy correctly.
- Concurrency: 100 concurrent permission requests on a single session — verify all answered, audit log has 100 entries.
- Snapshot/restore: marshal → restore → previously-answered queries replay deterministically.
- Pause/resume: queue freezes and thaws.

## Out of scope

- The proto-level definitions — WS02.
- Snapshot/Restore RPC itself — WS18.
- Pause/Resume RPC itself — WS17.

## Behavior change

**Yes** — adapters that issued sequential `Permit` calls now use the bidi stream. The host-side semantics are equivalent for the same input; the win is concurrency and snapshot-friendliness.

## Tests required

- Unit, concurrency, snapshot, pause/resume tests as above.

## Exit criteria

- All permission flow tests pass.
- Audit log contains structured entries.

## Files this workstream may modify

- `internal/adapter/permission_state.go` *(new)*.
- `internal/adapter/sessions.go` — spawn the goroutine, hook MarshalState/RestoreState.
- `internal/adapter/policy.go` — combined policy evaluator.
- Audit log writer in `internal/audit/` or equivalent.

## Files this workstream may NOT edit

- `proto/criteria/v2/` — WS02.
- WS17/WS18 territory (they call the hooks added here).
