package envreg

// Every production read of a CRITERIA_* variable must go through one of
// these rows (typed accessor or Lookup); the package's repo-wide sweep
// test enforces it. Rows are grouped by consumer surface.
var registry = []*Var{
	// --- internal/dirs: storage root resolution.
	&CRITERIAHome, &CRITERIAStateDir, &CRITERIAAdapters,

	// --- internal/cli: server connection flags shared by apply/agent/run/status/watch.
	&CRITERIAServerURL, &CRITERIAName, &CRITERIAAgentVersion, &CRITERIAServerCodec,
	&CRITERIAServerTLS, &CRITERIATLSCA, &CRITERIATLSCert, &CRITERIATLSKey,
	&CRITERIAServerBootstrapToken, &CRITERIAWorkflow,

	// --- internal/cli: local run presentation and control listeners.
	&CRITERIAOutput, &CRITERIAControlAddr, &CRITERIACastleAddr,

	// --- internal/cli: local approval pauses and adapter verification posture.
	&CRITERIAAllowUnsigned, &CRITERIALocalApproval, &CRITERIAApproval, &CRITERIASignal,

	// --- internal/adapterhost: adapter hosting, discovery, logging, boot gate.
	&CRITERIALogLevel, &CRITERIAAmbientHomeInstance, &CRITERIABootGateProbeTimeout,
	&CRITERIAOperatorViewURL, &CRITERIARunTicket, &CRITERIARunJob,

	// --- internal/engine: session crash posture.
	&CRITERIASessionCrashReopen,

	// --- workflow: evaluation-time file and path policy, and the version override.
	&CRITERIAFileFuncMaxBytes, &CRITERIAWorkflowAllowedPaths, &CRITERIAOverrideVersion,

	// --- internal/peer: phone-home remote connection (orchestrator-shared names).
	&CRITERIARemoteHost, &CRITERIARemoteToken, &CRITERIARemoteScope, &CRITERIARemoteDigest,
	&CRITERIARemoteTLSCert, &CRITERIARemoteTLSKey, &CRITERIARemoteCA,
	&CRITERIARemoteAdapters, &CRITERIARemoteAdaptersDir, &CRITERIARemoteScopesDir,
	&CRITERIAPeerChildKeepalive, &CRITERIAPeerBackoffMin, &CRITERIAPeerBackoffMax,
	&CRITERIAPeerJournalLimit,

	// --- internal/peer: legacy per-adapter manifest variables (also read by adapters).
	&CRITERIAAdapterName, &CRITERIAAdapterVersion, &CRITERIAAdapterBinary, &CRITERIAAdapterManifest,

	// --- internal/adapter: vault-backed secrets, sandbox shim, go-plugin protocol.
	&CRITERIAVaultMount, &CRITERIASandboxConfigPath, &CRITERIAPlugin,

	// --- internal/tunables-consumed timing and budget knobs. Lenient rows are
	// resolved via tunables.Settings; strict rows are parsed by their owning
	// packages (peer backoff/journal, local approval timeout).
	&CRITERIAHeartbeatInterval, &CRITERIASessionHeartbeatStall, &CRITERIAStepTimeoutTeardownWindow,
	&CRITERIAStepStallWindow, &CRITERIAAgentHeartbeatInterval, &CRITERIAServeAdapterConcurrency,
	&CRITERIAVerifyFailoverBudget, &CRITERIALocalApprovalFileTimeout,

	// --- Hidden prefix families: names an operator-facing row cannot enumerate.
	&CRITERIARemoteFamily, &CRITERIAAdapterFamily,

	// --- Hidden test-harness gates (registered so reads stay audited).
	&CRITERIARemoteE2E, &CRITERIAPeerE2E, &CRITERIAContainerTests,
	&CRITERIAConformanceAdapter, &CRITERIAConformanceAdapterName, &CRITERIARemoteStepStartedFile,
}

