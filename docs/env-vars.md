# Environment variables

This document lists every operator-facing `CRITERIA_*` environment variable
the criteria engine reads. The authoritative source is the registry in
[`internal/envreg`](../internal/envreg): every variable below is declared
there with a name, kind, default, consumer surface, and doc line, and raw
`os.Getenv`/`os.LookupEnv` of CRITERIA names outside that package is
forbidden (a repo-wide test enforces the single audit point at
`envreg.Lookup`). The generated table cannot go stale silently — a test
drift-checks the block against the registry; regenerate with
`CRITERIA_REGEN_DOCS=1 go test ./internal/envreg -run TestRenderDocTable`.

## Runtime timing and budget overrides

Values are Go durations (`30s`, `5m`, `1500ms`) unless noted otherwise.

<!-- BEGIN AUTO:ENVREG -->
| Variable | Kind | Default | Consumer | Description |
|---|---|---|---|---|
| `CRITERIA_ADAPTERS` | string | (unset) | internal/dirs, internal/adapterhost discovery | The adapter install root scanned for adapter binaries (default: $CRITERIA_HOME/adapters; $CRITERIA_ADAPTERS wins). |
| `CRITERIA_ADAPTER_BINARY` | string | (unset) | internal/peer (legacy passthrough) | The legacy single-adapter manifest: path to the adapter binary. |
| `CRITERIA_ADAPTER_MANIFEST` | string | (unset) | internal/peer (legacy passthrough) | The legacy single-adapter manifest: path to the adapter manifest. |
| `CRITERIA_ADAPTER_NAME` | string | (unset) | internal/peer (legacy passthrough) | The legacy single-adapter manifest: adapter name, passed through to child adapters and honored as the remote adapter name. |
| `CRITERIA_ADAPTER_VERSION` | string | (unset) | internal/peer (legacy passthrough) | The legacy single-adapter manifest: adapter version. |
| `CRITERIA_AGENT_HEARTBEAT_INTERVAL` | duration | 10s | internal/tunables (lenient) | The operator CLI heartbeat cadence against a server-compatible orchestrator while a run is in flight (`criteria agent`, `criteria apply --server`). |
| `CRITERIA_AGENT_VERSION` | string | (unset) | internal/cli agent | The agent version reported during registration (defaults to the binary version). |
| `CRITERIA_ALLOW_UNSIGNED` | bool | (unset) | internal/cli verification | When truthy (1/true/yes/on), skips adapter signature verification, overriding the --allow-unsigned=false default posture. |
| `CRITERIA_AMBIENT_HOME_INSTANCE` | string | (unset) | internal/adapterhost ambient home | The instance discriminator that gives an ambient-home adapter host its own home snapshot instead of sharing the process-wide one. |
| `CRITERIA_APPROVAL` | enum | (unset) | internal/cli localresume (CRITERIA_LOCAL_APPROVAL=env) | Selects how `criteria apply` resolves approval and signal-wait pauses non-interactively: stdin, file, env, or auto-approve. |
| `CRITERIA_BOOT_GATE_PROBE_TIMEOUT` | duration | 5s | internal/bootgate | Bounds the boot gate's operator-view request at every server-mode start: the process checks its CriteriaRun is still live before registering with castle. |
| `CRITERIA_CASTLE_ADDR` | string | (unset) | internal/cli apply/serveui | The castle control plane base URL the run-viewer's console proxies Login/Logout/ListSessions to (castle is the single source of truth for console login). |
| `CRITERIA_CONTROL_ADDR` | string | (unset) | internal/cli apply | The listen address (host:port) for the local run's control/run-state listener; empty selects a random loopback port. |
| `CRITERIA_FILE_FUNC_MAX_BYTES` | size | 1MiB | workflow file function | The byte budget for the file() eval function; clamped to [1KiB, 64MiB], a malformed value keeps the default. |
| `CRITERIA_HEARTBEAT_INTERVAL` | duration | 30s | internal/tunables (lenient) | The adapter log-stream heartbeat cadence (the transitional heartbeatutil helper used by in-tree fixture adapters and the MCP bridge) and the peer Supervise stream idle heartbeat. |
| `CRITERIA_HOME` | string | (unset) | internal/dirs | The criteria home directory for state, locks, and the OCI adapter cache (default follows XDG state/cache/home conventions). |
| `CRITERIA_LOCAL_APPROVAL` | enum | (unset) | internal/cli apply | Selects how `criteria apply` resolves approval and signal-wait pauses non-interactively: stdin, file, env, or auto-approve. |
| `CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT` | duration | 1h | internal/cli (strict) | How long file-mode local approval waits for the operator's decision file before the pause fails. Strict override: a malformed value fails the run loudly. |
| `CRITERIA_LOG_LEVEL` | enum | (unset) | internal/adapterhost loader | The hclog level for adapter host logs: TRACE, DEBUG, INFO, WARN, or ERROR (WARN by default when unset). |
| `CRITERIA_NAME` | string | (unset) | internal/cli apply/agent/run | The agent name used for server-mode registration (defaults to the hostname). |
| `CRITERIA_OPERATOR_VIEW_URL` | string | (unset) | internal/bootgate | The operator-view URL the orchestrator injects so a server-mode run can verify its CriteriaRun is still active; set together with CRITERIARunTicket and CRITERIARunJob, whose absence disables the boot gate. |
| `CRITERIA_OUTPUT` | enum | auto | internal/cli apply/watch/submit | Selects the output format: auto (concise on a TTY, json when piped), concise, or json. |
| `CRITERIA_OVERRIDE_VERSION` | string | (unset) | workflow/version | Overrides the embedded version, only when the baked-in version string is unparseable; a malformed override still fails version reporting. |
| `CRITERIA_PEER_BACKOFF_MAX` | duration | 30s | internal/peer | The peer host reconnect backoff ceiling; must be >= the backoff floor. Peer-owned strict override: a malformed value aborts peer startup. |
| `CRITERIA_PEER_BACKOFF_MIN` | duration | 1s | internal/peer | The peer host reconnect backoff floor. Peer-owned strict override: a malformed value aborts peer startup. |
| `CRITERIA_PEER_CHILD_KEEPALIVE` | bool | true | internal/peer | When falsy, disables child-process keep-alive so a crashed adapter is not restarted. |
| `CRITERIA_PEER_JOURNAL_LIMIT` | int | 4096 | internal/peer | The bounded supervision journal capacity in events. Peer-owned strict override: a non-positive or malformed value aborts peer startup. |
| `CRITERIA_REMOTE_ADAPTERS` | string | (unset) | internal/peer | The multi-adapter manifest: comma-separated adapters the parent hosts and serves over one child. |
| `CRITERIA_REMOTE_ADAPTERS_DIR` | string | (unset) | internal/peer | The directory holding the multi-adapter manifests when CRITERIARemoteAdapters is not set. |
| `CRITERIA_REMOTE_CA` | string | (unset) | internal/peer | The CA bundle PEM verifying the phone-home endpoint (all three TLS paths must be set together). |
| `CRITERIA_REMOTE_DIGEST` | string | (unset) | internal/peer | The expected sha256 digest of the child adapter binary on the phone-home peer connection. |
| `CRITERIA_REMOTE_HOST` | string | (unset) | internal/peer | The remote host:port of the phone-home peer connection. |
| `CRITERIA_REMOTE_SCOPE` | string | (unset) | internal/peer | The scope the accept token is valid for on the phone-home peer connection. |
| `CRITERIA_REMOTE_SCOPES_DIR` | string | (unset) | internal/peer | The directory of per-scope accept-token files resolved for multi-adapter hosting. |
| `CRITERIA_REMOTE_TLS_CERT` | string | (unset) | internal/peer | The client certificate PEM for phone-home mTLS (all three TLS paths must be set together). |
| `CRITERIA_REMOTE_TLS_KEY` | string | (unset) | internal/peer | The client key PEM for phone-home mTLS (all three TLS paths must be set together). |
| `CRITERIA_REMOTE_TOKEN` | string | (unset) | internal/peer | The accept token presented on the phone-home peer connection. |
| `CRITERIA_RUN_JOB` | string | (unset) | internal/bootgate | The job name the orchestrator injects alongside CRITERIARunTicket for boot-gate liveness checks. |
| `CRITERIA_RUN_TICKET` | string | (unset) | internal/bootgate | The CriteriaRun ticket the orchestrator injects for boot-gate liveness checks; the gate is disabled when this, CRITERIAOperatorViewURL, and CRITERIARunJob are all unset. |
| `CRITERIA_SANDBOX_CONFIG_PATH` | string | (unset) | internal/adapter/environment/sandbox (engine-set) | The path of the sandbox config handed to the sandbox shim child process; set by the engine and scrubbed from adapter environments. |
| `CRITERIA_SERVER_BOOTSTRAP_TOKEN` | string | (unset) | internal/cli apply | The token sent as the X-Server-Bootstrap header on Register in server mode; "file:<path>" reads the token from a mounted secret file. |
| `CRITERIA_SERVER_CODEC` | enum | proto | internal/cli server flags | Selects the Connect codec for server traffic: proto (default) or json. |
| `CRITERIA_SERVER_TLS` | enum | (unset) | internal/cli server flags | Selects the TLS mode: disable (default for http://), tls (default for https://), or mtls. |
| `CRITERIA_SERVER_URL` | string | (unset) | internal/cli server flags | The server base URL for server-mode commands; `criteria run` and `criteria status` default their --server flag to http://localhost:8080, apply/agent leave it empty (local mode). |
| `CRITERIA_SERVE_ADAPTER_CONCURRENCY` | int | 8 | internal/tunables (lenient) | Shared container budget for the workflow.s own step-adapter sessions while `criteria serve-adapter` serves a workflow. |
| `CRITERIA_SESSION_CRASH_REOPEN` | enum | (unset) | internal/engine node_step | Set to 0, false, or off to disable re-opening interrupted sessions after a process crash (re-open is enabled by default). |
| `CRITERIA_SESSION_HEARTBEAT_STALL` | duration | 90s | internal/tunables (lenient) | How long a session log stream may idle with no chunks or heartbeats before the adapter is treated as wedged. |
| `CRITERIA_SIGNAL` | enum | (unset) | internal/cli localresume (CRITERIA_LOCAL_APPROVAL=env) | Selects how `criteria apply` resolves approval and signal-wait pauses non-interactively: stdin, file, env, or auto-approve. |
| `CRITERIA_STATE_DIR` | string | (unset) | internal/dirs | The deprecated pre-0.5 alias for the state directory, superseded by the CRITERIA_HOME layout; still honored after CRITERIA_HOME for backward compatibility. |
| `CRITERIA_STEP_STALL_WINDOW` | duration | 30m | internal/tunables (lenient) | How long an unbounded step may run with no observable adapter activity before the engine tears it down. A zero or negative value disables stall detection. |
| `CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW` | duration | 10s | internal/tunables (lenient) | The window, opened by an engine-initiated step teardown, during which transport closes are reclassified as teardown consequences instead of session crashes. |
| `CRITERIA_TLS_CA` | string | (unset) | internal/cli server flags | The CA bundle PEM used to verify the server's TLS certificate. |
| `CRITERIA_TLS_CERT` | string | (unset) | internal/cli server flags | The client certificate PEM for mTLS. |
| `CRITERIA_TLS_KEY` | string | (unset) | internal/cli server flags | The client key PEM for mTLS. |
| `CRITERIA_VAULT_MOUNT` | string | (unset) | internal/adapter secrets (vault provider) | The vault mount path that overrides the manifest-declared mount for the vault secret provider. |
| `CRITERIA_VERIFY_FAILOVER_BUDGET` | duration | 1m | internal/tunables (lenient) | How long phase-B adapter verify re-waits for a fresh line after a transport-class loss before failing the adapter init with the typed budget reason. |
| `CRITERIA_WORKFLOW` | string | (unset) | internal/cli run | The path to the workflow .chcl or .hcl file for `criteria run`. |
| `CRITERIA_WORKFLOW_ALLOWED_PATHS` | string | (unset) | workflow file function | The OS path-list-separated allowlist of directories the file() eval function may read from (empty and unset both mean no access). |
<!-- END AUTO:ENVREG -->

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
- Runner-era transport timing that went away with the removed runner
  (`cmd/criteria-adapter-remote-runner`, PR #551).

## Registering a new variable

Add the knob to `internal/envreg` (`internal/envreg/registry.go`: a named
`Var` row with kind, default where one exists, consumer surface, and a
one-line doc) and read it through its typed accessor or `envreg.Lookup`.
`os.Getenv`/`os.LookupEnv` of a CRITERIA name anywhere else fails the
registry sweep test. Then regenerate the generated block in this file with
`CRITERIA_REGEN_DOCS=1 go test ./internal/envreg -run TestRenderDocTable` —
the drift test in `internal/envreg` will point the way if the table goes
stale.