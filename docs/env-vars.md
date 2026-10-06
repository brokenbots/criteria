# Environment variables

This document lists the operator-facing environment overrides for the
criteria engine's tunable timing and budget settings (KB-172). The
authoritative source is the registry in
[`internal/tunables`](../internal/tunables): every override below is
described by `tunables.Envvars()`, which owns the env name, the built-in
default, and this generated table (a test drift-checks the block against the
registry, so the table cannot go stale silently).

## Runtime timing and budget overrides

Values are Go durations (`30s`, `5m`, `1500ms`) unless noted otherwise.

<!-- BEGIN AUTO:TUNABLES -->
| Variable | Type | Default | Description |
| --- | --- | --- | --- |
| `CRITERIA_AGENT_HEARTBEAT_INTERVAL` | duration | `10s` | Operator CLI heartbeat cadence against a server-compatible orchestrator while a run is in flight (`criteria agent`, `criteria apply --server`). |
| `CRITERIA_HEARTBEAT_INTERVAL` | duration | `30s` | Adapter log-stream heartbeat cadence (the transitional heartbeatutil helper used by in-tree fixture adapters and the MCP bridge) and the peer Supervise stream idle heartbeat. |
| `CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT` | duration | `1h` | How long file-mode local approval waits for the operator's decision file before the pause fails. Strict override: a malformed value fails the run loudly. |
| `CRITERIA_PEER_BACKOFF_MAX` | duration | `30s` | Peer host reconnect backoff ceiling; must be >= the backoff floor. Peer-owned override: a malformed value aborts peer startup. |
| `CRITERIA_PEER_BACKOFF_MIN` | duration | `1s` | Peer host reconnect backoff floor. Peer-owned override: a malformed value aborts peer startup. |
| `CRITERIA_PEER_JOURNAL_LIMIT` | int | `4096` | Bounded supervision journal capacity in events. Peer-owned override: a non-positive or malformed value aborts peer startup. |
| `CRITERIA_SESSION_HEARTBEAT_STALL` | duration | `90s` | How long a session log stream may idle with no chunks or heartbeats before the adapter is treated as wedged (CRI-271). |
| `CRITERIA_STEP_STALL_WINDOW` | duration | `30m` | How long an unbounded step may run with no observable adapter activity before the engine tears it down (KB-25). A zero or negative value disables stall detection. |
| `CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW` | duration | `10s` | Window, opened by an engine-initiated step teardown, during which transport closes are reclassified as teardown consequences instead of session crashes (CRI-287). |
<!-- END AUTO:TUNABLES -->

## Override postures

Host knobs — everything the engine host applies itself — are **lenient**: an
unset, malformed, or non-positive value keeps the built-in default, so you
cannot accidentally disable a safety window with a typo. The exceptions are
explicit and documented:

- `CRITERIA_STEP_STALL_WINDOW`: a zero or negative value intentionally
  disables the KB-25 stall watchdog.
- `CRITERIA_PEER_BACKOFF_MIN`, `CRITERIA_PEER_BACKOFF_MAX`,
  `CRITERIA_PEER_JOURNAL_LIMIT` are applied **strictly** by the peer
  supervisor (`internal/peer`): a malformed or non-positive value aborts
  peer startup instead of silently defaulting, so a bad manifest fails fast
  in k8s rather than wedging.

## Deliberately unexposed timing knobs

Not every timing constant is an operator setting. The following stay as
code-owned constants (single source, no env override) because they are
either internal plumbing or transport-grade invariants:

- Derived budgets that stem from registered knobs:
  [respawn log-stream drain](../internal/tunables) (a third of the
  CRI-271 stall threshold, clamped to `[5s, 30s]`), the KB-25 watchdog
  poll interval (a quarter of the stall window, clamped to `[10ms, 1s]`),
  and the CRI-169 pause settle grace (a tenth of the pause drain window,
  clamped to `[100ms, 2s]`).
- The log merge buffer's accumulation delay (`500ms`,
  `tunables.DefaultLogMergeDelay`), the local resume status-file poll
  interval (`2s`, `tunables.DefaultFilePollingInterval`), the
  pause-tool-call drain window (`60s`,
  `tunables.DefaultPauseToolCallDrainWindow`), and the gRPC keepalive
  policy in `internal/adapterhost` (keepalive.go).
- Peer dial/teardown budgets (`internal/peer/serve.go` and `child.go`):
  dial timeout, TCP keep-alive period, shutdown budgets, child exit poll
  interval, session-close and control grace windows.
- Deprecated runner-era transport timing
  (`cmd/criteria-adapter-remote-runner`) and non-timing posture knobs such
  as `CRITERIA_PEER_CHILD_KEEPALIVE` (documented by the peer supervisor
  docs).

Registering a new override: add the knob to `internal/tunables` (a
`Default*` constant, an `Env*` name, and one `Envvars()` row) and regenerate
the generated block in this file — the drift test in `internal/tunables`
will point the way if the table goes stale.