// CRITERIA_HOME / CRITERIA_STATE_DIR / CRITERIA_ADAPTERS.
var (
	CRITERIAHome     = Var{name: "CRITERIA_HOME", kind: KindString, scope: "internal/dirs", doc: "The criteria home directory for state, locks, and the OCI adapter cache (default follows XDG state/cache/home conventions)."}
	CRITERIAStateDir = Var{name: "CRITERIA_STATE_DIR", kind: KindString, scope: "internal/dirs", doc: "The deprecated pre-0.5 alias for the state directory, superseded by the CRITERIA_HOME layout; still honored after CRITERIA_HOME for backward compatibility."}
	CRITERIAAdapters = Var{name: "CRITERIA_ADAPTERS", kind: KindString, scope: "internal/dirs, internal/adapterhost discovery", doc: "The adapter install root scanned for adapter binaries (default: $CRITERIA_HOME/adapters; $CRITERIA_ADAPTERS wins)."}
)

// Server-connection flags shared by apply/agent/run/status/watch.
var (
	CRITERIAServerURL            = Var{name: "CRITERIA_SERVER_URL", kind: KindString, scope: "internal/cli server flags", doc: "The server base URL for server-mode commands; `criteria run` and `criteria status` default their --server flag to http://localhost:8080, apply/agent leave it empty (local mode)."}
	CRITERIAName                 = Var{name: "CRITERIA_NAME", kind: KindString, scope: "internal/cli apply/agent/run", doc: "The agent name used for server-mode registration (defaults to the hostname)."}
	CRITERIAAgentVersion         = Var{name: "CRITERIA_AGENT_VERSION", kind: KindString, scope: "internal/cli agent", doc: "The agent version reported during registration (defaults to the binary version)."}
	CRITERIAServerCodec          = Var{name: "CRITERIA_SERVER_CODEC", kind: KindEnum, def: "proto", scope: "internal/cli server flags", doc: "Selects the Connect codec for server traffic: proto (default) or json."}
	CRITERIAServerTLS            = Var{name: "CRITERIA_SERVER_TLS", kind: KindEnum, scope: "internal/cli server flags", doc: "Selects the TLS mode: disable (default for http://), tls (default for https://), or mtls."}
	CRITERIATLSCA                = Var{name: "CRITERIA_TLS_CA", kind: KindString, scope: "internal/cli server flags", doc: "The CA bundle PEM used to verify the server's TLS certificate."}
	CRITERIATLSCert              = Var{name: "CRITERIA_TLS_CERT", kind: KindString, scope: "internal/cli server flags", doc: "The client certificate PEM for mTLS."}
	CRITERIATLSKey               = Var{name: "CRITERIA_TLS_KEY", kind: KindString, scope: "internal/cli server flags", doc: "The client key PEM for mTLS."}
	CRITERIAServerBootstrapToken = Var{name: "CRITERIA_SERVER_BOOTSTRAP_TOKEN", kind: KindString, scope: "internal/cli apply", doc: "The token sent as the X-Server-Bootstrap header on Register in server mode; \"file:<path>\" reads the token from a mounted secret file."}
	CRITERIAWorkflow             = Var{name: "CRITERIA_WORKFLOW", kind: KindString, scope: "internal/cli run", doc: "The path to the workflow .chcl or .hcl file for `criteria run`."}
)

// Local run presentation and control listeners.
var (
	CRITERIAOutput      = Var{name: "CRITERIA_OUTPUT", kind: KindEnum, def: "auto", scope: "internal/cli apply/watch/submit", doc: "Selects the output format: auto (concise on a TTY, json when piped), concise, or json."}
	CRITERIAControlAddr = Var{name: "CRITERIA_CONTROL_ADDR", kind: KindString, scope: "internal/cli apply", doc: "The listen address (host:port) for the local run's control/run-state listener; empty selects a random loopback port."}
	CRITERIACastleAddr  = Var{name: "CRITERIA_CASTLE_ADDR", kind: KindString, scope: "internal/cli apply/serveui", doc: "The castle control plane base URL the run-viewer's console proxies Login/Logout/ListSessions to (castle is the single source of truth for console login)."}
)

// Local approval pauses and adapter verification posture.
var (
	CRITERIAAllowUnsigned = Var{name: "CRITERIA_ALLOW_UNSIGNED", kind: KindBool, scope: "internal/cli verification", doc: "When truthy (1/true/yes/on), skips adapter signature verification, overriding the --allow-unsigned=false default posture."}
	CRITERIALocalApproval = Var{name: "CRITERIA_LOCAL_APPROVAL", kind: KindEnum, scope: "internal/cli apply", doc: "Selects how `criteria apply` resolves approval and signal-wait pauses non-interactively: stdin, file, env, or auto-approve."}
	CRITERIAApproval      = Var{name: "CRITERIA_APPROVAL", kind: KindEnum, prefix: true, scope: "internal/cli localresume (CRITERIA_LOCAL_APPROVAL=env)", doc: "Per-node approval decision family: CRITERIA_APPROVAL_<NODE> resolves a paused approval node to approved or rejected without prompting, with NODE uppercased and dots/hyphens as underscores."}
	CRITERIASignal        = Var{name: "CRITERIA_SIGNAL", kind: KindEnum, prefix: true, scope: "internal/cli localresume (CRITERIA_LOCAL_APPROVAL=env)", doc: "Per-node wait outcome family: CRITERIA_SIGNAL_<NODE> resolves a paused signal-wait node to the outcome it resumes with, with NODE uppercased and dots/hyphens as underscores."}
)

// Adapter hosting, discovery, logging, and the boot gate.
var (
	CRITERIALogLevel             = Var{name: "CRITERIA_LOG_LEVEL", kind: KindEnum, scope: "internal/adapterhost loader, internal/cli peer and serve-adapter", doc: "The hclog level for adapter host logs: TRACE, DEBUG, INFO, WARN, or ERROR (WARN by default when unset); also selects the slog level for criteria peer and criteria serve-adapter (INFO when unset)."}
	CRITERIAAmbientHomeInstance  = Var{name: "CRITERIA_AMBIENT_HOME_INSTANCE", kind: KindString, scope: "internal/adapterhost ambient home", doc: "The instance discriminator that gives an ambient-home adapter host its own home snapshot instead of sharing the process-wide one."}
	CRITERIABootGateProbeTimeout = Var{name: "CRITERIA_BOOT_GATE_PROBE_TIMEOUT", kind: KindDuration, def: "5s", scope: "internal/bootgate, internal/tunables", doc: "Bounds the boot gate's operator-view request at every server-mode start: the process checks its CriteriaRun is still live before registering with castle."}
	CRITERIAOperatorViewURL      = Var{name: "CRITERIA_OPERATOR_VIEW_URL", kind: KindString, scope: "internal/bootgate", doc: "The operator-view URL the orchestrator injects so a server-mode run can verify its CriteriaRun is still active; set together with CRITERIA_RUN_TICKET and CRITERIA_RUN_JOB, whose absence disables the boot gate."}
	CRITERIARunTicket            = Var{name: "CRITERIA_RUN_TICKET", kind: KindString, scope: "internal/bootgate", doc: "The CriteriaRun ticket the orchestrator injects for boot-gate liveness checks; the gate is disabled when this, CRITERIA_OPERATOR_VIEW_URL, and CRITERIA_RUN_JOB are all unset."}
	CRITERIARunJob               = Var{name: "CRITERIA_RUN_JOB", kind: KindString, scope: "internal/bootgate", doc: "The job name the orchestrator injects alongside CRITERIA_RUN_TICKET for boot-gate liveness checks."}
)

var CRITERIASessionCrashReopen = Var{name: "CRITERIA_SESSION_CRASH_REOPEN", kind: KindEnum, scope: "internal/engine node_step", doc: "Set to 0, false, or off to disable re-opening interrupted sessions after a process crash (re-open is enabled by default)."}

// Evaluation-time file and path policy, and the version override.
var (
	CRITERIAFileFuncMaxBytes     = Var{name: "CRITERIA_FILE_FUNC_MAX_BYTES", kind: KindSize, def: "1MiB", scope: "workflow file function", doc: "The byte budget for the file() eval function; clamped to [1KiB, 64MiB], a malformed value keeps the default."}
	CRITERIAWorkflowAllowedPaths = Var{name: "CRITERIA_WORKFLOW_ALLOWED_PATHS", kind: KindString, scope: "workflow file function", doc: "The OS path-list-separated allowlist of directories the file() eval function may read from (empty and unset both mean no access)."}
	CRITERIAOverrideVersion      = Var{name: "CRITERIA_OVERRIDE_VERSION", kind: KindString, scope: "workflow/version", doc: "Overrides the embedded version, only when the baked-in version string is unparseable; a malformed override still fails version reporting."}
)

// Phone-home remote connection (orchestrator-shared names).
var (
	CRITERIARemoteHost         = Var{name: "CRITERIA_REMOTE_HOST", kind: KindString, scope: "internal/peer", doc: "The remote host:port of the phone-home peer connection."}
	CRITERIARemoteToken        = Var{name: "CRITERIA_REMOTE_TOKEN", kind: KindString, scope: "internal/peer", doc: "The accept token presented on the phone-home peer connection."}
	CRITERIARemoteScope        = Var{name: "CRITERIA_REMOTE_SCOPE", kind: KindString, scope: "internal/peer", doc: "The scope the accept token is valid for on the phone-home peer connection."}
	CRITERIARemoteDigest       = Var{name: "CRITERIA_REMOTE_DIGEST", kind: KindString, scope: "internal/peer", doc: "The expected sha256 digest of the child adapter binary on the phone-home peer connection."}
	CRITERIARemoteTLSCert      = Var{name: "CRITERIA_REMOTE_TLS_CERT", kind: KindString, scope: "internal/peer", doc: "The client certificate PEM for phone-home mTLS (all three TLS paths must be set together)."}
	CRITERIARemoteTLSKey       = Var{name: "CRITERIA_REMOTE_TLS_KEY", kind: KindString, scope: "internal/peer", doc: "The client key PEM for phone-home mTLS (all three TLS paths must be set together)."}
	CRITERIARemoteCA           = Var{name: "CRITERIA_REMOTE_CA", kind: KindString, scope: "internal/peer", doc: "The CA bundle PEM verifying the phone-home endpoint (all three TLS paths must be set together)."}
	CRITERIARemoteAdapters     = Var{name: "CRITERIA_REMOTE_ADAPTERS", kind: KindString, scope: "internal/peer", doc: "The multi-adapter manifest: comma-separated adapters the parent hosts and serves over one child."}
	CRITERIARemoteAdaptersDir  = Var{name: "CRITERIA_REMOTE_ADAPTERS_DIR", kind: KindString, scope: "internal/peer", doc: "The directory holding the multi-adapter manifests when CRITERIA_REMOTE_ADAPTERS is not set."}
	CRITERIARemoteScopesDir    = Var{name: "CRITERIA_REMOTE_SCOPES_DIR", kind: KindString, scope: "internal/peer", doc: "The directory of per-scope accept-token files resolved for multi-adapter hosting."}
	CRITERIAPeerChildKeepalive = Var{name: "CRITERIA_PEER_CHILD_KEEPALIVE", kind: KindBool, def: "true", scope: "internal/peer", doc: "When falsy, disables child-process keep-alive so a crashed adapter is not restarted."}
	CRITERIAPeerBackoffMin     = Var{name: "CRITERIA_PEER_BACKOFF_MIN", kind: KindDuration, def: "1s", scope: "internal/peer", doc: "The peer host reconnect backoff floor. Peer-owned strict override: a malformed value aborts peer startup."}
	CRITERIAPeerBackoffMax     = Var{name: "CRITERIA_PEER_BACKOFF_MAX", kind: KindDuration, def: "30s", scope: "internal/peer", doc: "The peer host reconnect backoff ceiling; must be >= the backoff floor. Peer-owned strict override: a malformed value aborts peer startup."}
	CRITERIAPeerJournalLimit   = Var{name: "CRITERIA_PEER_JOURNAL_LIMIT", kind: KindInt, def: "4096", scope: "internal/peer", doc: "The bounded supervision journal capacity in events. Peer-owned strict override: a non-positive or malformed value aborts peer startup."}
)

// Legacy per-adapter manifest variables (also read by adapters).
var (
	CRITERIAAdapterName     = Var{name: "CRITERIA_ADAPTER_NAME", kind: KindString, scope: "internal/peer (legacy passthrough)", doc: "The legacy single-adapter manifest: adapter name, passed through to child adapters and honored as the remote adapter name."}
	CRITERIAAdapterVersion  = Var{name: "CRITERIA_ADAPTER_VERSION", kind: KindString, scope: "internal/peer (legacy passthrough)", doc: "The legacy single-adapter manifest: adapter version."}
	CRITERIAAdapterBinary   = Var{name: "CRITERIA_ADAPTER_BINARY", kind: KindString, scope: "internal/peer (legacy passthrough)", doc: "The legacy single-adapter manifest: path to the adapter binary."}
	CRITERIAAdapterManifest = Var{name: "CRITERIA_ADAPTER_MANIFEST", kind: KindString, scope: "internal/peer (legacy passthrough)", doc: "The legacy single-adapter manifest: path to the adapter manifest."}
)

// Secrets, sandbox shim, and go-plugin protocol scrubbing.
var (
	CRITERIAVaultMount        = Var{name: "CRITERIA_VAULT_MOUNT", kind: KindString, scope: "internal/adapter secrets (vault provider)", doc: "The vault mount path that overrides the manifest-declared mount for the vault secret provider."}
	CRITERIASandboxConfigPath = Var{name: "CRITERIA_SANDBOX_CONFIG_PATH", kind: KindString, scope: "internal/adapter/environment/sandbox (engine-set)", doc: "The path of the sandbox config handed to the sandbox shim child process; set by the engine and scrubbed from adapter environments."}
	CRITERIAPlugin            = Var{name: "CRITERIA_PLUGIN", kind: KindString, hidden: true, scope: "internal/adapterhost handshake (go-plugin protocol)", doc: "The go-plugin magic-cookie handshake name; scrubbed from child environments and matched by the sandbox shim."}
)

// Timing and budget knobs consumed through tunables. Lenient rows are
// resolved via tunables.Settings; strict rows are parsed by their owning
// packages (peer backoff/journal, local approval timeout).
var (
	CRITERIAHeartbeatInterval         = Var{name: "CRITERIA_HEARTBEAT_INTERVAL", kind: KindDuration, def: "30s", scope: "internal/tunables (lenient)", doc: "The adapter log-stream heartbeat cadence (the transitional heartbeatutil helper used by in-tree fixture adapters and the MCP bridge) and the peer Supervise stream idle heartbeat."}
	CRITERIASessionHeartbeatStall     = Var{name: "CRITERIA_SESSION_HEARTBEAT_STALL", kind: KindDuration, def: "90s", scope: "internal/tunables (lenient)", doc: "How long a session log stream may idle with no chunks or heartbeats before the adapter is treated as wedged."}
	CRITERIAStepTimeoutTeardownWindow = Var{name: "CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW", kind: KindDuration, def: "10s", scope: "internal/tunables (lenient)", doc: "The window, opened by an engine-initiated step teardown, during which transport closes are reclassified as teardown consequences instead of session crashes."}
	CRITERIAStepStallWindow           = Var{name: "CRITERIA_STEP_STALL_WINDOW", kind: KindDuration, def: "30m", scope: "internal/tunables (lenient)", doc: "How long an unbounded step may run with no observable adapter activity before the engine tears it down. A zero or negative value disables stall detection."}
	CRITERIAAgentHeartbeatInterval    = Var{name: "CRITERIA_AGENT_HEARTBEAT_INTERVAL", kind: KindDuration, def: "10s", scope: "internal/tunables (lenient)", doc: "The operator CLI heartbeat cadence against a server-compatible orchestrator while a run is in flight (`criteria agent`, `criteria apply --server`)."}
	CRITERIAServeAdapterConcurrency   = Var{name: "CRITERIA_SERVE_ADAPTER_CONCURRENCY", kind: KindInt, def: "8", scope: "internal/tunables (lenient)", doc: "Shared container budget for the workflow's own step-adapter sessions while `criteria serve-adapter` serves a workflow."}
	CRITERIAVerifyFailoverBudget      = Var{name: "CRITERIA_VERIFY_FAILOVER_BUDGET", kind: KindDuration, def: "1m", scope: "internal/tunables (lenient)", doc: "How long phase-B adapter verify re-waits for a fresh line after a transport-class loss before failing the adapter init with the typed budget reason."}
	CRITERIALocalApprovalFileTimeout  = Var{name: "CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT", kind: KindDuration, def: "1h", scope: "internal/cli (strict)", doc: "How long file-mode local approval waits for the operator's decision file before the pause fails. Strict override: a malformed value fails the run loudly."}
)

// Hidden prefix families: names an operator-facing row cannot enumerate.
var (
	CRITERIARemoteFamily  = Var{name: "CRITERIA_REMOTE", kind: KindString, prefix: true, hidden: true, scope: "internal/peer scrubbing", doc: "Phone-home connection family: the bare name and every CRITERIA_REMOTE_* member are scrubbed from child adapter environments (members above are the operator-facing subset the peer reads); the bare CRITERIA_REMOTE name is Env()-addressable from this same row."}
	CRITERIAAdapterFamily = Var{name: "CRITERIA_ADAPTER", kind: KindString, prefix: true, hidden: true, scope: "internal/peer scrubbing", doc: "Multi-adapter family: CRITERIA_ADAPTER_<NAME>_{BINARY,VERSION,DIGEST,MANIFEST} overrides are scrubbed from child environments; the four legacy CRITERIA_ADAPTER_* rows pass through."}
)

// Hidden test-harness gates (registered so reads stay audited).
var (
	CRITERIARemoteE2E              = Var{name: "CRITERIA_REMOTE_E2E", kind: KindBool, hidden: true, scope: "internal/ci/smoke (test gate)", doc: "Set to 1 to enable the remote-adapter end-to-end smoke tests."}
	CRITERIAPeerE2E                = Var{name: "CRITERIA_PEER_E2E", kind: KindBool, hidden: true, scope: "internal/ci/smoke (test gate)", doc: "Set to 1 to enable the peer end-to-end smoke tests."}
	CRITERIAContainerTests         = Var{name: "CRITERIA_CONTAINER_TESTS", kind: KindBool, hidden: true, scope: "internal/adapter/environment/container (test gate)", doc: "Set to 1 to enable container-runner tests that require a container runtime."}
	CRITERIAConformanceAdapter     = Var{name: "CRITERIA_CONFORMANCE_ADAPTER", kind: KindString, hidden: true, scope: "internal/adapter/conformance (test gate)", doc: "Selects an out-of-process adapter binary for the conformance suite instead of the in-memory reference."}
	CRITERIAConformanceAdapterName = Var{name: "CRITERIA_CONFORMANCE_ADAPTER_NAME", kind: KindString, hidden: true, scope: "internal/adapter/conformance (test gate)", doc: "The name recorded for the out-of-process conformance adapter."}
	CRITERIARemoteStepStartedFile  = Var{name: "CRITERIA_REMOTE_STEP_STARTED_FILE", kind: KindString, hidden: true, scope: "internal/ci/smoke fixture (test gate)", doc: "The file the remote-smoke fixture adapter touches when it processes the first step, used as the smoke test's readiness marker."}
)
